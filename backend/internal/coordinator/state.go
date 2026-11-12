// Package coordinator runs one ticket's coding → review → correction →
// pre-live lifecycle against the local Orchestrator daemon and YouTrack.
//
// The durable run record is the coordinator's memory: it survives a process
// restart, carries the identities needed to recognise its own work, and holds
// the review verdicts and pre-live evidence that the YouTrack guards require.
// Verdicts are only ever written here after the reviewer actually ran.
package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

// Phase is the lifecycle position of one ticket run.
type Phase string

const (
	PhaseDispatching     Phase = "dispatching"
	PhaseCoding          Phase = "coding"
	PhaseAwaitingReview  Phase = "awaiting_review"
	PhaseReviewing       Phase = "reviewing"
	PhaseCorrecting      Phase = "correcting"
	PhaseReviewPassed    Phase = "review_passed"
	PhaseAwaitingPrelive Phase = "awaiting_prelive"
	PhasePreliveVerified Phase = "prelive_verified"
	PhaseBlocked         Phase = "blocked"
	PhaseComplete        Phase = "complete"
)

// Active reports whether a phase still owns the ticket. Starting another run
// while an active phase exists requires the recorded session to be gone.
func (p Phase) Active() bool {
	switch p {
	case PhaseDispatching, PhaseCoding, PhaseAwaitingReview, PhaseReviewing, PhaseCorrecting, PhaseReviewPassed, PhaseAwaitingPrelive:
		return true
	}
	return false
}

// Session is the code worker a run owns.
type Session struct {
	// OrchestratorID is the daemon session id; NativeID is the agent's own
	// session id. They live in different id spaces and both are persisted.
	OrchestratorID string `json:"orchestratorId"`
	NativeID       string `json:"nativeId,omitempty"`
	Branch         string `json:"branch"`
	WorktreePath   string `json:"worktreePath"`
	Model          string `json:"model"`
	Effort         string `json:"effort,omitempty"`
	// Subscription is the OpenCode subscription label pinned for this worker.
	Subscription string `json:"subscription,omitempty"`
}

// Review is one independent review that actually ran.
type Review struct {
	Commit   string    `json:"commit"`
	Verdict  string    `json:"verdict"`
	Session  string    `json:"session"`
	Model    string    `json:"model"`
	Effort   string    `json:"effort,omitempty"`
	Source   string    `json:"source,omitempty"`
	Findings string    `json:"findings,omitempty"`
	At       time.Time `json:"at"`
}

// Prelive is one pre-live check that actually ran.
type Prelive struct {
	Commit       string    `json:"commit"`
	Command      string    `json:"command"`
	ExitCode     int       `json:"exitCode"`
	OutputDigest string    `json:"outputDigest"`
	At           time.Time `json:"at"`
}

// Run is the durable record for one ticket.
type Run struct {
	Issue           string   `json:"issue"`
	Project         string   `json:"project"`
	Phase           Phase    `json:"phase"`
	Coder           Session  `json:"coder"`
	SubmittedCommit string   `json:"submittedCommit,omitempty"`
	Reviews         []Review `json:"reviews,omitempty"`
	PreliveCheck    *Prelive `json:"prelive,omitempty"`
	ReviewRounds    int      `json:"reviewRounds"`
	// Approved records the owner's dispatch authorization so a restart can
	// resume the same run without a second approval prompt.
	Approved bool `json:"approved"`
	// ReviewInFlight names the commit whose reviewer process may have run. It is
	// persisted before the reviewer starts and cleared only after a verdict is
	// recorded, so a crash cannot authorise a duplicate review.
	ReviewInFlight string `json:"reviewInFlight,omitempty"`
	// CorrectionSentFor is the commit whose findings were delivered to the
	// coder. It makes correction delivery restart-safe and idempotent.
	CorrectionSentFor string `json:"correctionSentFor,omitempty"`
	// LastReviewRequestAt is the ticket's Review Requested At value that the
	// scheduler last acted on, kept for audit and duplicate-request reporting.
	LastReviewRequestAt string            `json:"lastReviewRequestAt,omitempty"`
	Note                string            `json:"note,omitempty"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	Extra               map[string]string `json:"extra,omitempty"`
}

// LatestReview returns the newest verdict recorded for commit.
func (r Run) LatestReview(commit string) (Review, bool) {
	for i := len(r.Reviews) - 1; i >= 0; i-- {
		if r.Reviews[i].Commit == commit {
			return r.Reviews[i], true
		}
	}
	return Review{}, false
}

// PassingReview returns the newest passing verdict for commit.
func (r Run) PassingReview(commit string) (Review, bool) {
	for i := len(r.Reviews) - 1; i >= 0; i-- {
		if r.Reviews[i].Commit == commit && r.Reviews[i].Verdict == youtrack.VerdictPass {
			return r.Reviews[i], true
		}
	}
	return Review{}, false
}

// ReviewEvidence converts a recorded review into the evidence the YouTrack
// guards accept.
func (r Review) ReviewEvidence() youtrack.ReviewEvidence {
	return youtrack.ReviewEvidence{
		Commit:     r.Commit,
		Verdict:    r.Verdict,
		Session:    r.Session,
		Model:      r.Model,
		Effort:     r.Effort,
		Source:     r.Source,
		RecordedAt: r.At,
	}
}

// Evidence converts a recorded pre-live check into guard evidence.
func (p Prelive) Evidence() youtrack.PreliveEvidence {
	return youtrack.PreliveEvidence{
		Commit:       p.Commit,
		Command:      p.Command,
		ExitCode:     p.ExitCode,
		OutputDigest: p.OutputDigest,
		RecordedAt:   p.At,
	}
}

// ErrNoRun reports that a ticket has no recorded run.
var ErrNoRun = errors.New("coordinator: no recorded run for this ticket")

// errRunLocked reports that another live process owns the ticket run.
var errRunLocked = errors.New("coordinator: run lock is held by another supervisor")

var (
	issueFilePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	commitPattern    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// Store persists runs as one JSON file per ticket under Dir.
type Store struct {
	Dir string
}

// DefaultStoreDir is the coordinator state root inside AO's state directory.
func DefaultStoreDir(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		return filepath.Join(".ao", "coordinator", "runs")
	}
	return filepath.Join(dataDir, "coordinator", "runs")
}

// Path returns the run file for an issue.
func (s Store) Path(issue string) string {
	return filepath.Join(s.Dir, strings.ToUpper(strings.TrimSpace(issue))+".json")
}

// Load reads a run. A missing file reports ErrNoRun.
func (s Store) Load(issue string) (Run, error) {
	if !issueFilePattern.MatchString(strings.ToUpper(strings.TrimSpace(issue))) {
		return Run{}, fmt.Errorf("coordinator: invalid issue id %q", issue)
	}
	data, err := os.ReadFile(s.Path(issue))
	if errors.Is(err, os.ErrNotExist) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("coordinator: read run: %w", err)
	}
	var run Run
	if err := json.Unmarshal(data, &run); err != nil {
		return Run{}, fmt.Errorf("coordinator: decode run: %w", err)
	}
	return run, nil
}

// List returns every durable run record. It is the scheduler's explicit
// authorization set: only tickets an owner dispatched with `--approved` have a
// run. Lock and temporary files are ignored.
func (s Store) List(ctx context.Context) ([]Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("coordinator: list runs: %w", err)
	}
	runs := make([]Run, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		issue := strings.TrimSuffix(name, ".json")
		if !issueFilePattern.MatchString(issue) {
			continue
		}
		run, err := s.Load(issue)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// Save writes a run atomically with owner-only permissions.
func (s Store) Save(run Run) error {
	if !issueFilePattern.MatchString(strings.ToUpper(strings.TrimSpace(run.Issue))) {
		return fmt.Errorf("coordinator: invalid issue id %q", run.Issue)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("coordinator: create run dir: %w", err)
	}
	run.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("coordinator: encode run: %w", err)
	}
	temporary, err := os.CreateTemp(s.Dir, ".run-*")
	if err != nil {
		return fmt.Errorf("coordinator: stage run: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("coordinator: secure run: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("coordinator: write run: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("coordinator: sync run: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("coordinator: close run: %w", err)
	}
	if err := os.Rename(name, s.Path(run.Issue)); err != nil {
		return fmt.Errorf("coordinator: activate run: %w", err)
	}
	// Persist the directory entry too: a crash immediately after rename must not
	// resurrect the previous phase on restart.
	if dir, err := os.Open(s.Dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// AddReview appends a verdict, replacing any earlier verdict for the same
// commit so a corrected commit cannot inherit an old pass.
func (r *Run) AddReview(review Review) {
	r.Reviews = slices.DeleteFunc(r.Reviews, func(existing Review) bool {
		return existing.Commit == review.Commit
	})
	r.Reviews = append(r.Reviews, review)
}

// RunLock is a held, exclusive owner lock for one ticket run.
type RunLock struct {
	file *os.File
}

// Lock takes the exclusive supervisor lock for issue without waiting. The lock
// is bound to an open file descriptor, so the kernel releases it when the
// process exits, including a crash or SIGKILL; a stale lock file is harmless.
// Only one process may own a ticket's lifecycle at a time.
func (s Store) Lock(ctx context.Context, issue string) (*RunLock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !issueFilePattern.MatchString(strings.ToUpper(strings.TrimSpace(issue))) {
		return nil, fmt.Errorf("coordinator: invalid issue id %q", issue)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("coordinator: create run dir: %w", err)
	}
	path := s.Path(issue) + ".lock"
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("coordinator: open run lock: %w", err)
	}
	if err := tryRunLock(file); err != nil {
		holder := readRunLockPID(file)
		_ = file.Close()
		if errors.Is(err, errRunLocked) {
			if holder > 0 {
				return nil, fmt.Errorf("%w: %s (pid %d)", ErrAlreadyRunning, strings.ToUpper(strings.TrimSpace(issue)), holder)
			}
			return nil, fmt.Errorf("%w: %s", ErrAlreadyRunning, strings.ToUpper(strings.TrimSpace(issue)))
		}
		return nil, fmt.Errorf("coordinator: lock run: %w", err)
	}
	_ = file.Truncate(0)
	_, _ = file.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	return &RunLock{file: file}, nil
}

// Release frees the ticket lock. It is safe on a nil lock.
func (l *RunLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlockErr := unlockRunLock(l.file)
	closeErr := l.file.Close()
	l.file = nil
	if unlockErr != nil {
		return fmt.Errorf("coordinator: unlock run: %w", unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("coordinator: close run lock: %w", closeErr)
	}
	return nil
}

// readRunLockPID best-effort reads the holder PID. The lock itself lives in the
// kernel; this text is diagnostics only.
func readRunLockPID(file *os.File) int {
	buf := make([]byte, 32)
	n, err := file.ReadAt(buf, 0)
	if n == 0 && err != nil {
		return 0
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(buf[:n])))
	if convErr != nil {
		return 0
	}
	return pid
}

// Claim reserves a ticket before any external dispatch. The exclusive durable
// file survives crashes and prevents concurrent coordinators starting workers.
// An ambiguous dispatch is reconciled by an operator, never retried blindly.
func (s Store) Claim(ctx context.Context, run Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !issueFilePattern.MatchString(run.Issue) {
		return errors.New("coordinator: invalid issue id")
	}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.Path(run.Issue), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return ErrAlreadyRunning
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}
