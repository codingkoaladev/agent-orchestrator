package youtrack

import (
	"context"
	"strings"
	"testing"
	"time"
)

const (
	testCommit  = "0123456789abcdef0123456789abcdef01234567"
	otherCommit = "fedcba9876543210fedcba9876543210fedcba98"
	testNative  = "ses_native_aaa"
	testAO      = "pilot-2"
)

func baseFacts() Facts {
	return Facts{
		IssueID:               "CKM-825",
		CoderSessionID:        testNative,
		OrchestratorSessionID: testAO,
		Branch:                "CKM-825",
		WorktreePath:          "/home/pawel/projects/koala-orchestrator",
		CoderModel:            "opencode-go/deepseek-v4.1-flash",
		LivenessKnown:         true,
		Ticket: TicketFacts{
			State:             StateInProgress,
			AIStatus:          AIStatusInProgress,
			RecordedSessionID: testNative,
		},
	}
}

func passReview() *ReviewEvidence {
	return &ReviewEvidence{
		Commit:     testCommit,
		Verdict:    VerdictPass,
		Session:    "codex-review-1",
		Model:      "gpt-6.1-sol",
		Effort:     "low",
		Source:     "codex-cli",
		RecordedAt: time.Now(),
	}
}

func okPrelive() *PreliveEvidence {
	return &PreliveEvidence{Commit: testCommit, Command: "make prelive-check", ExitCode: 0, OutputDigest: "abc123"}
}

func TestValidateStartRequiresBothIdentitiesAndKnownLiveness(t *testing.T) {
	facts := baseFacts()
	if _, err := Validate(StageStart, facts, nil, nil); err != nil {
		t.Fatalf("start refused complete facts: %v", err)
	}
	missingNative := baseFacts()
	missingNative.CoderSessionID = ""
	if _, err := Validate(StageStart, missingNative, nil, nil); err == nil {
		t.Fatal("start accepted a missing native session id")
	}
	missingAO := baseFacts()
	missingAO.OrchestratorSessionID = ""
	if _, err := Validate(StageStart, missingAO, nil, nil); err == nil {
		t.Fatal("start accepted a missing Orchestrator session id")
	}
	unknown := baseFacts()
	unknown.LivenessKnown = false
	if _, err := Validate(StageStart, unknown, nil, nil); err == nil {
		t.Fatal("start dispatched while session liveness was unknown")
	}
}

func TestValidateStartRefusesDuplicateAcrossBothIDSpaces(t *testing.T) {
	native := baseFacts()
	native.Ticket.RecordedSessionID = "ses_other"
	native.Sessions = []SessionFact{{ID: "ses_other", Live: true}}
	if _, err := Validate(StageStart, native, nil, nil); err == nil {
		t.Fatal("start dispatched while the ticket's native session was live")
	}

	ao := baseFacts()
	ao.Ticket.RecordedSessionID = "pilot-9"
	ao.Sessions = []SessionFact{{ID: "pilot-9", Live: true}}
	if _, err := Validate(StageStart, ao, nil, nil); err == nil {
		t.Fatal("start dispatched while the ticket's Orchestrator session was live")
	}

	// Re-running the same session is idempotent.
	same := baseFacts()
	same.Sessions = []SessionFact{{ID: testAO, Live: true}}
	if _, err := Validate(StageStart, same, nil, nil); err != nil {
		t.Fatalf("start refused its own live session: %v", err)
	}
}

func TestValidateSubmitRequiresFullCommitAndRefusesReleasedTicket(t *testing.T) {
	facts := baseFacts()
	facts.Commit = "abc123"
	if _, err := Validate(StageSubmit, facts, nil, nil); err == nil {
		t.Fatal("submit accepted a short commit")
	}
	facts.Commit = testCommit
	result, err := Validate(StageSubmit, facts, nil, nil)
	if err != nil {
		t.Fatalf("submit refused a valid commit: %v", err)
	}
	// The guard binds the ticket value observed during validation, so a commit
	// that lands mid-flight makes the write retry instead of overwrite.
	if result.ExpectSubmitted != facts.Ticket.SubmittedCommit {
		t.Fatalf("submit did not bind the observed ticket commit: %#v", result)
	}
	released := facts
	released.Ticket.State = StateForRelease
	if _, err := Validate(StageSubmit, released, nil, nil); err == nil {
		t.Fatal("submit accepted a ticket already For Release")
	}
}

func TestValidateReviewBindsToTheLiveTicketAndRecordedVerdict(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit

	// No recorded review: a caller cannot assert one.
	if _, err := Validate(StageReview, facts, nil, nil); err == nil {
		t.Fatal("review accepted a caller-asserted pass with no recorded review")
	}

	// Stale: ticket holds a newer submission.
	stale := facts
	stale.Ticket.SubmittedCommit = otherCommit
	if _, err := Validate(StageReview, stale, passReview(), nil); err == nil {
		t.Fatal("review accepted evidence for a commit the ticket did not submit")
	}

	// Evidence for a different commit than the ticket's submission.
	wrongCommit := passReview()
	wrongCommit.Commit = otherCommit
	if _, err := Validate(StageReview, facts, wrongCommit, nil); err == nil {
		t.Fatal("review accepted a verdict recorded for another commit")
	}

	// Self-review by the native session.
	self := passReview()
	self.Session = testNative
	if _, err := Validate(StageReview, facts, self, nil); err == nil {
		t.Fatal("review accepted self-review from the native session")
	}

	// Self-review by the Orchestrator session.
	selfAO := passReview()
	selfAO.Session = testAO
	if _, err := Validate(StageReview, facts, selfAO, nil); err == nil {
		t.Fatal("review accepted self-review from the Orchestrator session")
	}

	// Same model as the coder.
	sameModel := passReview()
	sameModel.Model = facts.CoderModel
	if _, err := Validate(StageReview, facts, sameModel, nil); err == nil {
		t.Fatal("review accepted the coder's own model")
	}

	// Unknown verdict.
	unknown := passReview()
	unknown.Verdict = "maybe"
	if _, err := Validate(StageReview, facts, unknown, nil); err == nil {
		t.Fatal("review accepted an unknown verdict")
	}
}

func TestPassingReviewKeepsAIWorkInProgress(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit

	result, err := Validate(StageReview, facts, passReview(), nil)
	if err != nil {
		t.Fatalf("review pass refused: %v", err)
	}
	if result.Fields[FieldState] != StateCodeReview {
		t.Fatalf("pass state = %q, want %q", result.Fields[FieldState], StateCodeReview)
	}
	if result.Fields[FieldAIStatus] != AIStatusInProgress {
		t.Fatalf("pass set AI Status %q; only verified pre-live acceptance may complete AI work", result.Fields[FieldAIStatus])
	}
	if result.Fields[FieldLastReviewed] != testCommit {
		t.Fatalf("pass did not record the reviewed commit: %#v", result.Fields)
	}
}

func TestFailingReviewReturnsToCoder(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit
	review := passReview()
	review.Verdict = VerdictFail
	result, err := Validate(StageReview, facts, review, nil)
	if err != nil {
		t.Fatalf("review fail refused: %v", err)
	}
	if result.Fields[FieldAIStatus] != AIStatusFailed || result.Fields[FieldState] != StateInProgress {
		t.Fatalf("fail fields = %#v", result.Fields)
	}
}

func TestValidatePreliveRequiresPassingReviewOfTheSubmittedCommitAndRealEvidence(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit
	facts.Ticket.LastReviewedCommit = testCommit

	// The ticket's reviewed field alone must never establish a pass: an earlier
	// failed review leaves that field set.
	if _, err := Validate(StagePrelive, facts, nil, okPrelive()); err == nil {
		t.Fatal("prelive accepted a reviewed-commit field with no recorded passing review")
	}

	failed := passReview()
	failed.Verdict = VerdictFail
	if _, err := Validate(StagePrelive, facts, failed, okPrelive()); err == nil {
		t.Fatal("prelive accepted a recorded failing review")
	}

	// Reviewed commit differs from the submitted one.
	otherReviewed := facts
	otherReviewed.Ticket.LastReviewedCommit = otherCommit
	if _, err := Validate(StagePrelive, otherReviewed, passReview(), okPrelive()); err == nil {
		t.Fatal("prelive accepted a reviewed commit that is not the submitted one")
	}

	// No evidence at all.
	if _, err := Validate(StagePrelive, facts, passReview(), nil); err == nil {
		t.Fatal("prelive accepted a claim with no recorded check")
	}

	// Evidence for another commit.
	otherEvidence := okPrelive()
	otherEvidence.Commit = otherCommit
	if _, err := Validate(StagePrelive, facts, passReview(), otherEvidence); err == nil {
		t.Fatal("prelive accepted evidence for another commit")
	}

	// Arbitrary non-empty text is not evidence: a command and digest are required.
	free := &PreliveEvidence{Commit: testCommit, OutputDigest: ""}
	if _, err := Validate(StagePrelive, facts, passReview(), free); err == nil {
		t.Fatal("prelive accepted evidence with no command and digest")
	}

	// A failed check is not acceptance.
	failedCheck := okPrelive()
	failedCheck.ExitCode = 2
	if _, err := Validate(StagePrelive, facts, passReview(), failedCheck); err == nil {
		t.Fatal("prelive accepted a failed check")
	}

	result, err := Validate(StagePrelive, facts, passReview(), okPrelive())
	if err != nil {
		t.Fatalf("prelive refused valid evidence: %v", err)
	}
	if result.Fields[FieldState] != StateForRelease || result.Fields[FieldAIStatus] != AIStatusInProgress {
		t.Fatalf("prelive fields = %#v", result.Fields)
	}
	if result.ExpectSubmitted != testCommit || result.ExpectLastReviewed != testCommit {
		t.Fatalf("prelive did not bind both ticket facts: %#v", result)
	}
}

type fakeAPI struct {
	issue        Issue
	recent       []string
	written      map[string]Value
	commented    string
	commentCount int
	onGetIssue   func(n int, issue *Issue)
	gets         int
}

func (f *fakeAPI) GetIssue(context.Context, string) (Issue, error) {
	f.gets++
	if f.onGetIssue != nil {
		f.onGetIssue(f.gets, &f.issue)
	}
	issue := Issue{ID: f.issue.ID, Fields: map[string]Field{}}
	for name, field := range f.issue.Fields {
		issue.Fields[name] = field
	}
	for name, value := range f.written {
		field := issue.Fields[name]
		field.Value = value.Text
		issue.Fields[name] = field
	}
	return issue, nil
}

func (f *fakeAPI) UpdateFields(_ context.Context, _ string, fields map[string]Value) error {
	if f.written == nil {
		f.written = map[string]Value{}
	}
	for name, value := range fields {
		f.written[name] = value
	}
	return nil
}

func (f *fakeAPI) Comment(_ context.Context, _, text string) error {
	if f.commentCount > 0 && f.commented == text {
		f.commentCount++
	}
	f.commentCount++
	f.commented = text
	return nil
}

func (f *fakeAPI) RecentComments(context.Context, string, int) ([]string, error) {
	return f.recent, nil
}

func stageIssue() Issue {
	return Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState:         {Name: FieldState, Exists: true, Kind: KindState},
		FieldAIStatus:      {Name: FieldAIStatus, Exists: true, Kind: KindState},
		FieldCoderModel:    {Name: FieldCoderModel, Exists: true, Kind: KindEnum, Allowed: []string{"deepseek-v4.1-flash"}},
		FieldBranch:        {Name: FieldBranch, Exists: true, Kind: KindText},
		FieldWorktreePath:  {Name: FieldWorktreePath, Exists: true, Kind: KindText},
		FieldCodingSession: {Name: FieldCodingSession, Exists: true, Kind: KindText},
		FieldLastSubmitted: {Name: FieldLastSubmitted, Exists: true, Kind: KindText},
		FieldLastReviewed:  {Name: FieldLastReviewed, Exists: true, Kind: KindText},
	}}
}

func TestApplyAbortsWhenTheTicketSubmissionChangesMidFlight(t *testing.T) {
	api := &fakeAPI{issue: stageIssue()}
	// The first read is pre-validation; by the guard read a newer commit landed.
	api.onGetIssue = func(n int, issue *Issue) {
		if n >= 2 {
			issue.Fields[FieldLastSubmitted] = Field{Name: FieldLastSubmitted, Exists: true, Kind: KindText, Value: otherCommit}
		}
	}
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit
	result, err := Validate(StageSubmit, facts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), api, api.issue, result); err == nil {
		t.Fatal("Apply overwrote a newer submission")
	}
	if len(api.written) != 0 {
		t.Fatalf("Apply wrote fields after the guard failed: %#v", api.written)
	}
}

func TestApplyDoesNotRepeatAnIdenticalComment(t *testing.T) {
	api := &fakeAPI{issue: stageIssue()}
	result, err := Validate(StageSubmit, func() Facts {
		facts := baseFacts()
		facts.Commit = testCommit
		return facts
	}(), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.recent = []string{result.Comment}
	if _, err := Apply(context.Background(), api, api.issue, result); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if api.commented != "" {
		t.Fatal("Apply repeated an identical evidence comment")
	}
}

func TestApplySkipsUndefinedFieldsAndRejectsUnknownBundles(t *testing.T) {
	api := &fakeAPI{issue: Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState:         {Name: FieldState, Exists: true, Kind: KindState},
		FieldAIStatus:      {Name: FieldAIStatus, Exists: true, Kind: KindState},
		FieldCoderModel:    {Name: FieldCoderModel, Exists: true, Kind: KindEnum, Allowed: []string{"deepseek-v4.1-flash"}},
		FieldBranch:        {Name: FieldBranch, Exists: true, Kind: KindText},
		FieldWorktreePath:  {Name: FieldWorktreePath, Exists: true, Kind: KindText},
		FieldCodingSession: {Name: FieldCodingSession, Exists: true, Kind: KindText},
	}}}
	facts := baseFacts()
	facts.Commit = testCommit
	facts.Ticket.SubmittedCommit = testCommit
	api.issue.Fields[FieldLastSubmitted] = Field{Name: FieldLastSubmitted, Exists: true, Kind: KindText, Value: testCommit}
	result, err := Validate(StageReview, facts, passReview(), nil)
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := Apply(context.Background(), api, api.issue, result)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	want := map[string]bool{FieldReviewerEffort: true, FieldReviewSession: true, FieldLastReviewed: true}
	for _, name := range skipped {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("Apply did not report missing fields %v (skipped %v)", want, skipped)
	}
	if got := api.written[FieldCoderModel].Text; got != "" {
		t.Fatalf("review wrote the coder model: %q", got)
	}
}

func TestFieldKindMapsProjectFieldTypes(t *testing.T) {
	cases := map[string]FieldKind{
		"StateProjectCustomField":  KindState,
		"EnumProjectCustomField":   KindEnum,
		"SimpleProjectCustomField": KindText,
		"UserProjectCustomField":   KindOther,
	}
	for projectType, want := range cases {
		if got := fieldKind(projectType); got != want {
			t.Fatalf("fieldKind(%q) = %q, want %q", projectType, got, want)
		}
	}
}

func TestValidateRejectsMalformedIssueIDs(t *testing.T) {
	facts := baseFacts()
	facts.IssueID = "not-an-issue"
	if _, err := Validate(StageStart, facts, nil, nil); err == nil {
		t.Fatal("Validate accepted a malformed issue id")
	}
	if !ValidIssueID("CKM-825") {
		t.Fatal("ValidIssueID rejected a valid id")
	}
}

// The guard package has no run-store dependency: evidence is passed in by the
// coordinator, never read from a caller-controlled path here.

func TestCommentBodiesStayFreeOfSecrets(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	result, err := Validate(StageSubmit, facts, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Comment, "token") || strings.Contains(result.Comment, "Authorization") {
		t.Fatalf("comment mentions credentials: %q", result.Comment)
	}
}
