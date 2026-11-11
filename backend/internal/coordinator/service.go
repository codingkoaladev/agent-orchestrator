package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

// TicketState is the coordinator's view of one ticket.
type TicketState struct {
	Facts  youtrack.TicketFacts
	Fields map[string]string
}

// Ticket is the YouTrack side of a run. Apply receives the same facts the
// guards validate against, so a transition can never be built from a caller's
// claim about the ticket.
type Ticket interface {
	Read(ctx context.Context, issue string) (TicketState, error)
	Apply(ctx context.Context, stage youtrack.Stage, facts youtrack.Facts, review *youtrack.ReviewEvidence, prelive *youtrack.PreliveEvidence) ([]string, error)
}

// SpawnRequest asks the daemon to start or adopt a worker.
type SpawnRequest struct {
	Project      string
	Issue        string
	Branch       string
	Model        string
	Effort       string
	Subscription string
	Prompt       string
	DisplayName  string
}

// SessionStatus is one daemon session.
type SessionStatus struct {
	ID           string
	NativeID     string
	Branch       string
	WorktreePath string
	Status       string
	Activity     string
	Terminated   bool
}

// Sessions is the Orchestrator daemon side of a run.
type Sessions interface {
	Spawn(ctx context.Context, request SpawnRequest) (Session, error)
	List(ctx context.Context) ([]youtrack.SessionFact, error)
	Get(ctx context.Context, id string) (SessionStatus, error)
	Send(ctx context.Context, id, text string) error
}

// ReviewRequest is one independent review of an exact commit.
type ReviewRequest struct {
	Issue    string
	Commit   string
	Worktree string
	Model    string
	Effort   string
}

// ReviewResult is what the reviewer actually reported.
type ReviewResult struct {
	Verdict  string
	Session  string
	Model    string
	Effort   string
	Findings string
	Source   string
}

// Reviewer runs the independent reviewer outside the coding session.
type Reviewer interface {
	Review(ctx context.Context, request ReviewRequest) (ReviewResult, error)
}

// SubmissionDetector answers "which commit is ready for review?" from the
// checkout itself, so waiting burns no model tokens.
type SubmissionDetector interface {
	Detect(ctx context.Context, worktree, base string) (string, error)
}

// PreliveRunner executes the pre-live check and records its result.
type PreliveRunner interface {
	Run(ctx context.Context, worktree, commit, command string) (Prelive, error)
}

// Service owns the lifecycle. Every dependency is a port so the whole loop is
// exercised with synthetic agents, trackers and checkouts in tests.
type Service struct {
	Tickets       Ticket
	Sessions      Sessions
	Reviewer      Reviewer
	Detector      SubmissionDetector
	PreliveRunner PreliveRunner
	Store         Store
	PollEvery     time.Duration
	WaitFor       time.Duration
	MaxRounds     int
	Now           func() time.Time
	Log           func(event string, fields map[string]string)
}

// StartRequest is the owner's authorization to work one ticket.
type StartRequest struct {
	Issue        string
	Project      string
	Branch       string
	Model        string
	Effort       string
	Subscription string
	Prompt       string
	// Approved states that the owner authorized this dispatch. The ticket's AI
	// Automation Enabled value is advisory until its bundle is populated.
	Approved bool
}

// ErrAlreadyRunning reports an active run for the ticket.
var ErrAlreadyRunning = errors.New("coordinator: ticket already has an active run")

// ErrNotApproved reports a missing dispatch authorization.
// ErrNotDispatched identifies failures proven to occur before any launch request.
var ErrNotDispatched = errors.New("coordinator: no worker was dispatched")

var ErrNotApproved = errors.New("coordinator: dispatch is not approved for this ticket")

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) log(event string, fields map[string]string) {
	if s.Log != nil {
		s.Log(event, fields)
	}
}

// Start begins a run or reports the existing one. It fails closed when the
// daemon cannot answer whether the recorded session is still alive.
func (s *Service) Start(ctx context.Context, request StartRequest) (Run, error) {
	if !request.Approved {
		return Run{}, ErrNotApproved
	}
	ticket, err := s.Tickets.Read(ctx, request.Issue)
	if err != nil {
		return Run{}, fmt.Errorf("read ticket: %w", err)
	}
	if ticket.Facts.State == youtrack.StateForRelease {
		return Run{}, errors.New("ticket is already For Release; a new run needs an owner-authorized state change")
	}

	sessions, err := s.Sessions.List(ctx)
	if err != nil {
		// Liveness is unknown. Never dispatch on a guess.
		return Run{}, fmt.Errorf("session liveness unknown: %w", err)
	}

	existing, err := s.Store.Load(request.Issue)
	switch {
	case err == nil:
		if existing.Phase.Active() {
			if existing.Phase == PhaseDispatching {
				if existing.Coder.OrchestratorID == "" {
					return existing, errors.New("dispatch outcome requires reconciliation; refusing another worker")
				}
				facts, err := s.factsFor(ctx, request.Issue, ticket, existing.Coder, sessions)
				if err != nil {
					return existing, err
				}
				if _, err = s.Tickets.Apply(ctx, youtrack.StageStart, facts, nil, nil); err != nil {
					return existing, fmt.Errorf("record existing worker on ticket: %w", err)
				}
				existing.Coder.NativeID = facts.CoderSessionID
				existing.Phase = PhaseCoding
				return existing, s.Store.Save(existing)
			}
			if liveSession(sessions, existing.Coder.OrchestratorID) {
				return existing, fmt.Errorf("%w: %s is %s", ErrAlreadyRunning, existing.Issue, existing.Phase)
			}
			// The recorded worker is gone: reconcile instead of dispatching a
			// second one, and let the review loop continue on the same run.
			existing.Phase = PhaseAwaitingReview
			existing.Note = "recorded session is no longer live; resumed without a new dispatch"
			if err := s.Store.Save(existing); err != nil {
				return Run{}, err
			}
			return existing, nil
		}
		// Preserve terminal runs for audit; replacement requires explicit archival.
		return existing, errors.New("ticket already has a recorded run; archive it explicitly before a new dispatch")
	case !errors.Is(err, ErrNoRun):
		return Run{}, err
	}

	if blocked, err := blockingDispatch(ticket, sessions, request); err != nil {
		return Run{}, err
	} else if blocked != "" {
		return Run{}, errors.New(blocked)
	}

	run := Run{Issue: request.Issue, Project: request.Project, Phase: PhaseDispatching, UpdatedAt: s.now()}
	if err := s.Store.Claim(ctx, run); err != nil {
		return Run{}, err
	}
	coder, err := s.Sessions.Spawn(ctx, SpawnRequest{
		Project:      request.Project,
		Issue:        request.Issue,
		Branch:       request.Branch,
		Model:        request.Model,
		Effort:       request.Effort,
		Subscription: request.Subscription,
		Prompt:       request.Prompt,
		DisplayName:  request.Issue + " worker",
	})
	if err != nil {
		if errors.Is(err, ErrNotDispatched) {
			if releaseErr := os.Remove(s.Store.Path(run.Issue)); releaseErr != nil {
				return run, fmt.Errorf("spawn rejected: %w; release claim: %v", err, releaseErr)
			}
		}
		return Run{}, fmt.Errorf("spawn coder: %w", err)
	}
	run.Coder = coder
	if err := s.Store.Save(run); err != nil {
		return run, err
	}
	facts, err := s.factsFor(ctx, request.Issue, ticket, coder, sessions)
	if err != nil {
		return Run{}, err
	}
	if _, err := s.Tickets.Apply(ctx, youtrack.StageStart, facts, nil, nil); err != nil {
		return Run{}, fmt.Errorf("record start on ticket: %w", err)
	}
	run.Phase = PhaseCoding
	run.Coder.NativeID = facts.CoderSessionID
	if err := s.Store.Save(run); err != nil {
		return Run{}, err
	}
	s.log("run_started", map[string]string{"issue": run.Issue, "session": coder.OrchestratorID})
	return run, nil
}

// blockingDispatch refuses a dispatch while the ticket records a live session,
// including one this run record does not know about.
func blockingDispatch(ticket TicketState, sessions []youtrack.SessionFact, request StartRequest) (string, error) {
	recorded := strings.TrimSpace(ticket.Facts.RecordedSessionID)
	if recorded == "" {
		return "", nil
	}
	for _, session := range sessions {
		if (session.ID == recorded || session.NativeID == recorded) && session.Live {
			return fmt.Sprintf("ticket records live session %s; refusing another dispatch", recorded), nil
		}
	}
	return "", nil
}

func liveSession(sessions []youtrack.SessionFact, id string) bool {
	for _, session := range sessions {
		if session.ID == id && session.Live {
			return true
		}
	}
	return false
}

// factsFor assembles the guard facts from authoritative sources.
func (s *Service) factsFor(ctx context.Context, issue string, ticket TicketState, coder Session, sessions []youtrack.SessionFact) (youtrack.Facts, error) {
	status, err := s.Sessions.Get(ctx, coder.OrchestratorID)
	if err != nil {
		return youtrack.Facts{}, fmt.Errorf("read coder session: %w", err)
	}
	native := coder.NativeID
	if native == "" {
		native = status.NativeID
	}
	return youtrack.Facts{
		IssueID:               issue,
		CoderSessionID:        native,
		OrchestratorSessionID: coder.OrchestratorID,
		Branch:                firstNonEmpty(coder.Branch, status.Branch),
		WorktreePath:          firstNonEmpty(coder.WorktreePath, status.WorktreePath),
		CoderModel:            coder.Model,
		Ticket:                ticket.Facts,
		Sessions:              sessions,
		LivenessKnown:         true,
	}, nil
}

// AwaitSubmission waits for a new commit in the run's worktree. It polls the
// checkout only; no model turn is spent while waiting.
func (s *Service) AwaitSubmission(ctx context.Context, issue, base string) (string, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return "", err
	}
	deadline := s.now().Add(s.WaitFor)
	for {
		commit, err := s.Detector.Detect(ctx, run.Coder.WorktreePath, base)
		if err == nil && commit != "" {
			run.SubmittedCommit = commit
			run.Phase = PhaseAwaitingReview
			if err := s.Store.Save(run); err != nil {
				return "", err
			}
			return commit, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if s.now().After(deadline) {
			return "", fmt.Errorf("no submission appeared in %s within %s", run.Coder.WorktreePath, s.WaitFor)
		}
		if err := sleep(ctx, s.PollEvery); err != nil {
			return "", err
		}
	}
}

// Review reviews the run's submitted commit and, on failure, returns the
// findings to the same coder and waits for a corrected commit.
func (s *Service) Review(ctx context.Context, issue string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	if strings.TrimSpace(run.SubmittedCommit) == "" {
		return Run{}, errors.New("run has no submitted commit to review")
	}
	rounds := s.MaxRounds
	if rounds <= 0 {
		rounds = 3
	}
	for attempt := 0; attempt < rounds; attempt++ {
		ticket, err := s.Tickets.Read(ctx, issue)
		if err != nil {
			return Run{}, fmt.Errorf("read ticket: %w", err)
		}
		if ticket.Facts.SubmittedCommit != run.SubmittedCommit {
			return Run{}, fmt.Errorf("ticket submitted commit %q does not match run commit %q", ticket.Facts.SubmittedCommit, run.SubmittedCommit)
		}
		sessions, err := s.Sessions.List(ctx)
		if err != nil {
			return Run{}, fmt.Errorf("session liveness unknown: %w", err)
		}
		facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
		if err != nil {
			return Run{}, err
		}
		facts.Commit = run.SubmittedCommit

		run.Phase = PhaseReviewing
		run.ReviewRounds = attempt + 1
		if err := s.Store.Save(run); err != nil {
			return Run{}, err
		}
		result, err := s.Reviewer.Review(ctx, ReviewRequest{
			Issue:    issue,
			Commit:   run.SubmittedCommit,
			Worktree: run.Coder.WorktreePath,
			Model:    run.Coder.Model,
			Effort:   run.Coder.Effort,
		})
		if err != nil {
			run.Phase = PhaseBlocked
			run.Note = "reviewer failed: " + err.Error()
			_ = s.Store.Save(run)
			return run, fmt.Errorf("run reviewer: %w", err)
		}
		review := Review{
			Commit:   run.SubmittedCommit,
			Verdict:  result.Verdict,
			Session:  result.Session,
			Model:    result.Model,
			Effort:   result.Effort,
			Source:   result.Source,
			Findings: result.Findings,
			At:       s.now(),
		}
		run.AddReview(review)
		if _, err := s.Tickets.Apply(ctx, youtrack.StageReview, facts, ptr(review.ReviewEvidence()), nil); err != nil {
			run.Phase = PhaseBlocked
			run.Note = "review transition failed: " + err.Error()
			_ = s.Store.Save(run)
			return run, fmt.Errorf("record review: %w", err)
		}
		if review.Verdict == youtrack.VerdictPass {
			run.Phase = PhaseAwaitingPrelive
			run.Note = ""
			if err := s.Store.Save(run); err != nil {
				return Run{}, err
			}
			return run, nil
		}
		if err := s.Sessions.Send(ctx, run.Coder.OrchestratorID, correctionMessage(review)); err != nil {
			run.Phase = PhaseBlocked
			run.Note = "could not return findings to the coder: " + err.Error()
			_ = s.Store.Save(run)
			return run, err
		}
		run.Phase = PhaseCorrecting
		if err := s.Store.Save(run); err != nil {
			return Run{}, err
		}
		next, err := s.AwaitSubmission(ctx, issue, run.SubmittedCommit)
		if err != nil {
			run.Phase = PhaseBlocked
			run.Note = "no corrected commit: " + err.Error()
			_ = s.Store.Save(run)
			return run, err
		}
		run.SubmittedCommit = next
		if err := s.Store.Save(run); err != nil {
			return Run{}, err
		}
		// The corrected commit must be submitted to the ticket before it can be
		// reviewed; the next loop iteration re-reads the ticket and refuses a
		// mismatch.
		if err := s.submitCorrected(ctx, issue, run); err != nil {
			run.Phase = PhaseBlocked
			run.Note = err.Error()
			_ = s.Store.Save(run)
			return run, err
		}
	}
	run.Phase = PhaseBlocked
	run.Note = fmt.Sprintf("review did not pass after %d rounds", rounds)
	_ = s.Store.Save(run)
	return run, errors.New(run.Note)
}

func (s *Service) submitCorrected(ctx context.Context, issue string, run Run) error {
	ticket, err := s.Tickets.Read(ctx, issue)
	if err != nil {
		return err
	}
	sessions, err := s.Sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("session liveness unknown: %w", err)
	}
	facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
	if err != nil {
		return err
	}
	facts.Commit = run.SubmittedCommit
	_, err = s.Tickets.Apply(ctx, youtrack.StageSubmit, facts, nil, nil)
	return err
}

func correctionMessage(review Review) string {
	message := "Independent review of commit " + review.Commit + " failed. Fix these findings, then commit the corrections:\n\n" + review.Findings
	return strings.TrimSpace(message)
}

// Prelive runs the configured pre-live check and only then claims release
// readiness. Without a configured environment this must not be called.
func (s *Service) Prelive(ctx context.Context, issue, command string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	if run.Phase != PhaseAwaitingPrelive {
		return Run{}, fmt.Errorf("run is %s, not ready for pre-live acceptance", run.Phase)
	}
	ticket, err := s.Tickets.Read(ctx, issue)
	if err != nil {
		return Run{}, err
	}
	if ticket.Facts.LastReviewedCommit != run.SubmittedCommit {
		return Run{}, fmt.Errorf("ticket reviewed commit %q is not the submitted commit %q", ticket.Facts.LastReviewedCommit, run.SubmittedCommit)
	}
	record, err := s.PreliveRunner.Run(ctx, run.Coder.WorktreePath, run.SubmittedCommit, command)
	if err != nil {
		run.Phase = PhaseBlocked
		run.Note = "pre-live check failed: " + err.Error()
		_ = s.Store.Save(run)
		return run, err
	}
	run.PreliveCheck = &record
	sessions, err := s.Sessions.List(ctx)
	if err != nil {
		return Run{}, fmt.Errorf("session liveness unknown: %w", err)
	}
	facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
	if err != nil {
		return Run{}, err
	}
	facts.Commit = run.SubmittedCommit
	review, ok := run.PassingReview(run.SubmittedCommit)
	if !ok {
		return Run{}, errors.New("no recorded passing review for the submitted commit")
	}
	if _, err := s.Tickets.Apply(ctx, youtrack.StagePrelive, facts, ptr(review.ReviewEvidence()), ptr(record.Evidence())); err != nil {
		run.Phase = PhaseBlocked
		run.Note = "pre-live transition failed: " + err.Error()
		_ = s.Store.Save(run)
		return run, err
	}
	run.Phase = PhasePreliveVerified
	if err := s.Store.Save(run); err != nil {
		return Run{}, err
	}
	return run, nil
}

// Status reports the durable run without touching the daemon.
func (s *Service) Status(issue string) (Run, error) { return s.Store.Load(issue) }

func ptr[T any](value T) *T { return &value }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		d = 50 * time.Millisecond
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
