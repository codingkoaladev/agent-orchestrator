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
	ID                     string `json:"id"`
	NativeID               string `json:"agentSessionId"`
	ProviderConversationID string `json:"providerConversationId"`
	ProjectID              string `json:"projectId"`
	Branch                 string `json:"branch"`
	Harness                string `json:"harness"`
	Mode                   string `json:"mode"`
	Model                  string `json:"model"`
	Effort                 string `json:"effort"`
	IsTerminated           bool   `json:"isTerminated"`
	Status                 string `json:"status"`
	Activity               struct {
		State string `json:"state"`
	} `json:"activity"`
}

// resolveNativeIdentity maps a session's projected identities onto the agent's
// own session id.
//
// Chat workers do not populate agentSessionId; their identity is the
// harness-tagged provider conversation handle, for example
// "ao-harness:opencode-v2:ses_abc". The harness segment is part of the trust
// boundary: a handle tagged for another harness is ambiguous and must be
// refused rather than silently treated as this session's native id. When no
// provider handle exists the harness's own agentSessionId is authoritative.
func resolveNativeIdentity(harness, agentSessionID, providerConversationID string) (string, error) {
	provider := strings.TrimSpace(providerConversationID)
	agent := strings.TrimSpace(agentSessionID)
	if provider == "" {
		return agent, nil
	}
	parts := strings.SplitN(provider, ":", 3)
	if len(parts) != 3 || parts[0] != "ao-harness" || strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[2]) == "" {
		return "", fmt.Errorf("coordinator: provider conversation %q is not a harness-tagged native identity", provider)
	}
	if harness != "" && parts[1] != harness {
		return "", fmt.Errorf("coordinator: provider conversation is tagged %q but the session harness is %q", parts[1], harness)
	}
	native := strings.TrimSpace(parts[2])
	if agent != "" && agent != native {
		return "", fmt.Errorf("coordinator: session reports conflicting native identities %q and %q", agent, native)
	}
	return native, nil
}

func (d *Daemon) getWire(ctx context.Context, id string) (sessionWire, error) {
	var response struct {
		Session sessionWire `json:"session"`
	}
	if err := d.do(ctx, http.MethodGet, "/api/v1/sessions/"+id, nil, &response); err != nil {
		return sessionWire{}, err
	}
	if strings.TrimSpace(response.Session.ID) == "" {
		return sessionWire{}, errors.New("coordinator: daemon response has no session id")
	}
	return response.Session, nil
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
	session := Session{
		OrchestratorID: response.Session.ID,
		Branch:         firstNonEmpty(response.Session.Branch, request.Branch),
		Model:          firstNonEmpty(response.Session.Model, request.Model),
		Effort:         request.Effort,
		Subscription:   request.Subscription,
	}
	native, identityErr := resolveNativeIdentity(response.Session.Harness, response.Session.NativeID, response.Session.ProviderConversationID)
	session.NativeID = native
	worktree, worktreeErr := d.WorktreeFor(response.Session.ProjectID, response.Session.ID)
	session.WorktreePath = worktree
	if identityErr != nil {
		return session, identityErr
	}
	if worktreeErr != nil {
		return session, worktreeErr
	}
	return session, nil
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
		native, err := resolveNativeIdentity(session.Harness, session.NativeID, session.ProviderConversationID)
		if err != nil {
			// An ambiguous identity would make duplicate-dispatch protection
			// fail open. Treat the whole listing as unknown liveness.
			return nil, fmt.Errorf("coordinator: session %s has an ambiguous native identity: %w", session.ID, err)
		}
		facts = append(facts, youtrack.SessionFact{
			ID:           session.ID,
			NativeID:     native,
			Branch:       session.Branch,
			WorktreePath: worktree,
			Live:         !session.IsTerminated,
		})
	}
	return facts, nil
}

// Get reads one session.
func (d *Daemon) Get(ctx context.Context, id string) (SessionStatus, error) {
	session, err := d.getWire(ctx, id)
	if err != nil {
		return SessionStatus{}, err
	}
	native, err := resolveNativeIdentity(session.Harness, session.NativeID, session.ProviderConversationID)
	if err != nil {
		return SessionStatus{}, err
	}
	worktree, _ := d.WorktreeFor(session.ProjectID, session.ID)
	return SessionStatus{
		ID:                     session.ID,
		NativeID:               native,
		ProviderConversationID: session.ProviderConversationID,
		Harness:                session.Harness,
		Mode:                   session.Mode,
		Branch:                 session.Branch,
		WorktreePath:           worktree,
		Model:                  session.Model,
		Status:                 session.Status,
		Activity:               session.Activity.State,
		Terminated:             session.IsTerminated,
	}, nil
}

// Send delivers a message to the worker. Chat workers accept turns on the
// conversation API; sending to the terminal endpoint would be silently wrong
// for them. ClientMessageID makes a retry after a crash idempotent.
func (d *Daemon) Send(ctx context.Context, id string, message SendRequest) error {
	if strings.TrimSpace(message.Text) == "" {
		return errors.New("coordinator: cannot send an empty message")
	}
	session, err := d.getWire(ctx, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(session.Mode) == "chat" {
		body := map[string]any{"text": message.Text}
		if message.ClientMessageID != "" {
			body["clientMessageId"] = message.ClientMessageID
		}
		return d.do(ctx, http.MethodPost, "/api/v1/sessions/"+id+"/conversation/messages", body, nil)
	}
	return d.do(ctx, http.MethodPost, "/api/v1/sessions/"+id+"/send", map[string]any{"message": message.Text}, nil)
}

// Snapshotter materializes one exact commit outside the worker's dirty
// worktree, so environment-only local changes can never enter a review.
type Snapshotter interface {
	Create(ctx context.Context, worktree, commit string) (path string, cleanup func(), err error)
}

// GitSnapshot materializes one exact commit in a temporary isolated clone, so
// environment-only local changes can never enter a review while the reviewer
// still has the real commit diff.
type GitSnapshot struct{}

// Create clones the source repository into a private temp dir and checks out
// commit there. Uncommitted and untracked files in the source worktree are
// deliberately absent, and the source repository's metadata is not modified.
func (GitSnapshot) Create(ctx context.Context, worktree, commit string) (string, func(), error) {
	if !commitPattern.MatchString(commit) {
		return "", nil, fmt.Errorf("coordinator: snapshot needs a full commit SHA, got %q", commit)
	}
	dir, err := os.MkdirTemp("", "ao-review-"+commit[:12]+"-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	clone := exec.CommandContext(ctx, "git", "clone", "--shared", "--no-checkout", "--quiet", worktree, dir)
	var cloneErr bytes.Buffer
	clone.Stderr = &cloneErr
	if err := clone.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("coordinator: clone review snapshot: %w", err)
	}
	checkout := exec.CommandContext(ctx, "git", "-c", "core.autocrlf=false", "-C", dir, "checkout", "--detach", "--quiet", commit)
	var checkoutErr bytes.Buffer
	checkout.Stderr = &checkoutErr
	if err := checkout.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("coordinator: check out review commit %s: %w", commit, err)
	}
	return dir, cleanup, nil
}

// CodexReviewer runs the independent reviewer as a separate process, distinct
// from the coding session. Its native session id comes from the reviewer's own
// JSON event stream; a locally generated handle is never recorded as identity.
type CodexReviewer struct {
	Binary   string
	Model    string
	Effort   string
	Snapshot Snapshotter
}

// Review runs `codex exec` against one exact commit and turns its final message
// into a verdict. A missing native thread id is a failure to review, never an
// invitation to invent one.
func (r CodexReviewer) Review(ctx context.Context, request ReviewRequest) (ReviewResult, error) {
	if !commitPattern.MatchString(strings.TrimSpace(request.Commit)) {
		return ReviewResult{}, errors.New("coordinator: review needs a full 40-character commit")
	}
	dir := request.Worktree
	cleanup := func() {}
	if request.IsolatedSnapshot && r.Snapshot == nil {
		return ReviewResult{}, errors.New("coordinator: review requires an isolated commit snapshot")
	}
	if r.Snapshot != nil {
		snapshot, release, err := r.Snapshot.Create(ctx, request.Worktree, request.Commit)
		if err != nil {
			return ReviewResult{}, err
		}
		dir, cleanup = snapshot, release
	}
	defer cleanup()

	output, err := os.CreateTemp("", "ao-review-message-*.txt")
	if err != nil {
		return ReviewResult{}, err
	}
	outputPath := output.Name()
	_ = output.Close()
	defer os.Remove(outputPath)

	binary := firstNonEmpty(r.Binary, "codex")
	model := firstNonEmpty(request.Model, r.Model, "gpt-6.1-sol")
	effort := firstNonEmpty(request.Effort, r.Effort, "low")
	args := []string{"exec", "--skip-git-repo-check", "--json", "-m", model, "-C", dir, "-o", outputPath}
	if effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
	}
	args = append(args, reviewPrompt(request))
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return ReviewResult{}, fmt.Errorf("coordinator: reviewer run failed: %w", err)
	}
	threadID := parseCodexThreadID(stdout.String())
	if threadID == "" {
		return ReviewResult{}, errors.New("coordinator: reviewer did not report a native session id; refusing a generated identity")
	}
	message, err := os.ReadFile(outputPath)
	if err != nil {
		return ReviewResult{}, fmt.Errorf("coordinator: read reviewer message: %w", err)
	}
	verdict, findings := parseVerdict(string(message))
	return ReviewResult{
		Verdict:  verdict,
		Session:  threadID,
		Model:    model,
		Effort:   effort,
		Findings: findings,
		Source:   "codex-cli",
	}, nil
}

func reviewPrompt(request ReviewRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the independent reviewer for %s. Review only commit %s.\n", request.Issue, request.Commit)
	b.WriteString("The checkout may contain uncommitted environment files. They are not part of the submission, must not be reviewed, and must not be accepted as changes.\n")
	if strings.TrimSpace(request.Summary) != "" {
		fmt.Fprintf(&b, "\nTicket summary:\n%s\n", request.Summary)
	}
	if strings.TrimSpace(request.Acceptance) != "" {
		fmt.Fprintf(&b, "\nTicket acceptance contract:\n%s\n", request.Acceptance)
	}
	b.WriteString("\nReport every correctness, safety, lifecycle or contract problem you find. ")
	b.WriteString("Answer with a final line of PASS when the commit needs no correction, or FAIL followed by the findings.")
	return b.String()
}

// parseCodexThreadID reads the reviewer's real session id from its JSONL event
// stream. Only the thread lifecycle event is accepted; arbitrary ids in tool
// output are not a native session identity.
func parseCodexThreadID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue
		}
		if event.Type == "thread.started" && strings.TrimSpace(event.ThreadID) != "" {
			return strings.TrimSpace(event.ThreadID)
		}
	}
	return ""
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

// PullRequest is the observed GitHub state for one branch.
type PullRequest struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	HeadRef    string `json:"headRefName"`
	HeadCommit string `json:"headRefOid"`
	BaseRef    string `json:"baseRefName"`
	State      string `json:"state"`
	Draft      bool   `json:"isDraft"`
}

// PullRequests resolves the open PR a branch submits to.
type PullRequests interface {
	Find(ctx context.Context, worktree, branch string) (PullRequest, error)
}

// GHPullRequests reads the open PR through the authenticated gh CLI. It never
// prints the token; gh reads it from its own credential store.
type GHPullRequests struct {
	Binary string
}

// Find requires exactly one open PR for branch.
func (g GHPullRequests) Find(ctx context.Context, worktree, branch string) (PullRequest, error) {
	binary := firstNonEmpty(g.Binary, "gh")
	cmd := exec.CommandContext(ctx, binary, "pr", "list", "--head", branch, "--state", "open", "--limit", "10",
		"--json", "number,url,headRefName,headRefOid,baseRefName,state,isDraft")
	cmd.Dir = worktree
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return PullRequest{}, fmt.Errorf("coordinator: read pull requests for %s: %w", branch, err)
	}
	var requests []PullRequest
	if err := json.Unmarshal(out, &requests); err != nil {
		return PullRequest{}, fmt.Errorf("coordinator: decode pull requests: %w", err)
	}
	switch len(requests) {
	case 0:
		return PullRequest{}, fmt.Errorf("coordinator: no open pull request for branch %s", branch)
	case 1:
		return requests[0], nil
	default:
		return PullRequest{}, fmt.Errorf("coordinator: %d open pull requests for branch %s; refusing an ambiguous submission", len(requests), branch)
	}
}

// CheckoutState is what the worktree proves about the submitted commit.
type CheckoutState struct {
	Head       string
	Branch     string
	Staged     []string
	Unstaged   []string
	Untracked  []string
	Conflicted []string
}

// Checkout validates the worker's registered checkout.
type Checkout interface {
	Validate(ctx context.Context, worktree, branch, commit string) (CheckoutState, error)
}

// GitCheckout validates HEAD, branch and the index. Unstaged and untracked
// environment files are reported but tolerated: they are not part of HEAD and
// are excluded from the review snapshot. Staged code is refused because it is
// not in the submitted commit.
type GitCheckout struct{}

// Validate fails closed when the checkout cannot prove the submitted commit.
func (GitCheckout) Validate(ctx context.Context, worktree, branch, commit string) (CheckoutState, error) {
	if strings.TrimSpace(worktree) == "" {
		return CheckoutState{}, errors.New("coordinator: worktree path is required")
	}
	info, err := os.Stat(worktree)
	if err != nil || !info.IsDir() {
		return CheckoutState{}, fmt.Errorf("coordinator: worktree %q is not a directory", worktree)
	}
	head, err := git(ctx, worktree, "rev-parse", "HEAD")
	if err != nil {
		return CheckoutState{}, err
	}
	state := CheckoutState{Head: strings.TrimSpace(head)}
	if state.Head != commit {
		return state, fmt.Errorf("coordinator: worktree HEAD %s is not the submitted commit %s", state.Head, commit)
	}
	if branch != "" {
		current, err := git(ctx, worktree, "symbolic-ref", "--quiet", "--short", "HEAD")
		if err != nil {
			return state, fmt.Errorf("coordinator: worktree is not on the intended branch %s: %w", branch, err)
		}
		state.Branch = strings.TrimSpace(current)
		if state.Branch != branch {
			return state, fmt.Errorf("coordinator: worktree is on branch %q, not the intended branch %q", state.Branch, branch)
		}
	}
	state.Staged, err = gitNameList(ctx, worktree, "diff", "--cached", "--name-only")
	if err != nil {
		return state, err
	}
	if len(state.Staged) > 0 {
		return state, fmt.Errorf("coordinator: worktree has staged changes not in the submitted commit: %s", strings.Join(bounded(state.Staged), ", "))
	}
	state.Conflicted, err = gitNameList(ctx, worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return state, err
	}
	if len(state.Conflicted) > 0 {
		return state, fmt.Errorf("coordinator: worktree has unresolved conflicts: %s", strings.Join(bounded(state.Conflicted), ", "))
	}
	state.Unstaged, err = gitNameList(ctx, worktree, "diff", "--name-only")
	if err != nil {
		return state, err
	}
	state.Untracked, err = gitNameList(ctx, worktree, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return state, err
	}
	return state, nil
}

func bounded(values []string) []string {
	if len(values) <= 10 {
		return values
	}
	return append(values[:10:10], fmt.Sprintf("… and %d more", len(values)-10))
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

func gitNameList(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	names := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			names = append(names, trimmed)
		}
	}
	return names, nil
}
