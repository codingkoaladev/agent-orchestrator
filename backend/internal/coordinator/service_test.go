package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	mu          sync.Mutex
	facts       youtrack.TicketFacts
	fields      map[string]string
	summary     string
	description string
	applied     []youtrack.Stage
	readErr     error
	applyErr    error
	skipped     []string
}

func (f *fakeTicket) Read(context.Context, string) (TicketState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return TicketState{}, f.readErr
	}
	return TicketState{Facts: f.facts, Fields: f.fields, Summary: f.summary, Description: f.description}, nil
}

func (f *fakeTicket) Apply(_ context.Context, stage youtrack.Stage, facts youtrack.Facts, review *youtrack.ReviewEvidence, prelive *youtrack.PreliveEvidence) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.applyErr != nil {
		return nil, f.applyErr
	}
	if _, err := youtrack.Validate(stage, facts, review, prelive); err != nil {
		return nil, err
	}
	f.applied = append(f.applied, stage)
	if f.skipped != nil {
		return f.skipped, nil
	}
	switch stage {
	case youtrack.StageStart:
		f.facts.State = youtrack.StateInProgress
		f.facts.AIStatus = youtrack.AIStatusInProgress
		f.facts.RecordedSessionID = facts.CoderSessionID
		f.facts.Branch = facts.Branch
		f.facts.WorktreePath = facts.WorktreePath
		f.facts.CoderModel = facts.CoderModel
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

func (f *fakeTicket) update(fn func(*youtrack.TicketFacts)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.facts)
}

func (f *fakeTicket) snapshot() youtrack.TicketFacts {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.facts
}

type fakeSessions struct {
	mu       sync.Mutex
	spawned  int
	live     map[string]bool
	listErr  error
	gets     map[string]SessionStatus
	sent     []SendRequest
	sentTo   []string
	onSend   func(SendRequest)
	spawnErr error
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{live: map[string]bool{}, gets: map[string]SessionStatus{}}
}

func (f *fakeSessions) Spawn(_ context.Context, request SpawnRequest) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.spawnErr != nil {
		return Session{}, f.spawnErr
	}
	f.spawned++
	id := fmt.Sprintf("pilot-%d", f.spawned)
	worktree := "/tmp/wt-" + id
	f.live[id] = true
	f.gets[id] = SessionStatus{ID: id, NativeID: nativeID, Mode: "chat", Branch: request.Branch, WorktreePath: worktree, Activity: "active"}
	return Session{OrchestratorID: id, NativeID: nativeID, Branch: request.Branch, WorktreePath: worktree, Model: request.Model, Effort: request.Effort}, nil
}

func (f *fakeSessions) List(context.Context) ([]youtrack.SessionFact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	facts := make([]youtrack.SessionFact, 0, len(f.live))
	for id, live := range f.live {
		facts = append(facts, youtrack.SessionFact{ID: id, NativeID: f.gets[id].NativeID, Live: live})
	}
	return facts, nil
}

func (f *fakeSessions) Get(_ context.Context, id string) (SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status, ok := f.gets[id]
	if !ok {
		return SessionStatus{}, errors.New("unknown session")
	}
	return status, nil
}

func (f *fakeSessions) Send(_ context.Context, id string, message SendRequest) error {
	f.mu.Lock()
	f.sent = append(f.sent, message)
	f.sentTo = append(f.sentTo, id)
	onSend := f.onSend
	f.mu.Unlock()
	if onSend != nil {
		onSend(message)
	}
	return nil
}

func (f *fakeSessions) setStatus(id string, fn func(*SessionStatus)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.gets[id]
	fn(&status)
	f.gets[id] = status
}

func (f *fakeSessions) messages() []SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SendRequest(nil), f.sent...)
}

func (f *fakeSessions) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawned
}

type fakeReviewer struct {
	results []ReviewResult
	calls   int
	onCall  func(int)
	reqs    []ReviewRequest
}

func (f *fakeReviewer) Review(_ context.Context, request ReviewRequest) (ReviewResult, error) {
	f.calls++
	f.reqs = append(f.reqs, request)
	if f.onCall != nil {
		f.onCall(f.calls)
	}
	if len(f.results) == 0 {
		return ReviewResult{}, errors.New("no review queued")
	}
	result := f.results[0]
	f.results = f.results[1:]
	result.Session = firstNonEmpty(result.Session, "codex-thread-1")
	result.Model = firstNonEmpty(result.Model, "gpt-6.1-sol")
	result.Effort = firstNonEmpty(result.Effort, "low")
	return result, nil
}

type fakeCheckout struct {
	err   error
	state CheckoutState
	calls int
}

func (f *fakeCheckout) Validate(context.Context, string, string, string) (CheckoutState, error) {
	f.calls++
	if f.err != nil {
		return CheckoutState{}, f.err
	}
	return f.state, nil
}

type fakePullRequests struct {
	err   error
	pr    PullRequest
	calls int
}

func (f *fakePullRequests) Find(_ context.Context, _ string, branch string) (PullRequest, error) {
	f.calls++
	if f.err != nil {
		return PullRequest{}, f.err
	}
	pr := f.pr
	pr.HeadRef = firstNonEmpty(pr.HeadRef, branch)
	pr.State = firstNonEmpty(pr.State, "OPEN")
	return pr, nil
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

type harness struct {
	service  *Service
	ticket   *fakeTicket
	sessions *fakeSessions
	reviewer *fakeReviewer
	checkout *fakeCheckout
	prs      *fakePullRequests
	prelive  *fakePrelive
	store    *Store
}

func newHarness(t *testing.T, ticket *fakeTicket) *harness {
	t.Helper()
	sessions := newFakeSessions()
	store := Store{Dir: t.TempDir()}
	reviewer := &fakeReviewer{}
	checkout := &fakeCheckout{}
	prs := &fakePullRequests{}
	prelive := &fakePrelive{record: Prelive{ExitCode: 0, OutputDigest: "digest"}}
	service := &Service{
		Tickets:        ticket,
		Sessions:       sessions,
		Reviewer:       reviewer,
		Checkout:       checkout,
		PullRequests:   prs,
		PreliveRunner:  prelive,
		Store:          store,
		PollEvery:      time.Millisecond,
		WaitFor:        500 * time.Millisecond,
		ConfirmFor:     20 * time.Millisecond,
		IdentityWait:   20 * time.Millisecond,
		MaxRounds:      2,
		ReviewerModel:  "gpt-6.1-sol",
		ReviewerEffort: "low",
	}
	return &harness{service: service, ticket: ticket, sessions: sessions, reviewer: reviewer, checkout: checkout, prs: prs, prelive: prelive, store: &store}
}

func approved() StartRequest {
	return StartRequest{
		Issue: "CKM-825", Project: "ckadmin", Branch: "CKM-825",
		Model: "opencode-go/deepseek-v4.1-flash", Prompt: "do the work", Approved: true,
	}
}

func (h *harness) seed(t *testing.T, run Run) {
	t.Helper()
	run.Approved = true
	if run.Coder.Model == "" {
		run.Coder.Model = "opencode-go/deepseek-v4.1-flash"
	}
	if err := h.store.Save(run); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) liveWorker(t *testing.T, commit string) {
	t.Helper()
	h.sessions.mu.Lock()
	h.sessions.live["pilot-1"] = true
	h.sessions.gets["pilot-1"] = SessionStatus{
		ID: "pilot-1", NativeID: nativeID, Mode: "chat", Branch: "CKM-825",
		WorktreePath: "/tmp/wt", Activity: "idle",
	}
	h.sessions.mu.Unlock()
	h.prs.pr = PullRequest{Number: 1, HeadRef: "CKM-825", HeadCommit: commit}
}

func TestStartDispatchesOnceAndRefusesADuplicate(t *testing.T) {
	ticket := &fakeTicket{}
	h := newHarness(t, ticket)

	run, err := h.service.Start(context.Background(), approved())
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if run.Phase != PhaseCoding || h.sessions.spawned != 1 {
		t.Fatalf("start phase=%q spawned=%d", run.Phase, h.sessions.spawned)
	}
	if _, err := h.service.Start(context.Background(), approved()); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Start() error = %v, want ErrAlreadyRunning", err)
	}
	if h.sessions.spawned != 1 {
		t.Fatalf("duplicate start spawned %d workers", h.sessions.spawned)
	}
}

func TestStartRequiresApproval(t *testing.T) {
	h := newHarness(t, &fakeTicket{})
	request := approved()
	request.Approved = false
	if _, err := h.service.Start(context.Background(), request); !errors.Is(err, ErrNotApproved) {
		t.Fatalf("Start() without approval error = %v", err)
	}
	if h.sessions.spawned != 0 {
		t.Fatal("unapproved start dispatched a worker")
	}
}

func TestStartFailsClosedWhenLivenessIsUnknown(t *testing.T) {
	h := newHarness(t, &fakeTicket{})
	h.sessions.listErr = errors.New("daemon unreachable")
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("Start dispatched while session liveness was unknown")
	}
	if h.sessions.spawned != 0 {
		t.Fatal("Start spawned a worker on unknown liveness")
	}
}

func TestStartRefusesWhenTheTicketRecordsAnotherLiveSession(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{RecordedSessionID: "pilot-7"}}
	h := newHarness(t, ticket)
	h.sessions.live["pilot-7"] = true
	h.sessions.gets["pilot-7"] = SessionStatus{ID: "pilot-7", NativeID: "ses_other"}
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("Start dispatched over a live recorded session")
	}
	if h.sessions.spawned != 0 {
		t.Fatal("Start spawned a duplicate worker")
	}
}

func TestStartRefusesToRestartACoderGoneBeforeSubmission(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.sessions.live["pilot-1"] = false
	h.sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Terminated: true}
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("Start restarted work whose outcome was unknown")
	}
	if h.sessions.spawned != 0 {
		t.Fatalf("restart spawned %d workers", h.sessions.spawned)
	}
	run, err := h.store.Load("CKM-825")
	if err != nil || run.Phase != PhaseBlocked {
		t.Fatalf("run = %+v err=%v", run, err)
	}
}

func TestRestartResumesASubmittedRunWithoutASecondDispatch(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, SubmittedCommit: commitOne, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.sessions.live["pilot-1"] = false
	h.sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Terminated: true}
	run, err := h.service.Start(context.Background(), approved())
	if err != nil {
		t.Fatalf("resume Start() error = %v", err)
	}
	if run.Phase != PhaseAwaitingReview {
		t.Fatalf("resume phase = %q, want awaiting_review", run.Phase)
	}
	if h.sessions.spawned != 0 {
		t.Fatalf("resume dispatched a second worker (%d spawns)", h.sessions.spawned)
	}
}

func TestRunStoreSurvivesRestartAndKeepsApprovalAndIdentities(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	run := Run{Issue: "CKM-825", Phase: PhaseCoding, Approved: true, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Subscription: "subscription-3"}}
	if err := store.Save(run); err != nil {
		t.Fatal(err)
	}
	reloaded, err := (Store{Dir: store.Dir}).Load("CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Approved || reloaded.Coder.OrchestratorID != "pilot-1" || reloaded.Coder.NativeID != nativeID || reloaded.Coder.Subscription != "subscription-3" {
		t.Fatalf("durable run lost facts: %#v", reloaded)
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
	h := newHarness(t, &fakeTicket{})
	h.sessions.spawnErr = errors.New("response lost after dispatch")
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected ambiguous dispatch error")
	}
	run, err := h.store.Load("CKM-825")
	if err != nil || run.Phase != PhaseDispatching {
		t.Fatalf("missing durable claim: %+v %v", run, err)
	}
	h.sessions.spawnErr = nil
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("ambiguous dispatch was retried")
	}
	if h.sessions.spawned != 0 {
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

func TestStoreLockIsExclusive(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	first, err := store.Lock(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	if _, err := store.Lock(context.Background(), "CKM-825"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second supervisor lock = %v, want ErrAlreadyRunning", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := store.Lock(context.Background(), "CKM-825")
	if err != nil {
		t.Fatalf("lock was not reclaimable: %v", err)
	}
	_ = second.Release()
}

func TestStartResumesTicketRecordingWithoutAnotherWorker(t *testing.T) {
	ticket := &fakeTicket{applyErr: errors.New("tracker unavailable")}
	h := newHarness(t, ticket)
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected tracker failure")
	}
	run, err := h.store.Load("CKM-825")
	if err != nil || run.Coder.OrchestratorID != "pilot-1" || run.Phase != PhaseDispatching {
		t.Fatalf("worker not durably recorded: %+v %v", run, err)
	}
	ticket.mu.Lock()
	ticket.applyErr = nil
	ticket.mu.Unlock()
	run, err = h.service.Start(context.Background(), approved())
	if err != nil || run.Phase != PhaseCoding || run.Coder.NativeID != nativeID {
		t.Fatalf("failed recovery: %+v %v", run, err)
	}
	if h.sessions.spawned != 1 {
		t.Fatalf("started %d workers", h.sessions.spawned)
	}
}

func TestRejectedPreflightAllowsCorrectedRetry(t *testing.T) {
	h := newHarness(t, &fakeTicket{})
	h.sessions.spawnErr = fmt.Errorf("%w: subscription mismatch", ErrNotDispatched)
	if _, err := h.service.Start(context.Background(), approved()); err == nil {
		t.Fatal("expected preflight rejection")
	}
	h.sessions.spawnErr = nil
	if _, err := h.service.Start(context.Background(), approved()); err != nil {
		t.Fatalf("corrected preflight cannot retry: %v", err)
	}
	if h.sessions.spawned != 1 {
		t.Fatalf("spawned %d", h.sessions.spawned)
	}
}

func TestAwaitSubmissionWaitsForRealTicketStateAndExactCommit(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusInProgress,
		RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.service.WaitFor = 2 * time.Second

	done := make(chan Run, 1)
	go func() {
		run, _ := h.service.AwaitSubmission(context.Background(), "CKM-825")
		done <- run
	}()
	time.Sleep(20 * time.Millisecond)
	if h.reviewer.calls != 0 {
		t.Fatal("review ran before a ticket submission")
	}
	ticket.update(func(f *youtrack.TicketFacts) {
		f.AIStatus = youtrack.AIStatusAwaiting
		f.SubmittedCommit = commitOne
	})
	select {
	case run := <-done:
		if run.Phase != PhaseAwaitingReview || run.SubmittedCommit != commitOne {
			t.Fatalf("run = %+v", run)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitSubmission did not accept a verified ticket submission")
	}
}

func TestAwaitSubmissionRefusesAPullRequestAtAnotherCommit(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitTwo)

	if _, err := h.service.AwaitSubmission(context.Background(), "CKM-825"); err == nil {
		t.Fatal("submission with a different PR head was accepted")
	}
	run, _ := h.store.Load("CKM-825")
	if run.Phase != PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", run.Phase)
	}
}

func TestAwaitSubmissionWaitsForAnActiveCoderThenVerifies(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.sessions.setStatus("pilot-1", func(s *SessionStatus) { s.Activity = "active" })
	h.service.WaitFor = 2 * time.Second
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.sessions.setStatus("pilot-1", func(s *SessionStatus) { s.Activity = "idle" })
	}()
	run, err := h.service.AwaitSubmission(context.Background(), "CKM-825")
	if err != nil || run.Phase != PhaseAwaitingReview {
		t.Fatalf("run=%+v err=%v", run, err)
	}
}

func TestAwaitSubmissionRejectsStagedCode(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.checkout.err = errors.New("worktree has staged changes not in the submitted commit: code.go")
	if _, err := h.service.AwaitSubmission(context.Background(), "CKM-825"); err == nil {
		t.Fatal("staged code was accepted as a submitted commit")
	}
}

func TestAwaitSubmissionAllowsEnvironmentOnlyLocalChanges(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.checkout.state = CheckoutState{Unstaged: []string{".ddev/config.yaml"}, Untracked: []string{".env.local"}}
	run, err := h.service.AwaitSubmission(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(run.Note, "excluded from review") {
		t.Fatalf("note does not record excluded local files: %q", run.Note)
	}
}

func TestStartAdoptsAWorkerThatSubmittedDuringDispatch(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	if err := h.store.Save(Run{Issue: "CKM-825", Project: "ckadmin", Phase: PhaseDispatching, Approved: true,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	h.sessions.mu.Lock()
	h.sessions.live["pilot-1"] = true
	h.sessions.gets["pilot-1"] = SessionStatus{ID: "pilot-1", NativeID: nativeID, Mode: "chat", Branch: "CKM-825", WorktreePath: "/tmp/wt", Activity: "idle"}
	h.sessions.mu.Unlock()

	run, err := h.service.Start(context.Background(), approved())
	if err != nil {
		t.Fatal(err)
	}
	if h.sessions.spawned != 0 {
		t.Fatal("adoption dispatched a duplicate worker")
	}
	if run.Phase != PhaseAwaitingReview || run.SubmittedCommit != commitOne {
		t.Fatalf("run = %+v", run)
	}
	if facts := ticket.snapshot(); facts.AIStatus != youtrack.AIStatusAwaiting {
		t.Fatalf("start recording erased a real submission: %#v", facts)
	}
}

func TestSuperviseRefusesARunWithoutOwnerApproval(t *testing.T) {
	ticket := &fakeTicket{}
	h := newHarness(t, ticket)
	if err := h.store.Save(Run{Issue: "CKM-825", Phase: PhaseCoding,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Supervise(context.Background(), "CKM-825"); err == nil {
		t.Fatal("unapproved run was supervised")
	}
	run, _ := h.store.Load("CKM-825")
	if run.Phase != PhaseCoding || h.reviewer.calls != 0 {
		t.Fatalf("run=%+v reviews=%d", run, h.reviewer.calls)
	}
}

func TestPreliveRefusesAWorktreeThatDoesNotProveTheReviewedCommit(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne, RecordedSessionID: nativeID, LastReviewedCommit: commitOne}}
	h := newHarness(t, ticket)
	h.liveWorker(t, commitOne)
	h.checkout.err = errors.New("worktree HEAD is another commit")
	h.seed(t, Run{
		Issue: "CKM-825", Phase: PhaseAwaitingPrelive,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
		Reviews:         []Review{{Commit: commitOne, Verdict: youtrack.VerdictPass, Session: "codex-1", Model: "gpt-6.1-sol"}},
	})
	if _, err := h.service.Prelive(context.Background(), "CKM-825", "make check"); err == nil {
		t.Fatal("pre-live accepted a worktree that is not on the reviewed commit")
	}
	if h.prelive.calls != 0 {
		t.Fatal("pre-live check ran against the wrong commit")
	}
	if ticket.snapshot().State == youtrack.StateForRelease {
		t.Fatal("wrong-commit pre-live evidence reached For Release")
	}
}

func TestStartAndFollowWaitsAndReviewsWithoutASecondSpawn(t *testing.T) {
	ticket := &fakeTicket{}
	h := newHarness(t, ticket)
	h.service.WaitFor = 2 * time.Second
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}

	done := make(chan struct {
		run Run
		err error
	}, 1)
	go func() {
		run, err := h.service.StartAndFollow(context.Background(), approved())
		done <- struct {
			run Run
			err error
		}{run, err}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.sessions.spawnCount() == 1 && ticket.snapshot().RecordedSessionID != "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if h.sessions.spawnCount() != 1 {
		t.Fatalf("start did not dispatch exactly one worker (%d)", h.sessions.spawnCount())
	}
	h.sessions.setStatus("pilot-1", func(s *SessionStatus) { s.Activity = "idle" })
	h.prs.pr = PullRequest{Number: 1, HeadRef: "CKM-825", HeadCommit: commitOne}
	ticket.update(func(f *youtrack.TicketFacts) {
		f.AIStatus = youtrack.AIStatusAwaiting
		f.SubmittedCommit = commitOne
	})

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("StartAndFollow() error = %v", result.err)
		}
		if result.run.Phase != PhaseAwaitingPrelive {
			t.Fatalf("phase = %q", result.run.Phase)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("StartAndFollow exited without supervising the submitted ticket")
	}
	if h.reviewer.calls != 1 || h.sessions.spawnCount() != 1 {
		t.Fatalf("reviews=%d spawns=%d", h.reviewer.calls, h.sessions.spawnCount())
	}
}

func TestSuperviseAdoptsAWorkerThatSubmittedDuringDispatch(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Project: "ckadmin", Phase: PhaseDispatching,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	run, err := h.service.Supervise(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if h.sessions.spawned != 0 {
		t.Fatal("supervisor dispatched a duplicate worker")
	}
	if run.Phase != PhaseAwaitingPrelive {
		t.Fatalf("phase = %q", run.Phase)
	}
	if facts := ticket.snapshot(); facts.AIStatus == youtrack.AIStatusInProgress && facts.State == youtrack.StateInProgress {
		t.Fatal("adoption reset a real submission")
	}
}

func TestSuperviseReviewsOnceAndReturnsFindingsOverChat(t *testing.T) {
	ticket := &fakeTicket{
		summary:     "Fix the thing",
		description: "Acceptance: the thing works and nothing else changes.",
		facts: youtrack.TicketFacts{
			State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
			SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
		},
	}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{
		{Verdict: youtrack.VerdictFail, Findings: "guard trusts caller SHAs"},
		{Verdict: youtrack.VerdictPass},
	}
	h.sessions.onSend = func(SendRequest) {
		ticket.update(func(f *youtrack.TicketFacts) {
			f.AIStatus = youtrack.AIStatusAwaiting
			f.SubmittedCommit = commitTwo
		})
		h.prs.pr.HeadCommit = commitTwo
	}
	run, err := h.service.Supervise(context.Background(), "CKM-825")
	if err != nil {
		t.Fatalf("Supervise() error = %v", err)
	}
	if h.reviewer.calls != 2 {
		t.Fatalf("reviewer calls = %d, want 2", h.reviewer.calls)
	}
	if got := h.reviewer.reqs[0].Acceptance; !strings.Contains(got, "Acceptance: the thing works") {
		t.Fatalf("review did not receive the ticket acceptance contract: %q", got)
	}
	messages := h.sessions.messages()
	if len(messages) != 1 || h.sessions.sentTo[0] != "pilot-1" {
		t.Fatalf("correction was not delivered to the same coder: %#v %#v", messages, h.sessions.sentTo)
	}
	if !strings.Contains(messages[0].Text, "guard trusts caller SHAs") {
		t.Fatalf("correction did not carry findings: %q", messages[0].Text)
	}
	if messages[0].ClientMessageID != "review-correction:CKM-825:"+commitOne {
		t.Fatalf("correction is not idempotent: %q", messages[0].ClientMessageID)
	}
	if run.Phase != PhaseAwaitingPrelive || run.SubmittedCommit != commitTwo {
		t.Fatalf("run = %+v", run)
	}
	facts := ticket.snapshot()
	if facts.State != youtrack.StateCodeReview || facts.AIStatus == youtrack.AIStatusComplete {
		t.Fatalf("ticket facts = %#v", facts)
	}
	if run.Reviews[0].Session != "codex-thread-1" || run.Reviews[0].Model != "gpt-6.1-sol" || run.Reviews[0].Effort != "low" {
		t.Fatalf("review evidence = %#v", run.Reviews[0])
	}
}

func TestSuperviseRefusesADuplicateReviewAfterRestart(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{
		Issue: "CKM-825", Phase: PhaseReviewing, SubmittedCommit: commitOne, ReviewInFlight: commitOne,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"},
	})
	h.liveWorker(t, commitOne)
	if _, err := h.service.Supervise(context.Background(), "CKM-825"); err == nil {
		t.Fatal("restart repeated a review whose process may have run")
	}
	if h.reviewer.calls != 0 {
		t.Fatalf("reviewer ran %d times after an in-flight review", h.reviewer.calls)
	}
	run, _ := h.store.Load("CKM-825")
	if run.Phase != PhaseBlocked {
		t.Fatalf("phase = %q", run.Phase)
	}
}

func TestSuperviseRefusesAFalseTicketClaimWhenEvidenceFieldsAreMissing(t *testing.T) {
	ticket := &fakeTicket{
		facts: youtrack.TicketFacts{
			State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
			SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
		},
		skipped: []string{youtrack.FieldLastReviewed},
	}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	if _, err := h.service.Supervise(context.Background(), "CKM-825"); err == nil {
		t.Fatal("review was claimed after the ticket failed to record the reviewed commit")
	}
	run, _ := h.store.Load("CKM-825")
	if run.Phase != PhaseBlocked || run.Reviews[0].Verdict != youtrack.VerdictPass {
		t.Fatalf("run = %+v", run)
	}
	if ticket.snapshot().LastReviewedCommit == commitOne {
		t.Fatal("fake ticket unexpectedly recorded the reviewed commit")
	}
}

func TestSuperviseStopsAtPreliveWithoutConfiguration(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	run, err := h.service.Supervise(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != PhaseAwaitingPrelive {
		t.Fatalf("phase = %q", run.Phase)
	}
	if h.prelive.calls != 0 {
		t.Fatal("pre-live ran without deployment configuration")
	}
	facts := ticket.snapshot()
	if facts.State == youtrack.StateForRelease || facts.AIStatus == youtrack.AIStatusComplete {
		t.Fatalf("absent pre-live evidence moved the ticket: %#v", facts)
	}
}

func TestSuperviseRunsConfiguredPreliveAndNeverSetsComplete(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{
		State: youtrack.StateInProgress, AIStatus: youtrack.AIStatusAwaiting,
		SubmittedCommit: commitOne, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt",
	}}
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	h.service.PreliveCommand = "make prelive-check"
	run, err := h.service.Supervise(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	if run.Phase != PhasePreliveVerified || h.prelive.calls != 1 {
		t.Fatalf("run=%+v prelive calls=%d", run, h.prelive.calls)
	}
	facts := ticket.snapshot()
	if facts.State != youtrack.StateForRelease || facts.AIStatus != youtrack.AIStatusInProgress {
		t.Fatalf("ticket facts = %#v", facts)
	}
	if facts.State == "Done" || facts.AIStatus == youtrack.AIStatusComplete {
		t.Fatal("auto-Done/Complete was written")
	}
}

func TestPreliveIsGatedOnARecordedPassAndSetsRelease(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne, RecordedSessionID: nativeID}}
	h := newHarness(t, ticket)
	h.liveWorker(t, commitOne)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	if _, err := h.service.Prelive(context.Background(), "CKM-825", "make check"); err == nil {
		t.Fatal("Prelive ran before review passed")
	}
	h.seed(t, Run{
		Issue: "CKM-825", Phase: PhaseAwaitingPrelive,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
		Reviews:         []Review{{Commit: commitOne, Verdict: youtrack.VerdictPass, Session: "codex-1", Model: "gpt-6.1-sol", Effort: "low"}},
	})
	ticket.update(func(f *youtrack.TicketFacts) { f.LastReviewedCommit = commitOne })
	run, err := h.service.Prelive(context.Background(), "CKM-825", "make check")
	if err != nil {
		t.Fatalf("Prelive() error = %v", err)
	}
	if run.Phase != PhasePreliveVerified {
		t.Fatalf("phase = %q", run.Phase)
	}
	if facts := ticket.snapshot(); facts.State != youtrack.StateForRelease || facts.AIStatus != youtrack.AIStatusInProgress {
		t.Fatalf("release facts = %#v", facts)
	}
}

func TestPreliveFailureBlocksTheRun(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitOne, RecordedSessionID: nativeID, LastReviewedCommit: commitOne}}
	h := newHarness(t, ticket)
	h.liveWorker(t, commitOne)
	h.prelive.err = errors.New("migration check failed")
	h.seed(t, Run{
		Issue: "CKM-825", Phase: PhaseAwaitingPrelive,
		Coder:           Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", Model: "opencode-go/deepseek-v4.1-flash"},
		SubmittedCommit: commitOne,
		Reviews:         []Review{{Commit: commitOne, Verdict: youtrack.VerdictPass, Session: "codex-1", Model: "gpt-6.1-sol"}},
	})
	run, err := h.service.Prelive(context.Background(), "CKM-825", "make check")
	if err == nil {
		t.Fatal("Prelive accepted a failing check")
	}
	if run.Phase != PhaseBlocked {
		t.Fatalf("phase = %q, want blocked", run.Phase)
	}
	if ticket.snapshot().State == youtrack.StateForRelease {
		t.Fatal("a failed pre-live check moved the ticket to For Release")
	}
}

func TestReviewRefusesWhenTheTicketHoldsAnotherCommit(t *testing.T) {
	ticket := &fakeTicket{facts: youtrack.TicketFacts{SubmittedCommit: commitTwo, RecordedSessionID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt", AIStatus: youtrack.AIStatusAwaiting}}
	h := newHarness(t, ticket)
	h.liveWorker(t, commitOne)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseAwaitingReview, SubmittedCommit: commitOne, Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	if _, err := h.service.Review(context.Background(), "CKM-825"); err == nil {
		t.Fatal("Review accepted a run commit that the ticket does not hold")
	}
	if h.reviewer.calls != 0 {
		t.Fatal("Review ran the reviewer for a stale commit")
	}
}
