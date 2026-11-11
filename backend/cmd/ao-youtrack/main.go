// Command ao-youtrack applies one Orchestrator → YouTrack workflow transition.
//
// It is a thin, auditable bridge: the coordinator supplies the facts it observed
// (session id, branch, worktree, commit), this command validates them against
// the local checkout and the ticket's current fields, writes the evidence
// comment first, then the supported fields, and reads them back.
//
// The credential is read from an environment variable only (YOUTRACK_TOKEN by
// default), so it never appears in process arguments or output.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("stage is required")
	}
	stage := youtrack.Stage(args[0])
	fs := flag.NewFlagSet(string(stage), flag.ContinueOnError)
	var (
		issue           = fs.String("issue", "", "YouTrack issue id, for example CKM-825")
		session         = fs.String("session", "", "Orchestrator coding session id")
		branch          = fs.String("branch", "", "branch under work")
		worktree        = fs.String("worktree", "", "absolute worktree path")
		model           = fs.String("model", "", "provider-qualified coder model")
		commit          = fs.String("commit", "", "full 40-character commit SHA")
		reviewedCommit  = fs.String("reviewed-commit", "", "commit the independent reviewer examined")
		reviewerSession = fs.String("reviewer-session", "", "reviewer session id")
		reviewerModel   = fs.String("reviewer-model", "", "provider-qualified reviewer model")
		reviewerEffort  = fs.String("reviewer-effort", "", "reviewer reasoning effort")
		verdict         = fs.String("verdict", "", "pass or fail")
		evidence        = fs.String("evidence", "", "pre-live evidence summary")
		baseURL         = fs.String("base-url", "", "YouTrack REST base URL")
		tokenEnv        = fs.String("token-env", "YOUTRACK_TOKEN", "environment variable holding the token")
		dryRun          = fs.Bool("dry-run", false, "validate and print the transition without writing")
		skipWorktree    = fs.Bool("skip-worktree-check", false, "skip local git verification (read-only tickets only)")
		aoBin           = fs.String("ao-bin", "ao", "Orchestrator CLI used to read session facts")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *issue == "" {
		return errors.New("--issue is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := youtrack.NewClient(*baseURL, os.Getenv(*tokenEnv))
	current, err := client.GetIssue(ctx, *issue)
	if err != nil {
		return err
	}

	facts := youtrack.Facts{
		IssueID:           *issue,
		SessionID:         *session,
		Branch:            *branch,
		WorktreePath:      *worktree,
		CoderModel:        *model,
		Commit:            *commit,
		ReviewedCommit:    *reviewedCommit,
		ReviewerSession:   *reviewerSession,
		ReviewerModel:     *reviewerModel,
		ReviewerEffort:    *reviewerEffort,
		Verdict:           *verdict,
		PreliveEvidence:   *evidence,
		RecordedSessionID: current.Fields[youtrack.FieldCodingSession].Value,
	}
	if sessions, err := listSessions(ctx, *aoBin); err != nil {
		logf("sessions_unavailable", err.Error())
	} else {
		facts.Sessions = sessions
	}

	if !*skipWorktree {
		if err := verifyCheckout(facts); err != nil {
			return err
		}
	} else {
		logf("worktree_check_skipped", "operator requested --skip-worktree-check")
	}

	result, err := youtrack.Validate(stage, facts)
	if err != nil {
		return err
	}
	if *dryRun {
		payload, _ := json.MarshalIndent(result, "", "  ")
		fmt.Printf("dry run: no writes\n%s\n", payload)
		return nil
	}
	skipped, err := youtrack.Apply(ctx, client, current, result)
	if err != nil {
		return err
	}
	payload, _ := json.MarshalIndent(map[string]any{
		"issue":   issue,
		"stage":   result.Stage,
		"fields":  result.Fields,
		"skipped": append([]string{}, skipped...),
	}, "", "  ")
	fmt.Println(string(payload))
	if len(skipped) > 0 {
		logf("fields_missing", "not defined on this project: "+strings.Join(skipped, ", "))
	}
	return nil
}

// verifyCheckout proves the branch and commit belong to the recorded worktree,
// so a transition cannot cite a checkout that does not exist.
func verifyCheckout(facts youtrack.Facts) error {
	if facts.WorktreePath == "" {
		return nil
	}
	info, err := os.Stat(facts.WorktreePath)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("worktree %q is not a directory", facts.WorktreePath)
	}
	if facts.Branch != "" {
		got, err := gitOutput(facts.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return fmt.Errorf("worktree %q is not a git checkout: %w", facts.WorktreePath, err)
		}
		if got != facts.Branch {
			return fmt.Errorf("worktree %q is on branch %q, not %q", facts.WorktreePath, got, facts.Branch)
		}
	}
	for _, commit := range []string{facts.Commit, facts.ReviewedCommit} {
		if commit == "" {
			continue
		}
		if _, err := gitOutput(facts.WorktreePath, "cat-file", "-e", commit+"^{commit}"); err != nil {
			return fmt.Errorf("commit %s is not in worktree %q", commit, facts.WorktreePath)
		}
	}
	return nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", filepath.Clean(dir)}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

type sessionList struct {
	Data []struct {
		ID           string `json:"id"`
		Branch       string `json:"branch"`
		Status       string `json:"status"`
		IsTerminated bool   `json:"isTerminated"`
	} `json:"data"`
}

// listSessions reads session facts from the local daemon through the CLI.
func listSessions(ctx context.Context, aoBin string) ([]youtrack.SessionFact, error) {
	cmd := exec.CommandContext(ctx, aoBin, "session", "ls", "--json", "--all", "--include-terminated")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read sessions: %w", err)
	}
	var decoded sessionList
	if err := json.Unmarshal(out, &decoded); err != nil {
		return nil, fmt.Errorf("decode sessions: %w", err)
	}
	facts := make([]youtrack.SessionFact, 0, len(decoded.Data))
	for _, entry := range decoded.Data {
		facts = append(facts, youtrack.SessionFact{
			ID:     entry.ID,
			Branch: entry.Branch,
			Live:   !entry.IsTerminated,
		})
	}
	return facts, nil
}

func logf(code, detail string) {
	fmt.Fprintf(os.Stderr, "note: %s: %s\n", code, detail)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ao-youtrack <start|submit|review|prelive> --issue CKM-825 [flags]

The token is read from $YOUTRACK_TOKEN (override with --token-env) and is never
printed. Every write is followed by a readback of the fields it changed.`)
}
