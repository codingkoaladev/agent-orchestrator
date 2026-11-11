package coordinator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

// Daemon is the loopback Orchestrator API client.
type Daemon struct {
	BaseURL string
	HTTP    *http.Client
	// DataDir is AO's state root, used to resolve a session's worktree because
	// the API deliberately keeps that path private.
	DataDir string
}

// NewDaemon resolves the running daemon's loopback address from AO's run file.
func NewDaemon(runFile, dataDir string) (*Daemon, error) {
	data, err := os.ReadFile(runFile)
	if err != nil {
		return nil, fmt.Errorf("coordinator: read run file: %w", err)
	}
	var state struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("coordinator: decode run file: %w", err)
	}
	if state.Port <= 0 {
		return nil, errors.New("coordinator: run file has no daemon port")
	}
	return &Daemon{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d", state.Port),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
		DataDir: dataDir,
	}, nil
}

func (d *Daemon) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = encoded
	}
	request, err := http.NewRequestWithContext(ctx, method, d.BaseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := d.HTTP.Do(request)
	if err != nil {
		return fmt.Errorf("coordinator: %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(response.Body).Decode(&envelope)
		message := envelope.Error.Message
		if message == "" {
			message = envelope.Message
		}
		return fmt.Errorf("coordinator: %s %s: HTTP %d %s", method, path, response.StatusCode, message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(response.Body).Decode(out)
}

type sessionWire struct {
	ID           string `json:"id"`
	NativeID     string `json:"agentSessionId"`
	ProjectID    string `json:"projectId"`
	Branch       string `json:"branch"`
	Harness      string `json:"harness"`
	Model        string `json:"model"`
	IsTerminated bool   `json:"isTerminated"`
	Status       string `json:"status"`
	Activity     struct {
		State string `json:"state"`
	} `json:"activity"`
}

// Spawn starts a worker and resolves its durable identities.
func (d *Daemon) Spawn(ctx context.Context, request SpawnRequest) (Session, error) {
	// The daemon currently scopes subscription selection through project Env.
	// Require an explicit matching pin rather than claiming an ignored override.
	if request.Subscription != "" {
		var project struct {
			Project struct {
				Config struct {
					Env map[string]string `json:"env"`
				} `json:"config"`
			} `json:"project"`
		}
		if err := d.do(ctx, http.MethodGet, "/api/v1/projects/"+request.Project, nil, &project); err != nil {
			return Session{}, fmt.Errorf("%w: subscription preflight: %v", ErrNotDispatched, err)
		}
		if project.Project.Config.Env["OPENCODE_SUBSCRIPTION"] != request.Subscription {
			return Session{}, fmt.Errorf("%w: requested subscription must match the project's explicit OPENCODE_SUBSCRIPTION pin before dispatch", ErrNotDispatched)
		}
	}
	body := map[string]any{
		"projectId":   request.Project,
		"issueId":     request.Issue,
		"kind":        "worker",
		"harness":     "opencode-v2",
		"branch":      request.Branch,
		"prompt":      request.Prompt,
		"displayName": request.DisplayName,
	}
	if request.Model != "" {
		body["model"] = request.Model
	}
	if request.Effort != "" {
		body["effort"] = request.Effort
	}
	var response struct {
		Session sessionWire `json:"session"`
	}
	if err := d.do(ctx, http.MethodPost, "/api/v1/sessions", body, &response); err != nil {
		return Session{}, err
	}
	worktree, err := d.WorktreeFor(response.Session.ProjectID, response.Session.ID)
	if err != nil {
		return Session{}, err
	}
	return Session{
		OrchestratorID: response.Session.ID,
		NativeID:       response.Session.NativeID,
		Branch:         firstNonEmpty(response.Session.Branch, request.Branch),
		WorktreePath:   worktree,
		Model:          firstNonEmpty(response.Session.Model, request.Model),
		Effort:         request.Effort,
		Subscription:   request.Subscription,
	}, nil
}

// WorktreeFor resolves a session worktree. AO keeps the path off the API, so the
// coordinator derives its documented layout and refuses a path that is absent.
func (d *Daemon) WorktreeFor(projectID, sessionID string) (string, error) {
	if d.DataDir == "" || projectID == "" || sessionID == "" {
		return "", errors.New("coordinator: cannot resolve worktree without data dir, project and session")
	}
	path := filepath.Join(d.DataDir, "worktrees", projectID, sessionID)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("coordinator: session worktree %q is missing", path)
	}
	return path, nil
}

// List reports every session, or an error the caller must treat as unknown
// liveness rather than as "nothing is running".
func (d *Daemon) List(ctx context.Context) ([]youtrack.SessionFact, error) {
	var response struct {
		Sessions *[]sessionWire `json:"sessions"`
	}
	if err := d.do(ctx, http.MethodGet, "/api/v1/sessions?includeTerminated=true", nil, &response); err != nil {
		return nil, err
	}
	if response.Sessions == nil {
		return nil, errors.New("coordinator: daemon response has no sessions list; liveness unknown")
	}
	facts := make([]youtrack.SessionFact, 0, len(*response.Sessions))
	for _, session := range *response.Sessions {
		worktree, _ := d.WorktreeFor(session.ProjectID, session.ID)
		facts = append(facts, youtrack.SessionFact{
			ID:           session.ID,
			NativeID:     session.NativeID,
			Branch:       session.Branch,
			WorktreePath: worktree,
			Live:         !session.IsTerminated,
		})
	}
	return facts, nil
}

// Get reads one session.
func (d *Daemon) Get(ctx context.Context, id string) (SessionStatus, error) {
	var response struct {
		Session sessionWire `json:"session"`
	}
	if err := d.do(ctx, http.MethodGet, "/api/v1/sessions/"+id, nil, &response); err != nil {
		return SessionStatus{}, err
	}
	worktree, _ := d.WorktreeFor(response.Session.ProjectID, response.Session.ID)
	return SessionStatus{
		ID:           response.Session.ID,
		NativeID:     response.Session.NativeID,
		Branch:       response.Session.Branch,
		WorktreePath: worktree,
		Status:       response.Session.Status,
		Activity:     response.Session.Activity.State,
		Terminated:   response.Session.IsTerminated,
	}, nil
}

// Send delivers a message to the running worker.
func (d *Daemon) Send(ctx context.Context, id, text string) error {
	return d.do(ctx, http.MethodPost, "/api/v1/sessions/"+id+"/send", map[string]any{"message": text}, nil)
}

// CodexReviewer runs the independent reviewer as a separate process, distinct
// from the coding session.
type CodexReviewer struct {
	Binary string
	Model  string
	Effort string
	// Session names the reviewer identity recorded with the verdict. A CLI
	// review has no daemon session, so the coordinator derives a stable id.
	SessionPrefix string
}

// Review runs `codex review --commit <sha>` in the worktree and turns its output
// into a verdict. A non-zero exit or an unreadable result is a failure to
// review, not a failing review.
func (r CodexReviewer) Review(ctx context.Context, request ReviewRequest) (ReviewResult, error) {
	if strings.TrimSpace(request.Commit) == "" {
		return ReviewResult{}, errors.New("coordinator: review needs a commit")
	}
	binary := firstNonEmpty(r.Binary, "codex")
	args := []string{"exec", "--skip-git-repo-check", "-m", firstNonEmpty(r.Model, "gpt-6.1-sol")}
	if effort := firstNonEmpty(r.Effort, "low"); effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	args = append(args, "Review commit "+request.Commit+
		". Report every correctness, safety or contract problem you find. "+
		"Answer with a final line of PASS when the commit needs no correction, or FAIL followed by the findings.")
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = request.Worktree
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return ReviewResult{}, fmt.Errorf("coordinator: reviewer run failed: %w", err)
	}
	verdict, findings := parseVerdict(out.String())
	return ReviewResult{
		Verdict:  verdict,
		Session:  r.sessionID(request),
		Model:    firstNonEmpty(r.Model, "gpt-6.1-sol"),
		Effort:   firstNonEmpty(r.Effort, "low"),
		Findings: findings,
		Source:   "codex-cli:" + request.Commit,
	}, nil
}

func (r CodexReviewer) sessionID(request ReviewRequest) string {
	prefix := firstNonEmpty(r.SessionPrefix, "codex-review")
	sum := sha256.Sum256([]byte(prefix + ":" + request.Issue + ":" + request.Commit))
	return prefix + "-" + hex.EncodeToString(sum[:6])
}

// parseVerdict reads the reviewer's final verdict line.
func parseVerdict(output string) (string, string) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		upper := strings.ToUpper(line)
		if upper == "PASS" || strings.HasPrefix(upper, "PASS ") || strings.HasPrefix(upper, "PASS:") {
			return youtrack.VerdictPass, strings.TrimSpace(output)
		}
		if upper == "FAIL" || strings.HasPrefix(upper, "FAIL ") || strings.HasPrefix(upper, "FAIL:") {
			return youtrack.VerdictFail, strings.TrimSpace(output)
		}
	}
	// No explicit verdict is an inconclusive review.
	return "", strings.TrimSpace(output)
}

// GitDetector answers which commit is ready for review, with no model calls.
type GitDetector struct{}

// Detect reports HEAD when it differs from base and the tree is clean enough to
// review (no uncommitted changes to tracked files).
func (GitDetector) Detect(ctx context.Context, worktree, base string) (string, error) {
	status, err := git(ctx, worktree, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", errors.New("worktree has uncommitted changes")
	}
	head, err := git(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	head = strings.TrimSpace(head)
	if base != "" && head == base {
		return "", errors.New("no new commit yet")
	}
	return head, nil
}

// CommandPrelive runs the configured pre-live command in the worktree and
// records the command, exit code and output digest.
type CommandPrelive struct{}

// Run executes one check. It never treats a non-zero exit as success.
func (CommandPrelive) Run(ctx context.Context, worktree, commit, command string) (Prelive, error) {
	if strings.TrimSpace(command) == "" {
		return Prelive{}, errors.New("coordinator: pre-live command is not configured")
	}
	cmd := exec.CommandContext(ctx, "bash", "-lc", command)
	cmd.Dir = worktree
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	digest := sha256.Sum256(output)
	record := Prelive{
		Commit:       commit,
		Command:      command,
		ExitCode:     0,
		OutputDigest: hex.EncodeToString(digest[:]),
		At:           time.Now().UTC(),
	}
	if err != nil {
		record.ExitCode = 1
		if exit, ok := err.(*exec.ExitError); ok {
			record.ExitCode = exit.ExitCode()
		}
		return record, fmt.Errorf("coordinator: pre-live check failed with exit %d", record.ExitCode)
	}
	return record, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("coordinator: git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
