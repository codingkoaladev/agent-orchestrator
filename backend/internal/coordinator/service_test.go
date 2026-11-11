package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

const (
	commitOne = "1111111111111111111111111111111111111111"
	commitTwo = "2222222222222222222222222222222222222222"
	nativeID  = "ses_native_1"
)

// fakeTicket runs the real guards and then mutates its own ticket facts, so a
// service bug that builds invalid facts fails here rather than in production.
type fakeTicket struct {
	facts    youtrack.TicketFacts
	applied  []youtrack.Stage
	readErr  error
	applyErr error
}

func (f *fakeTicket) Read(context.Context, string) (TicketState, error) {
	if f.readErr != nil {
		return TicketState{}, f.readErr
	}
	return TicketState{Facts: f.facts}, nil
}

func (f *fakeTicket) Apply(_ context.Context, stage youtrack.Stage, facts youtrack.Facts, review *youtrack.ReviewEvidence, prelive *youtrack.PreliveEvidence) ([]string, error) {
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if _, err := youtrack.Validate(stage, facts, review, prelive); err != nil {
		return nil, err
	}
	f.applied = append(f.applied, stage)
	switch stage {
	case youtrack.StageStart:
		f.facts.State = youtrack.StateInProgress
		f.facts.AIStatus = youtrack.AIStatusInProgress
		f.facts.RecordedSessionID = facts.CoderSessionID
	case youtrack.StageSubmit:
		f.facts.SubmittedCommit = facts.Commit
		f.facts.AIStatus = youtrack.AIStatusAwaiting
	case youtrack.StageReview:
		f.facts.LastReviewedCommit = review.Commit
		if review.Verdict == youtrack.VerdictPass {
			f.facts.State = youtrack.StateCodeReview
			f.facts.AIStatus = youtrack.AIStatusInProgress
		} else {
			f.facts.State = youtrack.StateInProgress
			f.facts.AIStatus = youtrack.AIStatusFailed
		}
	case youtrack.StagePrelive:
		f.facts.State = youtrack.StateForRelease
		f.facts.AIStatus = youtrack.AIStatusInProgress
		f.facts.VerifiedCommit = prelive.Commit
	}
	return nil, nil
}

type fakeSessions struct {
	spawned  int
	live     map[string]bool
	listErr  error
	gets     map[string]SessionStatus
	sent     []string
	spawnErr error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{live: map[string]bool{}, gets: map[string]SessionStatus{}}
}

func (f *fakeSessions) Spawn(_ context.Context, request SpawnRequest) (Session, error) {
	if f.spawnErr != nil {
		return Session{}, f.spawnErr
	}
	f.spawned++
	id := "pilot-" + string(rune('1'+f.spawned-1))
	f.live[id] = true
	f.gets[id] = SessionStatus{ID: id, NativeID: nativeID, Branch: request.Branch, WorktreePath: "/tmp/wt-" + id, Activity: "working"}
	return Session{OrchestratorID: id, Branch: request.Branch, WorktreePath: "/tmp/wt-" + id, Model: request.Model, Effort: request.Effort}, nil
}

func (f *fakeSessions) List(context.Context) ([]youtrack.SessionFact, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	facts := make([]youtrack.SessionFact, 0, len(f.live))
	for id, live := range f.live {
		facts = append(facts, youtrack.SessionFact{ID: id, Live: live})
	}
	return facts, nil
}

func (f *fakeSessions) Get(_ context.Context, id string) (SessionStatus, error) {
	status, ok := f.gets[id]
	if !ok {
		return SessionStatus{}, errors.New("unknown session")
	}
	return status, nil
}

func (f *fakeSessions) Send(_ context.Context, _, text string) error {
	f.sent = append(f.sent, text)
	return nil
}

type fakeReviewer struct {
	results []ReviewResult
	calls   int
}

func (f *fakeReviewer) Review(_ context.Context, request ReviewRequest) (ReviewResult, error) {
	f.calls++
	if len(f.results) == 0 {
		return ReviewResult{}, errors.New("no review queued")
	}
	result := f.results[0]
	f.results = f.results[1:]
	result.Session = firstNonEmpty(result.Session, "codex-review-"+request.Commit[:6])
	result.Model = firstNonEmpty(result.Model, "gpt-6.1-sol")
	result.Effort = firstNonEmpty(result.Effort, "low")
	return result, nil
}

type fakeDetector struct{ commits []string }

func (f *fakeDetector) Detect(context.Context, string, string) (string, error) {
	if len(f.commits) == 0 {
		return "", errors.New("no new commit")
	}
	commit := f.commits[0]
	f.commits = f.commits[1:]
	return commit, nil
}

type fakePrelive struct {
	record Prelive
	err    error
	calls  int
}

func (f *fakePrelive) Run(_ context.Context, worktree, commit, command string) (Prelive, error) {
	f.calls++
	if f.err != nil {
		return Prelive{Commit: commit, Command: command, ExitCode: 1}, f.err
	}
	record := f.record
	record.Commit = commit
	record.Command = command
	return record, nil
}

func newService(t *testing.T, ticket *fakeTicket) (*Service, *fakeSessions, *fakeReviewer, *Store) {
	t.Helper()
	sessions := newFakeSessions()
	store := Store{Dir: t.TempDir()}
	service := &Service{
		Tickets:       ticket,
		Sessions:      sessions,
		Reviewer:      &fakeReviewer{},
		Detector:      &fakeDetector{},
		PreliveRunner: &fakePrelive{record: Prelive{ExitCode: 0, OutputDigest: "digest"}},
		Store:         store,
		PollEvery:     time.Millisecond,
		WaitFor:       200 * time.Millisecond,
		MaxRounds:     2,
	}
	return service, sessions, service.Reviewer.(*fakeReviewer), &store
}

func approved() StartRequest {
	return StartRequest{
		Issue: "CKM-825", Project: "ckadmin", Branch: "CKM-825",
		Model: "opencode-go/deepseek-v4.1-flash", Prompt: "do the work", Approved: true,
	}
}

func TestStartDispatchesOnceAndRefusesADuplicate(t *testing.T) {
	ticket := &fakeTicket{}
	service, sessions, _, _ := newService(t, ticket)

	run, err := service.Start(context.Background(), approved())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.Phase != PhaseCoding || sessions.spawned != 1 {
		t.Fatalf("start phase=%q spawned=%d", run.Phase, sessions.spawned)
	}
	if _, err := service.Start(context.Background(), approved()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyRunning", err)
	}
	if sessions.spawned != 1 {
		t.Fatalf("duplicate start spawned %d workers", sessions.spawned)
	}
}

func TestStartRequiresApproval(t *testing.T) {
	service, sessions, _, _ := newService(t, &fakeTicket{})
	request := approved()
	request.Approved = false
	if _, err := service.Start(context.Background(), request); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Start() without approval error = %v", err)
	}
	if sessions.spawned != 0 {
		t.Fatal("unapproved start dispatched a worker")
	}
}

func TestStartFailsClosedWhenLivenessIsUnknown(t *testing.T) {
	ticket := &fakeTicket{}
	service, sessions, _, _ := newService(t, ticket)
	sessions.listErr = errors.New("daemon unreachable")
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("Start dispatched while session liveness was unknown")
	}
	if sessions.spawned != 0 {
		t.Fatal("Start spawned a worker on unknown liveness")
	}
}

func TestStartRefusesWhenTheTicketRecordsAnotherLiveSession(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{RecordedSessionID: "pilot-7"}}
	service, sessions, _, _ := newService(t, ticket)
	sessions.live["pilot-7"] = true
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("Start dispatched over a live recorded session")
	}
	if sessions.spawned != 0 {
		t.Fatal("Start spawned a duplicate worker")
	}
}

func TestRestartResumesWithoutASecondDispatch(t *testing.T) {
	ticket := &fakeTicket{}
	service, sessions, _, store := newService(t, ticket)
	if _, err := service.Start(context.Background(), approved()); err != nil {
		t.Fatal(err)
	}
	// The process restarts: a new service and store instance read the same run.
	reloaded := &Service{Tickets: ticket, Sessions: sessions, Store: *store, PollEvery: time.Millisecond}
	sessions.live["pilot-1"] = false
	run, err := reloaded.Start(context.Background(), approved())
	if err != nil {
		t.Fatalf("resume Start() error = %v", err)
	}
	if run.Phase != PhaseAwaitingReview {
		t.Fatalf("resume phase = %q, want awaiting_review", run.Phase)
	}
	if sessions.spawned != 1 {
		t.Fatalf("resume dispatched a second worker (%d spawns)", sessions.spawned)
	}
}

func seedRun(t *testing.T, store *Store) {
	t.Helper()
	if err := store.Save(Run{
		Issue: "CKM-825", Project: "ckadmin", Phase: PhaseAwaitingReview,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewLoopReturnsFindingsAndPassingReviewKeepsAIInProgress(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID,
	}}
	service, sessions, reviewer, store := newService(t, ticket)
	sessions.live["pilot-1"] = true
	sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}
	seedRun(t, store)
	reviewer.results = []ReviewResult{
		{Verdict: youtrack.VerdictFail, Findings: "guard trusts caller SHAs"},
		{Verdict: youtrack.VerdictPass},
	}
	service.Detector = &fakeDetector{commits: []string{commitTwo}}

	run, err := service.Review(context.Background(), "CKM-825")
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if reviewer.calls != 2 {
		t.Fatalf("reviewer calls = %d, want 2", reviewer.calls)
	}
	if len(sessions.sent) != 1 || !strings.Contains(sessions.sent[0], "guard trusts caller SHAs") {
		t.Fatalf("findings were not returned to the coder: %#v", sessions.sent)
	}
	if run.Phase != PhaseAwaitingPrelive {
		t.Fatalf("phase = %q, want awaiting_prelive", run.Phase)
	}
	if run.SubmittedCommit != commitTwo {
		t.Fatalf("submitted commit = %q, want the corrected commit", run.SubmittedCommit)
	}
	if ticket.facts.State != youtrack.StateCodeReview {
		t.Fatalf("ticket state = %q", ticket.facts.State)
	}
	if ticket.facts.AIStatus == youtrack.AIStatusComplete {
		t.Fatal("passing review completed AI work before pre-live acceptance")
	}
}

func TestReviewRefusesWhenTheTicketHoldsAnotherCommit(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitTwo, RecordedSessionID: nativeID}}
	service, sessions, reviewer, store := newService(t, ticket)
	sessions.live["pilot-1"] = true
	sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}
	seedRun(t, store)
	reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	if _, err := service.Review(context.Background(), "CKM-825"); err == nil {
		t.Fatal("Review accepted a run commit that the ticket does not hold")
	}
	if reviewer.calls != 0 {
		t.Fatal("Review ran the reviewer for a stale commit")
	}
}

func TestPreliveIsGatedOnARecordedPassAndSetsRelease(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne, RecordedSessionID: nativeID}}
	service, sessions, _, store := newService(t, ticket)
	sessions.live["pilot-1"] = true
	sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}

	// Not ready yet: the run is still coding.
	if err := store.Save(Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Prelive(context.Background(), "CKM-825", "make check"); err == nil {
		t.Fatal("Prelive ran before review passed")
	}

	// Passed review and ticket records the reviewed commit.
	if err := store.Save(Run{
		Issue: "CKM-825", Phase: PhaseAwaitingPrelive,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
		Reviews:         []Review{{Commit: commitOne, Verdict: youtrack.VerdictPass, Session: "codex-1", Model: "gpt-6.1-sol", Effort: "low"}},
	}); err != nil {
		t.Fatal(err)
	}
	ticket.facts.LastReviewedCommit = commitOne
	run, err := service.Prelive(context.Background(), "CKM-825", "make check")
	if err != nil {
		t.Fatalf("Prelive() error = %v", err)
	}
	if run.Phase != PhasePreliveVerified {
		t.Fatalf("phase = %q", run.Phase)
	}
	if ticket.facts.State != youtrack.StateForRelease || ticket.facts.AIStatus != youtrack.AIStatusInProgress {
		t.Fatalf("release facts = %#v", ticket.facts)
	}
}

func TestPreliveFailureBlocksTheRun(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne, RecordedSessionID: nativeID, LastReviewedCommit: commitOne}}
	service, sessions, _, store := newService(t, ticket)
	sessions.live["pilot-1"] = true
	sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}
	service.PreliveRunner = &fakePrelive{err: errors.New("migration check failed")}
	if err := store.Save(Run{
		Issue: "CKM-825", Phase: PhaseAwaitingPrelive,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
		Reviews:         []Review{{Commit: commitOne, Verdict: youtrack.VerdictPass, Session: "codex-1", Model: "gpt-6.1-sol"}},
	}); err != nil {
		t.Fatal(err)
	}
	run, err := service.Prelive(context.Background(), "CKM-825", "make check")
	if err == nil {
		t.Fatal("Prelive accepted a failing check")
	}
	if run.Phase != PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", run.Phase)
	}
	if ticket.facts.State == youtrack.StateForRelease {
		t.Fatal("a failed pre-live check moved the ticket to For Release")
	}
}

func TestRunStoreSurvivesRestartAndKeepsBothIdentities(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	run := Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Subscription: "subscription-3"}}
	if err := store.Save(run); err != nil {
		t.Fatal(err)
	}
	reloaded, err := (Store{Dir: store.Dir}).Load("CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Coder.OrchestratorID != "pilot-1" || reloaded.Coder.NativeID != nativeID {
		t.Fatalf("identities lost: %#v", reloaded.Coder)
	}
	if reloaded.Coder.Subscription != "subscription-3" {
		t.Fatalf("subscription lost: %#v", reloaded.Coder)
	}
}

func TestAddReviewReplacesAVerdictForTheSameCommit(t *testing.T) {
	run := Run{}
	run.AddReview(Review{Commit: commitOne, Verdict: youtrack.VerdictFail})
	run.AddReview(Review{Commit: commitOne, Verdict: youtrack.VerdictPass})
	if len(run.Reviews) != 1 || run.Reviews[0].Verdict != youtrack.VerdictPass {
		t.Fatalf("reviews = %#v", run.Reviews)
	}
	if _, ok := run.PassingReview(commitOne); !ok {
		t.Fatal("PassingReview did not find the pass")
	}
	if _, ok := run.PassingReview(commitTwo); ok {
		t.Fatal("PassingReview reported a pass for a commit never reviewed")
	}
}

func TestAmbiguousDispatchIsNotRetried(t *testing.T) {
	service, sessions, _, _ := newService(t, &fakeTicket{})
	// A lost response leaves the launch outcome unknown.
	sessions.spawnErr = errors.New("response lost after dispatch")
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected ambiguous dispatch error")
	}
	run, err := service.Store.Load("CKM-825")
	if err != nil || run.Phase != PhaseDispatching {
		t.Fatalf("missing durable claim: %+v %v", run, err)
	}
	sessions.spawnErr = nil
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("ambiguous dispatch was retried")
	}
	if sessions.spawned != 0 {
		t.Fatal("duplicate worker started")
	}
}
func TestNativeTicketIdentityPreventsDuplicateDispatch(t *testing.T) {
	ticket := TicketState{Facts: youtrack.TicketFacts{RecordedSessionID: "ses-native"}}
	blocked, err := blockingDispatch(ticket, []youtrack.SessionFact{{ID: "daemon-1", NativeID: "ses-native", Live: true}}, approved())
	if err != nil || blocked == "" {
		t.Fatal("native session identity did not block duplicate")
	}
}
func TestStoreClaimIsExclusive(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	run := Run{Issue: "CKM-825", Phase: PhaseDispatching}
	if err := store.Claim(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(context.Background(), run); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second claim: %v", err)
	}
}

func TestStartResumesTicketRecordingWithoutAnotherWorker(t *testing.T) {
	ticket := &fakeTicket{applyErr: errors.New("tracker unavailable")}
	service, sessions, _, _ := newService(t, ticket)
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected tracker failure")
	}
	run, err := service.Store.Load("CKM-825")
	if err != nil || run.Coder.OrchestratorID != "pilot-1" || run.Phase != PhaseDispatching {
		t.Fatalf("worker not durably recorded: %+v %v", run, err)
	}
	ticket.applyErr = nil
	run, err = service.Start(context.Background(), approved())
	if err != nil || run.Phase != PhaseCoding || run.Coder.NativeID != nativeID {
		t.Fatalf("failed recovery: %+v %v", run, err)
	}
	if sessions.spawned != 1 {
		t.Fatalf("started %d workers", sessions.spawned)
	}
}

func TestRejectedPreflightAllowsCorrectedRetry(t *testing.T) {
	service, sessions, _, _ := newService(t, &fakeTicket{})
	sessions.spawnErr = fmt.Errorf("%w: subscription mismatch", ErrNotDispatched)
	if _, err := service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected preflight rejection")
	}
	sessions.spawnErr = nil
	if _, err := service.Start(context.Background(), approved()); err != nil {
		t.Fatalf("corrected preflight cannot retry: %v", err)
	}
	if sessions.spawned != 1 {
		t.Fatalf("spawned %d", sessions.spawned)
	}
}
