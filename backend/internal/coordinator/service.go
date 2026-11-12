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
	Facts       youtrack.TicketFacts
	Fields      map[string]string
	Summary     string
	Description string
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
	ID                     string
	NativeID               string
	ProviderConversationID string
	Harness                string
	Mode                   string
	Branch                 string
	WorktreePath           string
	Model                  string
	Status                 string
	Activity               string
	Terminated             bool
}

// SendRequest is one message to the coder. ClientMessageID makes a retry after
// a crash idempotent at the daemon.
type SendRequest struct {
	Text            string
	ClientMessageID string
}

// Sessions is the Orchestrator daemon side of a run.
type Sessions interface {
	Spawn(ctx context.Context, request SpawnRequest) (Session, error)
	List(ctx context.Context) ([]youtrack.SessionFact, error)
	Get(ctx context.Context, id string) (SessionStatus, error)
	Send(ctx context.Context, id string, message SendRequest) error
}

// ReviewRequest is one independent review of an exact commit.
type ReviewRequest struct {
	Issue   string
	Summary string
	// Acceptance is the ticket's full description, so the reviewer sees the
	// contract rather than only the commit.
	Acceptance string
	Commit     string
	Worktree   string
	Model      string
	Effort     string
	// IsolatedSnapshot requires the reviewer to materialize the exact commit
	// outside the worker's possibly-dirty worktree. A reviewer that cannot do so
	// must fail instead of reviewing local environment changes.
	IsolatedSnapshot bool
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
	Checkout      Checkout
	PullRequests  PullRequests
	PreliveRunner PreliveRunner
	Store         Store
	PollEvery     time.Duration
	WaitFor       time.Duration
	// ConfirmFor is how long one observed YouTrack submission may keep failing
	// worktree/PR/session verification before the run blocks. That absorbs push
	// and SCM propagation delay without accepting a mismatch indefinitely.
	ConfirmFor     time.Duration
	IdentityWait   time.Duration
	MaxRounds      int
	PreliveCommand string
	ReviewerModel  string
	ReviewerEffort string
	Now            func() time.Time
	Log            func(event string, fields map[string]string)
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

// ErrAlreadyRunning reports an active run or supervisor for the ticket.
var ErrAlreadyRunning = errors.New("coordinator: ticket already has an active run")

// ErrNotDispatched identifies failures proven to occur before any launch request.
var ErrNotDispatched = errors.New("coordinator: no worker was dispatched")

// ErrNotApproved reports a missing dispatch authorization.
var ErrNotApproved = errors.New("coordinator: dispatch is not approved for this ticket")

// ErrNotEligible reports that a scheduled review found no reviewable
// submission. It is a skip, not a failed review.
var ErrNotEligible = errors.New("coordinator: ticket is not eligible for a scheduled review")

// requireApproved keeps automatic work limited to runs the owner authorized.
// It returns an error without mutating the run so `start --approved` can still
// adopt a pre-existing active dispatch.
func requireApproved(run Run) error {
	if run.Approved {
		return nil
	}
	return errors.New("run has no recorded owner approval; use `start --approved` to authorize the existing dispatch")
}

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

func (s *Service) pollEvery() time.Duration {
	if s.PollEvery > 0 {
		return s.PollEvery
	}
	return time.Second
}

func (s *Service) confirmFor() time.Duration {
	if s.ConfirmFor > 0 {
		return s.ConfirmFor
	}
	return 30 * time.Second
}

func (s *Service) identityWait() time.Duration {
	if s.IdentityWait > 0 {
		return s.IdentityWait
	}
	return 30 * time.Second
}

func (s *Service) maxRounds() int {
	if s.MaxRounds > 0 {
		return s.MaxRounds
	}
	return 3
}

// Start dispatches or adopts a worker and returns immediately. Use
// StartAndFollow to keep supervising the run.
func (s *Service) Start(ctx context.Context, request StartRequest) (Run, error) {
	if !request.Approved {
		return Run{}, ErrNotApproved
	}
	lock, err := s.Store.Lock(ctx, request.Issue)
	if err != nil {
		return Run{}, err
	}
	defer lock.Release()
	return s.startLocked(ctx, request)
}

// StartAndFollow is the automatic entry point: it dispatches only when needed,
// then holds the single-owner lock while the lifecycle advances.
func (s *Service) StartAndFollow(ctx context.Context, request StartRequest) (Run, error) {
	if !request.Approved {
		return Run{}, ErrNotApproved
	}
	lock, err := s.Store.Lock(ctx, request.Issue)
	if err != nil {
		return Run{}, err
	}
	defer lock.Release()
	run, err := s.startLocked(ctx, request)
	if err != nil && !errors.Is(err, ErrAlreadyRunning) {
		return run, err
	}
	return s.superviseLocked(ctx, request.Issue)
}

func (s *Service) startLocked(ctx context.Context, request StartRequest) (Run, error) {
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
			if request.Approved && !existing.Approved {
				existing.Approved = true
				if err := s.Store.Save(existing); err != nil {
					return Run{}, err
				}
			}
			if existing.Phase == PhaseDispatching {
				if existing.Coder.OrchestratorID == "" {
					return existing, errors.New("dispatch outcome requires reconciliation; refusing another worker")
				}
				facts, err := s.factsFor(ctx, request.Issue, ticket, existing.Coder, sessions)
				if err != nil {
					existing.Note = "worker is recorded but its identity/worktree is not yet verified: " + err.Error()
					_ = s.Store.Save(existing)
					return existing, err
				}
				if ticket.Facts.AIStatus == youtrack.AIStatusAwaiting && commitPattern.MatchString(strings.TrimSpace(ticket.Facts.SubmittedCommit)) {
					// The coder submitted while start recording was still in flight.
					// Adopt the worker without resetting the ticket's review signal.
					existing.Coder.NativeID = facts.CoderSessionID
					existing.Coder.WorktreePath = firstNonEmpty(existing.Coder.WorktreePath, facts.WorktreePath)
					existing.SubmittedCommit = ticket.Facts.SubmittedCommit
					existing.Phase = PhaseAwaitingReview
					existing.Note = "worker had already submitted before start recording; adopted without resetting the ticket"
					return existing, s.Store.Save(existing)
				}
				if err := s.applyAndVerify(ctx, youtrack.StageStart, facts, nil, nil); err != nil {
					return existing, fmt.Errorf("record existing worker on ticket: %w", err)
				}
				existing.Coder.NativeID = facts.CoderSessionID
				existing.Phase = PhaseCoding
				return existing, s.Store.Save(existing)
			}
			if liveSession(sessions, existing.Coder.OrchestratorID) {
				return existing, fmt.Errorf("%w: %s is %s", ErrAlreadyRunning, existing.Issue, existing.Phase)
			}
			if strings.TrimSpace(existing.SubmittedCommit) == "" {
				existing.Phase = PhaseBlocked
				existing.Note = "recorded coder session is gone before any ticket submission; refusing to restart unknown work"
				_ = s.Store.Save(existing)
				return existing, errors.New(existing.Note)
			}
			// A submitted commit survives the coder process. Resume review
			// against the ticket and PR instead of dispatching a second worker.
			existing.Phase = PhaseAwaitingReview
			existing.Note = "recorded session is no longer live; resuming review without a new dispatch"
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

	run := Run{Issue: request.Issue, Project: request.Project, Phase: PhaseDispatching, Approved: true, UpdatedAt: s.now()}
	if err := s.Store.Claim(ctx, run); err != nil {
		return Run{}, err
	}
	coder, spawnErr := s.Sessions.Spawn(ctx, SpawnRequest{
		Project:      request.Project,
		Issue:        request.Issue,
		Branch:       request.Branch,
		Model:        request.Model,
		Effort:       request.Effort,
		Subscription: request.Subscription,
		Prompt:       request.Prompt,
		DisplayName:  request.Issue + " worker",
	})
	if coder.OrchestratorID != "" {
		// Persist a worker the daemon actually created even if a later identity or
		// worktree lookup failed. Losing it would make reconciliation impossible.
		run.Coder = coder
		if saveErr := s.Store.Save(run); saveErr != nil {
			return run, saveErr
		}
	}
	if spawnErr != nil {
		if errors.Is(spawnErr, ErrNotDispatched) && coder.OrchestratorID == "" {
			if releaseErr := os.Remove(s.Store.Path(run.Issue)); releaseErr != nil {
				return run, fmt.Errorf("spawn rejected: %w; release claim: %v", spawnErr, releaseErr)
			}
		}
		return run, fmt.Errorf("spawn coder: %w", spawnErr)
	}
	run.Coder = coder
	if err := s.Store.Save(run); err != nil {
		return run, err
	}
	facts, err := s.factsFor(ctx, request.Issue, ticket, coder, sessions)
	if err != nil {
		return run, err
	}
	if err := s.applyAndVerify(ctx, youtrack.StageStart, facts, nil, nil); err != nil {
		return run, fmt.Errorf("record start on ticket: %w", err)
	}
	run.Phase = PhaseCoding
	run.Coder.NativeID = facts.CoderSessionID
	if err := s.Store.Save(run); err != nil {
		return run, err
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

// factsFor assembles the guard facts from authoritative sources and resolves
// the chat-native identity the daemon may establish just after spawn.
func (s *Service) factsFor(ctx context.Context, issue string, ticket TicketState, coder Session, sessions []youtrack.SessionFact) (youtrack.Facts, error) {
	status, err := s.statusWithIdentity(ctx, coder.OrchestratorID)
	if err != nil {
		return youtrack.Facts{}, fmt.Errorf("read coder session: %w", err)
	}
	native := firstNonEmpty(status.NativeID, coder.NativeID)
	if native == "" {
		return youtrack.Facts{}, errors.New("coder session has no verified native identity yet")
	}
	if coder.NativeID != "" && status.NativeID != "" && coder.NativeID != status.NativeID {
		return youtrack.Facts{}, fmt.Errorf("coder session identity changed from %q to %q", coder.NativeID, status.NativeID)
	}
	if status.WorktreePath != "" && coder.WorktreePath != "" && status.WorktreePath != coder.WorktreePath {
		return youtrack.Facts{}, fmt.Errorf("coder worktree changed from %q to %q", coder.WorktreePath, status.WorktreePath)
	}
	return youtrack.Facts{
		IssueID:               issue,
		CoderSessionID:        native,
		OrchestratorSessionID: coder.OrchestratorID,
		Branch:                firstNonEmpty(coder.Branch, status.Branch),
		WorktreePath:          firstNonEmpty(coder.WorktreePath, status.WorktreePath),
		CoderModel:            firstNonEmpty(coder.Model, status.Model),
		Ticket:                ticket.Facts,
		Sessions:              sessions,
		LivenessKnown:         true,
	}, nil
}

func (s *Service) statusWithIdentity(ctx context.Context, id string) (SessionStatus, error) {
	deadline := s.now().Add(s.identityWait())
	for {
		status, err := s.Sessions.Get(ctx, id)
		if err != nil {
			return SessionStatus{}, err
		}
		if strings.TrimSpace(status.NativeID) != "" {
			return status, nil
		}
		if !s.now().Before(deadline) {
			return status, nil
		}
		if err := sleep(ctx, s.pollEvery()); err != nil {
			return SessionStatus{}, err
		}
	}
}

// AwaitSubmission waits for the ticket to say Awaiting AI Review and for the
// exact commit it names to be proven by the coder, checkout and pull request. A
// new local commit alone is never a submission.
func (s *Service) AwaitSubmission(ctx context.Context, issue string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	var deadline time.Time
	if s.WaitFor > 0 {
		deadline = s.now().Add(s.WaitFor)
	}
	var confirmUntil time.Time
	lastReason := "no ticket submission yet"
	for {
		if ctx.Err() != nil {
			return run, ctx.Err()
		}
		if !deadline.IsZero() && s.now().After(deadline) {
			return s.block(issue, "no verified submission appeared within "+s.WaitFor.String()+": "+lastReason)
		}
		ticket, err := s.Tickets.Read(ctx, issue)
		if err != nil {
			lastReason = "read ticket: " + err.Error()
			if err := sleep(ctx, s.pollEvery()); err != nil {
				return run, err
			}
			continue
		}
		if ticket.Facts.State == youtrack.StateForRelease || ticket.Facts.State == "Done" {
			return s.block(issue, "ticket moved to "+ticket.Facts.State+" before an independent review; refusing to continue")
		}
		if ticket.Facts.AIStatus != youtrack.AIStatusAwaiting {
			lastReason = "ticket AI Status is " + ticket.Facts.AIStatus + ", not " + youtrack.AIStatusAwaiting
			confirmUntil = time.Time{}
			if err := sleep(ctx, s.pollEvery()); err != nil {
				return run, err
			}
			continue
		}
		commit := strings.TrimSpace(ticket.Facts.SubmittedCommit)
		if !commitPattern.MatchString(commit) {
			return s.block(issue, fmt.Sprintf("ticket reports malformed submitted commit %q", commit))
		}
		if _, ok := run.LatestReview(commit); ok {
			lastReason = "commit " + commit + " was already reviewed; waiting for a new submission"
			confirmUntil = time.Time{}
			if err := sleep(ctx, s.pollEvery()); err != nil {
				return run, err
			}
			continue
		}
		if confirmUntil.IsZero() {
			confirmUntil = s.now().Add(s.confirmFor())
		}
		check := s.verifySubmission(ctx, run, ticket, commit)
		if check.err != nil {
			return s.block(issue, check.err.Error())
		}
		if check.ready {
			run.SubmittedCommit = commit
			run.Phase = PhaseAwaitingReview
			run.Note = firstNonEmpty(check.reason, "submission verified against ticket, checkout and pull request")
			if err := s.Store.Save(run); err != nil {
				return Run{}, err
			}
			s.log("submission_verified", map[string]string{"issue": issue, "commit": commit})
			return run, nil
		}
		lastReason = check.reason
		if !check.waitForCoder && s.now().After(confirmUntil) {
			return s.block(issue, "submission not verified: "+check.reason)
		}
		if err := sleep(ctx, s.pollEvery()); err != nil {
			return run, err
		}
	}
}

// submissionCheck is the result of verifying one observed ticket submission.
type submissionCheck struct {
	reason       string
	ready        bool
	waitForCoder bool
	err          error
}

// verifySubmission checks one observed ticket submission. It returns ready when
// the ticket, coder, checkout and pull request all prove the same commit. A
// coder still working sets waitForCoder so the caller does not time it out.
func (s *Service) verifySubmission(ctx context.Context, run Run, ticket TicketState, commit string) submissionCheck {
	if ticket.Facts.State == youtrack.StateForRelease || ticket.Facts.State == "Done" {
		return submissionCheck{err: fmt.Errorf("ticket is %s before an independent review; refusing to continue", ticket.Facts.State)}
	}
	if strings.TrimSpace(ticket.Facts.SubmittedCommit) != commit {
		return submissionCheck{err: fmt.Errorf("ticket records submitted commit %q, not %q", ticket.Facts.SubmittedCommit, commit)}
	}
	recorded := strings.TrimSpace(ticket.Facts.RecordedSessionID)
	if recorded == "" {
		return submissionCheck{err: errors.New("ticket has no recorded coding session id")}
	}
	if recorded != run.Coder.NativeID && recorded != run.Coder.OrchestratorID {
		return submissionCheck{err: fmt.Errorf("ticket records coding session %q, not this run's worker %q/%q", recorded, run.Coder.OrchestratorID, run.Coder.NativeID)}
	}
	if ticket.Facts.Branch != "" && ticket.Facts.Branch != run.Coder.Branch {
		return submissionCheck{err: fmt.Errorf("ticket branch %q is not the intended branch %q", ticket.Facts.Branch, run.Coder.Branch)}
	}
	if ticket.Facts.WorktreePath != "" && ticket.Facts.WorktreePath != run.Coder.WorktreePath {
		return submissionCheck{err: fmt.Errorf("ticket worktree %q is not the registered worktree %q", ticket.Facts.WorktreePath, run.Coder.WorktreePath)}
	}
	if ticket.Facts.CoderModel != "" && run.Coder.Model != "" && !sameModel(ticket.Facts.CoderModel, run.Coder.Model) {
		return submissionCheck{err: fmt.Errorf("ticket coder model %q is not this run's model %q", ticket.Facts.CoderModel, run.Coder.Model)}
	}
	status, err := s.Sessions.Get(ctx, run.Coder.OrchestratorID)
	if err != nil {
		return submissionCheck{err: fmt.Errorf("session liveness unknown: %w", err)}
	}
	if status.NativeID != "" && status.NativeID != run.Coder.NativeID {
		return submissionCheck{err: fmt.Errorf("coder native identity changed from %q to %q", run.Coder.NativeID, status.NativeID)}
	}
	switch {
	case status.Terminated:
		// A finished worker may still have submitted a commit.
	case status.Activity == "idle" || status.Activity == "exited":
		// Finished for this submission.
	case status.Activity == "active":
		return submissionCheck{reason: "coder activity is active; waiting for it to finish", waitForCoder: true}
	default:
		return submissionCheck{reason: "coder activity is " + firstNonEmpty(status.Activity, "unknown") + "; not a finished state"}
	}
	checkout, err := s.checkout().Validate(ctx, run.Coder.WorktreePath, run.Coder.Branch, commit)
	if err != nil {
		return submissionCheck{reason: err.Error()}
	}
	pr, err := s.pullRequests().Find(ctx, run.Coder.WorktreePath, run.Coder.Branch)
	if err != nil {
		return submissionCheck{reason: err.Error()}
	}
	if pr.HeadRef != run.Coder.Branch {
		return submissionCheck{reason: fmt.Sprintf("pull request head %q is not the intended branch %q", pr.HeadRef, run.Coder.Branch)}
	}
	if pr.HeadCommit != commit {
		return submissionCheck{reason: fmt.Sprintf("pull request head %s is not the submitted commit %s", pr.HeadCommit, commit)}
	}
	note := "submission verified against ticket, checkout and pull request"
	if len(checkout.Unstaged)+len(checkout.Untracked) > 0 {
		note += fmt.Sprintf("; %d local uncommitted/environment file(s) excluded from review", len(checkout.Unstaged)+len(checkout.Untracked))
	}
	return submissionCheck{reason: note, ready: true}
}

func (s *Service) checkout() Checkout {
	if s.Checkout != nil {
		return s.Checkout
	}
	return GitCheckout{}
}

func (s *Service) pullRequests() PullRequests {
	if s.PullRequests != nil {
		return s.PullRequests
	}
	return GHPullRequests{}
}

func (s *Service) block(issue, note string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	run.Phase = PhaseBlocked
	run.Note = note
	_ = s.Store.Save(run)
	return run, errors.New(note)
}

// Supervise keeps one owner on the ticket and advances the durable run until it
// reaches pre-live readiness, blocks, or the context ends. It never dispatches a
// second worker and never repeats a review whose process may have started.
func (s *Service) Supervise(ctx context.Context, issue string) (Run, error) {
	lock, err := s.Store.Lock(ctx, issue)
	if err != nil {
		return Run{}, err
	}
	defer lock.Release()
	return s.superviseLocked(ctx, issue)
}

func (s *Service) superviseLocked(ctx context.Context, issue string) (Run, error) {
	for {
		run, err := s.Store.Load(issue)
		if err != nil {
			return Run{}, err
		}
		if err := requireApproved(run); err != nil {
			return run, err
		}
		switch run.Phase {
		case PhaseDispatching:
			run, err = s.reconcileDispatch(ctx, issue)
			if err != nil {
				return run, err
			}
			continue
		case PhaseCoding, PhaseCorrecting:
			if run.Phase == PhaseCorrecting {
				if err := s.ensureCorrection(ctx, &run); err != nil {
					return s.block(issue, err.Error())
				}
			}
			run, err = s.AwaitSubmission(ctx, issue)
			if err != nil {
				return run, err
			}
		case PhaseAwaitingReview, PhaseReviewing:
			run, err = s.reviewLocked(ctx, issue)
			if err != nil {
				return run, err
			}
		case PhaseAwaitingPrelive:
			if strings.TrimSpace(s.PreliveCommand) == "" {
				run.Note = "review passed; stopped because no pre-live deployment/configuration is available. Production and AI completion remain untouched."
				_ = s.Store.Save(run)
				return run, nil
			}
			return s.preliveLocked(ctx, issue, s.PreliveCommand)
		default:
			return run, nil
		}
	}
}

func (s *Service) reconcileDispatch(ctx context.Context, issue string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	if run.Coder.OrchestratorID == "" {
		return s.block(issue, "dispatch outcome is ambiguous; inspect the daemon and reconcile the claim without starting another worker")
	}
	ticket, err := s.Tickets.Read(ctx, issue)
	if err != nil {
		return run, err
	}
	sessions, err := s.Sessions.List(ctx)
	if err != nil {
		return run, fmt.Errorf("session liveness unknown: %w", err)
	}
	if !liveSession(sessions, run.Coder.OrchestratorID) {
		if strings.TrimSpace(run.SubmittedCommit) == "" {
			return s.block(issue, "recorded coder session is gone before submission; refusing to restart unknown work")
		}
		run.Phase = PhaseAwaitingReview
		run.Note = "recorded session is gone; resuming the submitted commit without a new dispatch"
		return run, s.Store.Save(run)
	}
	facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
	if err != nil {
		return run, err
	}
	if ticket.Facts.AIStatus == youtrack.AIStatusAwaiting && commitPattern.MatchString(strings.TrimSpace(ticket.Facts.SubmittedCommit)) {
		run.Coder.NativeID = facts.CoderSessionID
		run.Coder.WorktreePath = firstNonEmpty(run.Coder.WorktreePath, facts.WorktreePath)
		run.SubmittedCommit = ticket.Facts.SubmittedCommit
		run.Phase = PhaseAwaitingReview
		run.Note = "worker had already submitted while dispatch was being recorded; adopted without resetting the ticket"
		return run, s.Store.Save(run)
	}
	if err := s.applyAndVerify(ctx, youtrack.StageStart, facts, nil, nil); err != nil {
		return run, err
	}
	run.Coder.NativeID = facts.CoderSessionID
	run.Phase = PhaseCoding
	return run, s.Store.Save(run)
}

// Review verifies the ticket submission and runs the bounded review/correction
// loop. It holds the single-owner lock for the whole loop.
func (s *Service) Review(ctx context.Context, issue string) (Run, error) {
	lock, err := s.Store.Lock(ctx, issue)
	if err != nil {
		return Run{}, err
	}
	defer lock.Release()
	return s.reviewLocked(ctx, issue)
}

// ScheduledReviewRequest carries the scheduler's window and clock into the
// single-review transaction, so a request cleared or advanced between the sweep
// prefilter and the review cannot launch stale work.
type ScheduledReviewRequest struct {
	Window   time.Duration
	Location *time.Location
	Now      time.Time
}

// ReviewOnce performs at most one scheduled review of the current submission.
// It never waits for corrections; the next scheduled request is a new
// Review Requested At value. The per-ticket lock plus the persisted in-flight
// marker make duplicate reviews across processes impossible. A skip returns
// ErrNotEligible without mutating the run.
func (s *Service) ReviewOnce(ctx context.Context, issue string, scheduled ScheduledReviewRequest) (Run, bool, error) {
	lock, err := s.Store.Lock(ctx, issue)
	if err != nil {
		return Run{}, false, err
	}
	defer lock.Release()
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, false, err
	}
	if err := requireApproved(run); err != nil {
		return run, false, fmt.Errorf("%w: %v", ErrNotEligible, err)
	}
	if run.ReviewInFlight != "" || run.Phase == PhaseReviewing {
		return run, false, fmt.Errorf("%w: a review is already in flight for %s", ErrNotEligible, firstNonEmpty(run.ReviewInFlight, run.SubmittedCommit))
	}
	switch run.Phase {
	case PhaseCoding, PhaseCorrecting, PhaseAwaitingReview:
	case PhaseBlocked:
		return run, false, fmt.Errorf("%w: run is blocked: %s", ErrNotEligible, run.Note)
	default:
		return run, false, fmt.Errorf("%w: run phase %s does not need a review", ErrNotEligible, run.Phase)
	}
	ticket, err := s.Tickets.Read(ctx, issue)
	if err != nil {
		return run, false, err
	}
	if scheduled.Now.IsZero() {
		scheduled.Now = s.now()
	}
	if scheduled.Location == nil {
		return run, false, errors.New("scheduled review needs an explicit ticket timezone")
	}
	eligibility := EvaluateScheduledReview(run, ticket, scheduled.Now, scheduled.Window, scheduled.Location)
	if !eligibility.Eligible {
		return run, false, fmt.Errorf("%w: %s", ErrNotEligible, eligibility.Reason)
	}
	commit := strings.TrimSpace(ticket.Facts.SubmittedCommit)
	if run.SubmittedCommit != commit {
		run.SubmittedCommit = commit
	}
	check := s.verifySubmission(ctx, run, ticket, commit)
	if check.err != nil {
		return run, false, check.err
	}
	if !check.ready {
		return run, false, fmt.Errorf("%w: %s", ErrNotEligible, check.reason)
	}
	sessions, err := s.Sessions.List(ctx)
	if err != nil {
		return run, false, fmt.Errorf("session liveness unknown: %w", err)
	}
	facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
	if err != nil {
		return run, false, err
	}
	facts.Commit = commit
	review, err := s.reviewCommit(ctx, &run, ticket, facts)
	if err != nil {
		return run, false, err
	}
	if review.Verdict != youtrack.VerdictPass {
		if err := s.ensureCorrection(ctx, &run); err != nil {
			run, blockErr := s.block(issue, err.Error())
			return run, false, blockErr
		}
	}
	return run, true, nil
}

func (s *Service) reviewLocked(ctx context.Context, issue string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	if err := requireApproved(run); err != nil {
		return run, err
	}
	if run.Phase == PhaseCoding || run.Phase == PhaseCorrecting {
		if run.Phase == PhaseCorrecting {
			if err := s.ensureCorrection(ctx, &run); err != nil {
				return s.block(issue, err.Error())
			}
		}
		run, err = s.AwaitSubmission(ctx, issue)
		if err != nil {
			return run, err
		}
	}
	for {
		if strings.TrimSpace(run.SubmittedCommit) == "" {
			return s.block(issue, "run has no submitted commit to review")
		}
		if run.Phase == PhaseReviewing && run.ReviewInFlight == "" {
			return s.block(issue, "run is already in the reviewing phase; refusing a review whose prior attempt is unknown")
		}
		if run.ReviewInFlight == run.SubmittedCommit {
			return s.block(issue, "review for "+run.SubmittedCommit+" may already have run before the supervisor stopped; refusing a duplicate review")
		}
		ticket, err := s.Tickets.Read(ctx, issue)
		if err != nil {
			return run, fmt.Errorf("read ticket: %w", err)
		}
		if ticket.Facts.AIStatus != youtrack.AIStatusAwaiting {
			return s.block(issue, "ticket AI Status is "+ticket.Facts.AIStatus+", not "+youtrack.AIStatusAwaiting+" at review time")
		}
		check := s.verifySubmission(ctx, run, ticket, run.SubmittedCommit)
		if check.err != nil {
			return s.block(issue, check.err.Error())
		}
		if !check.ready {
			// The ticket may have moved just after AwaitSubmission; give it the
			// same bounded confirmation window before blocking. A coder that is
			// still working is waited for without a confirmation deadline.
			confirmed := s.now().Add(s.confirmFor())
			for !check.ready {
				if !check.waitForCoder && s.now().After(confirmed) {
					return s.block(issue, "review gate failed: "+check.reason)
				}
				if err := sleep(ctx, s.pollEvery()); err != nil {
					return run, err
				}
				fresh, err := s.Tickets.Read(ctx, issue)
				if err != nil {
					return run, fmt.Errorf("read ticket: %w", err)
				}
				if fresh.Facts.AIStatus != youtrack.AIStatusAwaiting {
					return s.block(issue, "ticket AI Status changed to "+fresh.Facts.AIStatus+" while confirming the submission")
				}
				ticket = fresh
				check = s.verifySubmission(ctx, run, ticket, run.SubmittedCommit)
				if check.err != nil {
					return s.block(issue, check.err.Error())
				}
			}
		}
		if review, ok := run.LatestReview(run.SubmittedCommit); ok {
			if review.Verdict == youtrack.VerdictPass {
				run.Phase = PhaseAwaitingPrelive
				run.Note = "passing review already recorded for " + run.SubmittedCommit
				return run, s.Store.Save(run)
			}
			if err := s.ensureCorrection(ctx, &run); err != nil {
				return s.block(issue, err.Error())
			}
			run, err = s.AwaitSubmission(ctx, issue)
			if err != nil {
				return run, err
			}
			continue
		}
		if run.ReviewRounds >= s.maxRounds() {
			return s.block(issue, fmt.Sprintf("review did not pass after %d rounds", run.ReviewRounds))
		}
		sessions, err := s.Sessions.List(ctx)
		if err != nil {
			return run, fmt.Errorf("session liveness unknown: %w", err)
		}
		facts, err := s.factsFor(ctx, issue, ticket, run.Coder, sessions)
		if err != nil {
			return run, err
		}
		facts.Commit = run.SubmittedCommit

		review, err := s.reviewCommit(ctx, &run, ticket, facts)
		if err != nil {
			return run, err
		}
		if review.Verdict == youtrack.VerdictPass {
			return run, nil
		}
		if err := s.ensureCorrection(ctx, &run); err != nil {
			return s.block(issue, err.Error())
		}
		run, err = s.AwaitSubmission(ctx, issue)
		if err != nil {
			return run, err
		}
	}
}

// reviewCommit performs exactly one review of the run's submitted commit and
// records its verdict. It persists the in-flight marker before launching the
// reviewer, so a crash cannot authorize a duplicate review.
func (s *Service) reviewCommit(ctx context.Context, run *Run, ticket TicketState, facts youtrack.Facts) (Review, error) {
	run.ReviewInFlight = run.SubmittedCommit
	run.Phase = PhaseReviewing
	run.ReviewRounds++
	if err := s.Store.Save(*run); err != nil {
		return Review{}, err
	}
	result, err := s.Reviewer.Review(ctx, ReviewRequest{
		Issue:            run.Issue,
		Summary:          ticket.Summary,
		Acceptance:       ticket.Description,
		Commit:           run.SubmittedCommit,
		Worktree:         run.Coder.WorktreePath,
		Model:            s.reviewerModel(),
		Effort:           s.reviewerEffort(),
		IsolatedSnapshot: true,
	})
	if err != nil {
		run.Note = "reviewer failed: " + err.Error()
		run.Phase = PhaseBlocked
		_ = s.Store.Save(*run)
		return Review{}, fmt.Errorf("run reviewer: %w", err)
	}
	if result.Verdict != youtrack.VerdictPass && result.Verdict != youtrack.VerdictFail {
		run.Note = "reviewer returned no conclusive verdict; refusing to guess or re-run"
		run.Phase = PhaseBlocked
		_ = s.Store.Save(*run)
		return Review{}, errors.New(run.Note)
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
	if strings.TrimSpace(review.Session) == "" {
		run.Note = "reviewer reported no native session id; refusing to record a generated identity"
		run.Phase = PhaseBlocked
		_ = s.Store.Save(*run)
		return Review{}, errors.New(run.Note)
	}
	if err := s.applyAndVerify(ctx, youtrack.StageReview, facts, ptr(review.ReviewEvidence()), nil); err != nil {
		run.AddReview(review)
		run.ReviewInFlight = ""
		run.Phase = PhaseBlocked
		run.Note = "review transition failed: " + err.Error()
		_ = s.Store.Save(*run)
		return Review{}, fmt.Errorf("record review: %w", err)
	}
	run.AddReview(review)
	run.ReviewInFlight = ""
	run.CorrectionSentFor = ""
	run.LastReviewRequestAt = ticket.Facts.ReviewRequestedAt
	if review.Verdict == youtrack.VerdictPass {
		run.Phase = PhaseAwaitingPrelive
		run.Note = "independent review passed; pre-live acceptance is still required"
	} else {
		run.Phase = PhaseCorrecting
		run.Note = "independent review failed; findings must go back to the same coder"
	}
	if err := s.Store.Save(*run); err != nil {
		return Review{}, err
	}
	return review, nil
}

func (s *Service) reviewerModel() string {
	return firstNonEmpty(s.ReviewerModel, "gpt-6.1-sol")
}

func (s *Service) reviewerEffort() string {
	return firstNonEmpty(s.ReviewerEffort, "low")
}

// ensureCorrection delivers one review's findings to the same coder over the
// session's real interface. The durable commit marker plus the daemon's
// idempotency key make a restart safe.
func (s *Service) ensureCorrection(ctx context.Context, run *Run) error {
	if run.CorrectionSentFor == run.SubmittedCommit {
		return nil
	}
	review, ok := run.LatestReview(run.SubmittedCommit)
	if !ok || review.Verdict != youtrack.VerdictFail {
		return errors.New("no failing review to return to the coder")
	}
	message := correctionMessage(review)
	if err := s.Sessions.Send(ctx, run.Coder.OrchestratorID, SendRequest{
		Text:            message,
		ClientMessageID: "review-correction:" + run.Issue + ":" + review.Commit,
	}); err != nil {
		return fmt.Errorf("could not return findings to the coder: %w", err)
	}
	run.CorrectionSentFor = run.SubmittedCommit
	return s.Store.Save(*run)
}

func correctionMessage(review Review) string {
	message := "Independent review of commit " + review.Commit + " failed. Fix these findings, commit and push the corrections, then submit the new full SHA to YouTrack with AI Status Awaiting AI Review. Do not amend or reuse the reviewed commit.\n\n" + review.Findings
	return strings.TrimSpace(message)
}

// Prelive runs the configured pre-live check and only then claims release
// readiness. Without a configured environment this must not be called.
func (s *Service) Prelive(ctx context.Context, issue, command string) (Run, error) {
	lock, err := s.Store.Lock(ctx, issue)
	if err != nil {
		return Run{}, err
	}
	defer lock.Release()
	return s.preliveLocked(ctx, issue, command)
}

func (s *Service) preliveLocked(ctx context.Context, issue, command string) (Run, error) {
	run, err := s.Store.Load(issue)
	if err != nil {
		return Run{}, err
	}
	if err := requireApproved(run); err != nil {
		return run, err
	}
	if run.Phase != PhaseAwaitingPrelive {
		return Run{}, fmt.Errorf("run is %s, not ready for pre-live acceptance", run.Phase)
	}
	if strings.TrimSpace(command) == "" {
		return s.block(issue, "pre-live configuration is absent; refusing to claim release readiness")
	}
	ticket, err := s.Tickets.Read(ctx, issue)
	if err != nil {
		return Run{}, err
	}
	if ticket.Facts.LastReviewedCommit != run.SubmittedCommit {
		return Run{}, fmt.Errorf("ticket reviewed commit %q is not the submitted commit %q", ticket.Facts.LastReviewedCommit, run.SubmittedCommit)
	}
	if _, err := s.checkout().Validate(ctx, run.Coder.WorktreePath, run.Coder.Branch, run.SubmittedCommit); err != nil {
		return s.block(issue, "pre-live worktree does not prove the reviewed commit: "+err.Error())
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
	if err := s.applyAndVerify(ctx, youtrack.StagePrelive, facts, ptr(review.ReviewEvidence()), ptr(record.Evidence())); err != nil {
		run.Phase = PhaseBlocked
		run.Note = "pre-live transition failed: " + err.Error()
		_ = s.Store.Save(run)
		return run, err
	}
	run.Phase = PhasePreliveVerified
	run.Note = "pre-live verified; production deployment remains a separate owner-authorized step"
	if err := s.Store.Save(run); err != nil {
		return Run{}, err
	}
	return run, nil
}

// applyAndVerify writes one transition, refuses a skipped evidence field, then
// re-reads the ticket and checks the values that make the claim true.
func (s *Service) applyAndVerify(ctx context.Context, stage youtrack.Stage, facts youtrack.Facts, review *youtrack.ReviewEvidence, prelive *youtrack.PreliveEvidence) error {
	skipped, err := s.Tickets.Apply(ctx, stage, facts, review, prelive)
	if err != nil {
		return err
	}
	if missing := youtrack.MissingEvidenceFields(stage, skipped); len(missing) > 0 {
		return fmt.Errorf("ticket schema cannot record required evidence for %s: %s", stage, strings.Join(missing, ", "))
	}
	ticket, err := s.Tickets.Read(ctx, facts.IssueID)
	if err != nil {
		return fmt.Errorf("re-read ticket after %s: %w", stage, err)
	}
	if ticket.Facts.AIStatus == youtrack.AIStatusComplete {
		return errors.New("refusing a ticket transition that sets AI Status Complete")
	}
	switch stage {
	case youtrack.StageStart:
		if ticket.Facts.State != youtrack.StateInProgress || ticket.Facts.AIStatus != youtrack.AIStatusInProgress {
			return fmt.Errorf("ticket did not record start: state=%q ai=%q", ticket.Facts.State, ticket.Facts.AIStatus)
		}
		if ticket.Facts.RecordedSessionID != facts.CoderSessionID && ticket.Facts.RecordedSessionID != facts.OrchestratorSessionID {
			return fmt.Errorf("ticket recorded session %q, not %q", ticket.Facts.RecordedSessionID, facts.CoderSessionID)
		}
		if ticket.Facts.Branch != "" && ticket.Facts.Branch != facts.Branch {
			return fmt.Errorf("ticket recorded branch %q, not %q", ticket.Facts.Branch, facts.Branch)
		}
		if ticket.Facts.WorktreePath != "" && ticket.Facts.WorktreePath != facts.WorktreePath {
			return fmt.Errorf("ticket recorded worktree %q, not %q", ticket.Facts.WorktreePath, facts.WorktreePath)
		}
		if ticket.Facts.CoderModel != "" && ticket.Facts.CoderModel != facts.CoderModel {
			if !sameModel(ticket.Facts.CoderModel, facts.CoderModel) {
				return fmt.Errorf("ticket recorded coder model %q, not %q", ticket.Facts.CoderModel, facts.CoderModel)
			}
		}
	case youtrack.StageSubmit:
		if ticket.Facts.SubmittedCommit != facts.Commit {
			return fmt.Errorf("ticket submitted commit is %q, not %q", ticket.Facts.SubmittedCommit, facts.Commit)
		}
	case youtrack.StageReview:
		if review == nil || ticket.Facts.LastReviewedCommit != review.Commit {
			return fmt.Errorf("ticket reviewed commit is %q, not %q", ticket.Facts.LastReviewedCommit, reviewCommit(review))
		}
		wantState, wantAI := youtrack.StateCodeReview, youtrack.AIStatusInProgress
		if review.Verdict == youtrack.VerdictFail {
			wantState, wantAI = youtrack.StateInProgress, youtrack.AIStatusFailed
		}
		if ticket.Facts.State != wantState || ticket.Facts.AIStatus != wantAI {
			return fmt.Errorf("ticket did not record review: state=%q ai=%q", ticket.Facts.State, ticket.Facts.AIStatus)
		}
	case youtrack.StagePrelive:
		if ticket.Facts.VerifiedCommit != facts.Commit {
			return fmt.Errorf("ticket pre-live commit is %q, not %q", ticket.Facts.VerifiedCommit, facts.Commit)
		}
		if ticket.Facts.State != youtrack.StateForRelease || ticket.Facts.AIStatus != youtrack.AIStatusInProgress {
			return fmt.Errorf("ticket did not record pre-live acceptance: state=%q ai=%q", ticket.Facts.State, ticket.Facts.AIStatus)
		}
	}
	return nil
}

func reviewCommit(review *youtrack.ReviewEvidence) string {
	if review == nil {
		return ""
	}
	return review.Commit
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

// sameModel accepts a provider-qualified model id and its bare ticket value.
func sameModel(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasSuffix(a, "/"+b) || strings.HasSuffix(b, "/"+a)
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
