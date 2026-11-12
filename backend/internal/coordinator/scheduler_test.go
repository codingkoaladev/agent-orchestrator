package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

var scheduleNow = time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

func scheduledTicket(at time.Time) *fakeTicket {
	return &fakeTicket{facts: youtrack.TicketFacts{
		State:             youtrack.StateInProgress,
		AIStatus:          youtrack.AIStatusAwaiting,
		SubmittedCommit:   commitOne,
		RecordedSessionID: nativeID,
		Branch:            "CKM-825",
		WorktreePath:      "/tmp/wt",
		ReviewRequestedAt: at.UTC().Format("2006-01-02 15:04:05"),
	}}
}

func scheduledScheduler(h *harness) *Scheduler {
	return &Scheduler{
		Service:       h.service,
		Interval:      5 * time.Minute,
		Window:        72 * time.Hour,
		TicketTimeout: time.Minute,
		Location:      time.UTC,
		Now:           func() time.Time { return scheduleNow },
	}
}

func TestParseReviewRequestedAtUsesTheSuppliedTimezone(t *testing.T) {
	london, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skipf("timezone data unavailable: %v", err)
	}
	parsed, err := ParseReviewRequestedAt("2026-07-01 10:00:00", london)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.UTC().Format(time.RFC3339), "2026-07-01T09:00:00Z"; got != want {
		t.Fatalf("parsed = %s, want %s", got, want)
	}
	if _, err := ParseReviewRequestedAt("", time.UTC); err == nil {
		t.Fatal("empty Review Requested At was accepted")
	}
}

func TestEvaluateScheduledReviewUsesReviewRequestedAtAndWindow(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-time.Hour))
	state := func(at string) TicketState {
		facts := ticket.facts
		facts.ReviewRequestedAt = at
		return TicketState{Facts: facts}
	}
	run := Run{Issue: "CKM-825", Phase: PhaseCoding, Approved: true,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}

	if got := EvaluateScheduledReview(run, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); !got.Eligible {
		t.Fatalf("recent request not eligible: %s", got.Reason)
	}
	if got := EvaluateScheduledReview(run, state(scheduleNow.Add(-72*time.Hour).Format("2006-01-02 15:04:05")), scheduleNow, 72*time.Hour, time.UTC); !got.Eligible {
		t.Fatalf("exactly-at-window request should be eligible: %s", got.Reason)
	}
	if got := EvaluateScheduledReview(run, state(scheduleNow.Add(-72*time.Hour-time.Second).Format("2006-01-02 15:04:05")), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("stale request was eligible")
	}
	if got := EvaluateScheduledReview(run, state(scheduleNow.Add(time.Minute).Format("2006-01-02 15:04:05")), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("future request was eligible")
	}
	if got := EvaluateScheduledReview(run, state(""), scheduleNow, 72*time.Hour, time.UTC); got.Eligible || !strings.Contains(got.Reason, "workflow") {
		t.Fatalf("missing workflow timestamp was not reported: %+v", got)
	}

	unapproved := run
	unapproved.Approved = false
	if got := EvaluateScheduledReview(unapproved, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("unapproved run was eligible")
	}

	reviewed := run
	reviewed.Reviews = []Review{{Commit: commitOne, Verdict: youtrack.VerdictFail}}
	if got := EvaluateScheduledReview(reviewed, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("already reviewed commit was eligible again")
	}

	inFlight := run
	inFlight.ReviewInFlight = commitOne
	if got := EvaluateScheduledReview(inFlight, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("in-flight review was eligible again")
	}

	sameRequest := run
	sameRequest.LastReviewRequestAt = ticket.facts.ReviewRequestedAt
	if got := EvaluateScheduledReview(sameRequest, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("an unchanged Review Requested At authorized another review")
	}

	newerRequest := run
	newerRequest.LastReviewRequestAt = scheduleNow.Add(-2 * time.Hour).Format("2006-01-02 15:04:05")
	if got := EvaluateScheduledReview(newerRequest, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); !got.Eligible {
		t.Fatalf("a newer Review Requested At was not eligible: %s", got.Reason)
	}

	corruptRequest := run
	corruptRequest.LastReviewRequestAt = "not-a-date"
	if got := EvaluateScheduledReview(corruptRequest, state(ticket.facts.ReviewRequestedAt), scheduleNow, 72*time.Hour, time.UTC); got.Eligible {
		t.Fatal("an unparseable LastReviewRequestAt authorized another review")
	}
}

func TestSchedulerSweepReviewsEligibleApprovedRunOnce(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-5 * time.Minute))
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Project: "ckadmin", Phase: PhaseCoding,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	scheduler := scheduledScheduler(h)

	report := scheduler.Sweep(context.Background())
	if report.Scanned != 1 || report.Eligible != 1 || report.Reviewed != 1 || report.Failed != 0 {
		t.Fatalf("first sweep = %+v", report)
	}
	if h.reviewer.calls != 1 {
		t.Fatalf("reviewer calls = %d", h.reviewer.calls)
	}
	if report.Results[0].ReviewID != "codex-thread-1" || report.Results[0].Commit != commitOne {
		t.Fatalf("result = %+v", report.Results[0])
	}

	report = scheduler.Sweep(context.Background())
	if report.Reviewed != 0 || report.Skipped != 1 {
		t.Fatalf("second sweep = %+v", report)
	}
	if h.reviewer.calls != 1 {
		t.Fatalf("scheduler duplicated the review: %d calls", h.reviewer.calls)
	}
}

func TestSchedulerSkipsUnapprovedMissingTimestampAndStaleRequests(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-time.Hour))
	h := newHarness(t, ticket)
	if err := h.store.Save(Run{Issue: "CKM-1", Phase: PhaseCoding,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.Save(Run{Issue: "CKM-2", Phase: PhaseCoding, Approved: true,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	stale := scheduledTicket(scheduleNow.Add(-73 * time.Hour))
	missing := ticket.facts
	missing.ReviewRequestedAt = ""
	if err := h.store.Save(Run{Issue: "CKM-3", Phase: PhaseCoding, Approved: true, SubmittedCommit: commitOne,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}}); err != nil {
		t.Fatal(err)
	}
	h.liveWorker(t, commitOne)
	scheduler := scheduledScheduler(h)
	scheduler.Service.Tickets = &multiTicket{
		byID: map[string]youtrack.TicketFacts{"CKM-1": ticket.facts, "CKM-2": missing, "CKM-3": stale.facts},
	}
	report := scheduler.Sweep(context.Background())
	if report.Reviewed != 0 || report.Skipped != 3 || h.reviewer.calls != 0 {
		t.Fatalf("report=%+v reviewer=%d", report, h.reviewer.calls)
	}
}

type multiTicket struct {
	byID map[string]youtrack.TicketFacts
}

func (m *multiTicket) Read(_ context.Context, issue string) (TicketState, error) {
	facts, ok := m.byID[issue]
	if !ok {
		return TicketState{}, errors.New("unknown issue")
	}
	return TicketState{Facts: facts}, nil
}

func (m *multiTicket) Apply(context.Context, youtrack.Stage, youtrack.Facts, *youtrack.ReviewEvidence, *youtrack.PreliveEvidence) ([]string, error) {
	return nil, nil
}

func TestSchedulerSkipsWhenAnotherSupervisorOwnsTheTicket(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-time.Minute))
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Approved: true,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	lock, err := h.store.Lock(context.Background(), "CKM-825")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	report := scheduledScheduler(h).Sweep(context.Background())
	if report.Reviewed != 0 || report.Skipped != 1 || h.reviewer.calls != 0 {
		t.Fatalf("report=%+v reviewer=%d", report, h.reviewer.calls)
	}
}

func TestSchedulerNeverDuplicatesTheCKM830PilotWithoutApproval(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-time.Minute))
	h := newHarness(t, ticket)
	if err := h.store.Save(Run{Issue: "CKM-830", Project: "magento", Phase: PhaseCoding,
		Coder: Session{OrchestratorID: "magento-1", NativeID: "ses_e314bfd54ffe4l8tmmGODe491M", Branch: "CKM-830", WorktreePath: "/home/pawel/.ao/data/worktrees/magento/magento-1"}}); err != nil {
		t.Fatal(err)
	}
	h.liveWorker(t, commitOne)
	h.reviewer.results = []ReviewResult{{Verdict: youtrack.VerdictPass}}
	report := scheduledScheduler(h).Sweep(context.Background())
	if report.Reviewed != 0 || h.reviewer.calls != 0 || len(h.sessions.messages()) != 0 {
		t.Fatalf("pilot was duplicated: report=%+v reviewer=%d sends=%d", report, h.reviewer.calls, len(h.sessions.messages()))
	}
}

func TestReviewOnceRechecksReviewRequestedAt(t *testing.T) {
	ticket := scheduledTicket(scheduleNow.Add(-time.Minute))
	ticket.facts.ReviewRequestedAt = ""
	h := newHarness(t, ticket)
	h.seed(t, Run{Issue: "CKM-825", Phase: PhaseCoding, Approved: true,
		Coder: Session{OrchestratorID: "pilot-1", NativeID: nativeID, Branch: "CKM-825", WorktreePath: "/tmp/wt"}})
	h.liveWorker(t, commitOne)
	_, reviewed, err := h.service.ReviewOnce(context.Background(), "CKM-825", ScheduledReviewRequest{
		Window: 72 * time.Hour, Location: time.UTC, Now: scheduleNow,
	})
	if reviewed || !errors.Is(err, ErrNotEligible) || h.reviewer.calls != 0 {
		t.Fatalf("reviewed=%v err=%v calls=%d", reviewed, err, h.reviewer.calls)
	}
}

func TestSchedulerRefusesAnIntervalFasterThanOneMinute(t *testing.T) {
	scheduler := &Scheduler{Service: &Service{}, Interval: 30 * time.Second}
	if err := scheduler.validate(); err == nil {
		t.Fatal("sub-minute scheduler interval was accepted")
	}
	if err := (&Scheduler{Service: &Service{}, Interval: 5 * time.Minute}).validate(); err != nil {
		t.Fatalf("five-minute interval rejected: %v", err)
	}
}

func TestStoreListReturnsOnlyRunRecords(t *testing.T) {
	store := Store{Dir: t.TempDir()}
	if err := store.Save(Run{Issue: "CKM-1", Phase: PhaseCoding, Approved: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(Run{Issue: "CKM-2", Phase: PhaseBlocked, Approved: false}); err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background(), "CKM-1")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Release()
	runs, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs = %+v", runs)
	}
}
