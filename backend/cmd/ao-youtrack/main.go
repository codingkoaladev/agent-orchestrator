// Command ao-youtrack applies one Orchestrator → YouTrack workflow transition.
//
// It is a thin, auditable bridge. The coordinator supplies only identities and
// the local checkout; the ticket is read live and the review verdict and
// pre-live evidence come from the coordinator's durable run record. A caller
// cannot assert a passing review or a reviewed commit.
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

	"github.com/aoagents/agent-orchestrator/backend/internal/coordinator"
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
		issue        = fs.String("issue", "", "YouTrack issue id, for example CKM-825")
		session      = fs.String("session", "", "agent-native coding session id")
		aoSession    = fs.String("ao-session", "", "Orchestrator daemon session id")
		branch       = fs.String("branch", "", "branch under work")
		worktree     = fs.String("worktree", "", "absolute worktree path")
		model        = fs.String("model", "", "provider-qualified coder model")
		commit       = fs.String("commit", "", "full 40-character commit SHA to submit")
		runDir       = fs.String("run-dir", defaultRunDir(), "coordinator run-state directory")
		baseURL      = fs.String("base-url", "", "YouTrack REST base URL")
		tokenEnv     = fs.String("token-env", "YOUTRACK_TOKEN", "environment variable holding the token")
		dryRun       = fs.Bool("dry-run", false, "validate and print the transition without writing")
		skipWorktree = fs.Bool("skip-worktree-check", false, "skip local git verification")
		aoBin        = fs.String("ao-bin", "ao", "Orchestrator CLI used to read session facts")
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
	store := coordinator.Store{Dir: *runDir}

	facts, review, prelive, err := assembleFacts(ctx, stage, store, current, factsInput{
		issue: *issue, session: *session, aoSession: *aoSession, branch: *branch,
		worktree: *worktree, model: *model, commit: *commit, aoBin: *aoBin,
	})
	if err != nil {
		return err
	}
	if stage != youtrack.StageStart && stage != youtrack.StageSubmit && !*skipWorktree {
		if err := verifyCheckout(facts); err != nil {
			return err
		}
	}
	if stage == youtrack.StageStart || stage == youtrack.StageSubmit {
		if !*skipWorktree {
			if err := verifyCheckout(facts); err != nil {
				return err
			}
		}
	}

	result, err := youtrack.Validate(stage, facts, review, prelive)
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

type factsInput struct {
	issue, session, aoSession, branch, worktree, model, commit, aoBin string
}

// assembleFacts builds the guard facts from the live ticket, the run record and
// the local checkout. Review and pre-live stages take their identities and
// evidence from the run record, never from flags.
func assembleFacts(ctx context.Context, stage youtrack.Stage, store coordinator.Store, current youtrack.Issue, in factsInput) (youtrack.Facts, *youtrack.ReviewEvidence, *youtrack.PreliveEvidence, error) {
	ticket := youtrack.TicketFacts{
		State:              current.Fields[youtrack.FieldState].Value,
		AIStatus:           current.Fields[youtrack.FieldAIStatus].Value,
		SubmittedCommit:    current.Fields[youtrack.FieldLastSubmitted].Value,
		LastReviewedCommit: current.Fields[youtrack.FieldLastReviewed].Value,
		VerifiedCommit:     current.Fields[youtrack.FieldPreliveVerified].Value,
		RecordedSessionID:  current.Fields[youtrack.FieldCodingSession].Value,
		CoderModel:         current.Fields[youtrack.FieldCoderModel].Value,
	}
	facts := youtrack.Facts{IssueID: in.issue, Ticket: ticket, Commit: in.commit}
	var review *youtrack.ReviewEvidence
	var prelive *youtrack.PreliveEvidence

	run, runErr := store.Load(in.issue)
	if errors.Is(runErr, coordinator.ErrNoRun) {
		// Review and pre-live evidence lives in the run record; without it there
		// is nothing authoritative to record and a caller could only assert one.
		if stage == youtrack.StageReview || stage == youtrack.StagePrelive {
			return facts, nil, nil, fmt.Errorf("no recorded run for %s: %w", in.issue, coordinator.ErrNoRun)
		}
	} else if runErr != nil {
		return facts, nil, nil, runErr
	}
	if runErr == nil {
		facts.CoderSessionID = run.Coder.NativeID
		facts.OrchestratorSessionID = run.Coder.OrchestratorID
		facts.Branch = run.Coder.Branch
		facts.WorktreePath = run.Coder.WorktreePath
		facts.CoderModel = run.Coder.Model
		if stage == youtrack.StageReview || stage == youtrack.StagePrelive {
			facts.Commit = run.SubmittedCommit
		}
	}
	// Explicit flags only fill gaps the run record does not own.
	if in.session != "" {
		facts.CoderSessionID = in.session
	}
	if in.aoSession != "" {
		facts.OrchestratorSessionID = in.aoSession
	}
	if in.branch != "" {
		facts.Branch = in.branch
	}
	if in.worktree != "" {
		facts.WorktreePath = in.worktree
	}
	if in.model != "" {
		facts.CoderModel = in.model
	}

	sessions, err := listSessions(ctx, in.aoBin)
	if err != nil {
		// Unknown liveness must not authorize a transition that could double
		// dispatch or overwrite a newer submission.
		return facts, nil, nil, fmt.Errorf("session liveness unknown: %w", err)
	}
	facts.Sessions = sessions
	facts.LivenessKnown = true

	if runErr == nil {
		if recorded, ok := run.LatestReview(facts.Commit); ok {
			evidence := recorded.ReviewEvidence()
			review = &evidence
		}
		if run.PreliveCheck != nil {
			evidence := run.PreliveCheck.Evidence()
			prelive = &evidence
		}
	}
	return facts, review, prelive, nil
}

// verifyCheckout proves the branch and commit belong to the recorded worktree.
func verifyCheckout(facts youtrack.Facts) error {
	if facts.WorktreePath == "" {
		return errors.New("worktree path is required")
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
	for _, commit := range []string{facts.Commit} {
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
		facts = append(facts, youtrack.SessionFact{ID: entry.ID, Branch: entry.Branch, Live: !entry.IsTerminated})
	}
	return facts, nil
}

func defaultRunDir() string {
	if dir := strings.TrimSpace(os.Getenv("AO_DATA_DIR")); dir != "" {
		return coordinator.DefaultStoreDir(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".ao", "coordinator", "runs")
	}
	return coordinator.DefaultStoreDir(filepath.Join(home, ".ao", "data"))
}

func logf(code, detail string) {
	fmt.Fprintf(os.Stderr, "note: %s: %s\n", code, detail)
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ao-youtrack <start|submit|review|prelive> --issue CKM-825 [flags]

Review and pre-live evidence comes from the coordinator run record; a caller
cannot assert a verdict. The token is read from $YOUTRACK_TOKEN (override with
--token-env) and is never printed. Every write is followed by a readback and a
compare-and-set against the ticket's submitted commit.`)
}
