package youtrack

import (
	"context"
	"strings"
	"testing"
)

const (
	testCommit   = "0123456789abcdef0123456789abcdef01234567"
	otherCommit  = "fedcba9876543210fedcba9876543210fedcba98"
	testSession  = "ses_aaa"
	otherSession = "ses_bbb"
)

func baseFacts() Facts {
	return Facts{
		IssueID:      "CKM-825",
		SessionID:    testSession,
		Branch:       "CKM-825",
		WorktreePath: "/home/pawel/projects/koala-orchestrator",
		CoderModel:   "opencode-go/deepseek-v4.1-flash",
	}
}

func TestValidateStart(t *testing.T) {
	facts := baseFacts()
	result, err := Validate(StageStart, facts)
	if err != nil {
		t.Fatalf("Validate(start) error = %v", err)
	}
	if result.Fields[FieldState] != StateInProgress || result.Fields[FieldAIStatus] != AIStatusInProgress {
		t.Fatalf("start fields = %#v", result.Fields)
	}
	if result.Fields[FieldCodingSession] != testSession {
		t.Fatalf("start did not record the session: %#v", result.Fields)
	}
	if !strings.Contains(result.Comment, testSession) {
		t.Fatalf("start comment lacks the session id: %q", result.Comment)
	}
}

func TestValidateStartRejectsIncompleteIdentity(t *testing.T) {
	for _, mutate := range []func(*Facts){
		func(f *Facts) { f.SessionID = "" },
		func(f *Facts) { f.Branch = "" },
		func(f *Facts) { f.WorktreePath = "" },
		func(f *Facts) { f.CoderModel = "" },
	} {
		facts := baseFacts()
		mutate(&facts)
		if _, err := Validate(StageStart, facts); err == nil {
			t.Fatalf("Validate(start) accepted incomplete facts: %#v", facts)
		}
	}
}

func TestValidateStartRefusesDuplicateDispatchOfALiveSession(t *testing.T) {
	facts := baseFacts()
	facts.RecordedSessionID = "ses_existing"
	facts.Sessions = []SessionFact{{ID: "ses_existing", Live: true}}
	if _, err := Validate(StageStart, facts); err == nil {
		t.Fatal("Validate(start) dispatched a ticket whose recorded session is still live")
	}

	// A terminated recorded session may be replaced.
	facts.Sessions = []SessionFact{{ID: "ses_existing", Live: false}}
	if _, err := Validate(StageStart, facts); err != nil {
		t.Fatalf("Validate(start) refused to replace a terminated session: %v", err)
	}

	// Re-running the same session is idempotent, not a duplicate.
	facts.RecordedSessionID = testSession
	facts.Sessions = []SessionFact{{ID: testSession, Live: true}}
	if _, err := Validate(StageStart, facts); err != nil {
		t.Fatalf("Validate(start) refused its own session: %v", err)
	}
}

func TestValidateSubmitRequiresAFullCommit(t *testing.T) {
	facts := baseFacts()
	for _, commit := range []string{"", "abc123", strings.ToUpper(testCommit)} {
		facts.Commit = commit
		if _, err := Validate(StageSubmit, facts); err == nil {
			t.Fatalf("Validate(submit) accepted commit %q", commit)
		}
	}
	facts.Commit = testCommit
	result, err := Validate(StageSubmit, facts)
	if err != nil {
		t.Fatalf("Validate(submit) error = %v", err)
	}
	if result.Fields[FieldLastSubmitted] != testCommit || result.Fields[FieldAIStatus] != AIStatusAwaiting {
		t.Fatalf("submit fields = %#v", result.Fields)
	}
}

func TestValidateReviewEnforcesIndependentReview(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.ReviewedCommit = testCommit
	facts.ReviewerModel = "gpt-6.1-sol"
	facts.ReviewerEffort = "low"
	facts.ReviewerSession = otherSession
	facts.Verdict = "pass"

	result, err := Validate(StageReview, facts)
	if err != nil {
		t.Fatalf("Validate(review) error = %v", err)
	}
	if result.Fields[FieldAIStatus] != AIStatusComplete || result.Fields[FieldState] != StateCodeReview {
		t.Fatalf("review pass fields = %#v", result.Fields)
	}
	if result.Fields[FieldLastReviewed] != testCommit {
		t.Fatalf("review did not record the reviewed commit: %#v", result.Fields)
	}

	selfReview := facts
	selfReview.ReviewerSession = testSession
	if _, err := Validate(StageReview, selfReview); err == nil {
		t.Fatal("Validate(review) accepted self-review")
	}

	sameModel := facts
	sameModel.ReviewerModel = facts.CoderModel
	if _, err := Validate(StageReview, sameModel); err == nil {
		t.Fatal("Validate(review) accepted a review by the coder's own model")
	}

	staleCommit := facts
	staleCommit.ReviewedCommit = otherCommit
	if _, err := Validate(StageReview, staleCommit); err == nil {
		t.Fatal("Validate(review) accepted a review of a different commit")
	}

	noVerdict := facts
	noVerdict.Verdict = "maybe"
	if _, err := Validate(StageReview, noVerdict); err == nil {
		t.Fatal("Validate(review) accepted an unknown verdict")
	}

	failed := facts
	failed.Verdict = "fail"
	result, err = Validate(StageReview, failed)
	if err != nil {
		t.Fatalf("Validate(review fail) error = %v", err)
	}
	if result.Fields[FieldAIStatus] != AIStatusFailed || result.Fields[FieldState] != StateInProgress {
		t.Fatalf("review fail fields = %#v", result.Fields)
	}
}

func TestValidatePreliveRequiresReviewedCommitAndEvidence(t *testing.T) {
	facts := baseFacts()
	facts.Commit = testCommit
	facts.ReviewedCommit = testCommit
	facts.PreliveEvidence = "pre-live smoke on staging build 42"

	result, err := Validate(StagePrelive, facts)
	if err != nil {
		t.Fatalf("Validate(prelive) error = %v", err)
	}
	if result.Fields[FieldPreliveVerified] != testCommit {
		t.Fatalf("prelive did not record the verified commit: %#v", result.Fields)
	}

	unreviewed := facts
	unreviewed.ReviewedCommit = ""
	if _, err := Validate(StagePrelive, unreviewed); err == nil {
		t.Fatal("Validate(prelive) accepted a commit with no independent review")
	}

	otherReviewed := facts
	otherReviewed.ReviewedCommit = otherCommit
	if _, err := Validate(StagePrelive, otherReviewed); err == nil {
		t.Fatal("Validate(prelive) accepted a pre-live commit that is not the reviewed one")
	}

	noEvidence := facts
	noEvidence.PreliveEvidence = " "
	if _, err := Validate(StagePrelive, noEvidence); err == nil {
		t.Fatal("Validate(prelive) accepted a claim with no evidence")
	}
}

type fakeAPI struct {
	issue       Issue
	written     map[string]string
	commented   string
	readbacks   int
	failComment bool
}

func (f *fakeAPI) GetIssue(context.Context, string) (Issue, error) {
	f.readbacks++
	issue := Issue{ID: f.issue.ID, Fields: map[string]Field{}}
	for name, field := range f.issue.Fields {
		issue.Fields[name] = field
	}
	for name, value := range f.written {
		field := issue.Fields[name]
		field.Value = value
		issue.Fields[name] = field
	}
	return issue, nil
}

func (f *fakeAPI) UpdateFields(_ context.Context, _ string, fields map[string]string) error {
	if f.written == nil {
		f.written = map[string]string{}
	}
	for name, value := range fields {
		f.written[name] = value
	}
	return nil
}

func (f *fakeAPI) Comment(_ context.Context, _, text string) error {
	if f.failComment {
		return errComment
	}
	f.commented = text
	return nil
}

var errComment = &testError{"comment failed"}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }

func TestApplySkipsUndefinedFieldsAndVerifiesReadback(t *testing.T) {
	api := &fakeAPI{issue: Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState:         {Name: FieldState, Exists: true, Allowed: []string{"In Progress", "Code Review"}},
		FieldAIStatus:      {Name: FieldAIStatus, Exists: true, Allowed: []string{"In Progress", "Complete"}},
		FieldCoderModel:    {Name: FieldCoderModel, Exists: true, Allowed: []string{"deepseek-v4.1-flash"}},
		FieldBranch:        {Name: FieldBranch, Exists: true},
		FieldWorktreePath:  {Name: FieldWorktreePath, Exists: true},
		FieldCodingSession: {Name: FieldCodingSession, Exists: true},
	}}}
	result, err := Validate(StageStart, baseFacts())
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := Apply(context.Background(), api, api.issue, result)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("Apply() skipped %v, want none", skipped)
	}
	if api.commented == "" {
		t.Fatal("Apply() wrote no evidence comment")
	}
	if api.written[FieldState] != StateInProgress {
		t.Fatalf("Apply() wrote %#v", api.written)
	}
	// The ticket bundle stores bare model ids while the agent needs the
	// provider-qualified one, so Apply must land the bare choice.
	if api.written[FieldCoderModel] != "deepseek-v4.1-flash" {
		t.Fatalf("coder model written as %q, want the bundle choice", api.written[FieldCoderModel])
	}
}

func TestApplyReportsMissingSchemaInsteadOfFakingIt(t *testing.T) {
	api := &fakeAPI{issue: Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState:         {Name: FieldState, Exists: true},
		FieldAIStatus:      {Name: FieldAIStatus, Exists: true},
		FieldReviewerModel: {Name: FieldReviewerModel, Exists: true},
	}}}
	facts := baseFacts()
	facts.Commit = testCommit
	facts.ReviewedCommit = testCommit
	facts.ReviewerModel = "gpt-6.1-sol"
	facts.ReviewerSession = otherSession
	facts.Verdict = "pass"
	result, err := Validate(StageReview, facts)
	if err != nil {
		t.Fatal(err)
	}
	skipped, err := Apply(context.Background(), api, api.issue, result)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	// The owner has not created these fields yet; they must be reported, not
	// invented, and must not fail the transition.
	want := map[string]bool{FieldReviewerEffort: true, FieldReviewSession: true, FieldLastReviewed: true}
	for _, name := range skipped {
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("Apply() did not report missing fields %v (skipped %v)", want, skipped)
	}
}

func TestApplyRejectsBundleValuesTheProjectDoesNotOffer(t *testing.T) {
	api := &fakeAPI{issue: Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState:      {Name: FieldState, Exists: true, Allowed: []string{"In Progress"}},
		FieldAIStatus:   {Name: FieldAIStatus, Exists: true, Allowed: []string{"In Progress"}},
		FieldCoderModel: {Name: FieldCoderModel, Exists: true, Allowed: []string{"deepseek-v4.1-flash"}},
	}}}
	result, err := Validate(StageStart, baseFacts())
	if err != nil {
		t.Fatal(err)
	}
	// A start transition writes only offered values, so make one value invalid.
	result.Fields[FieldAIStatus] = "Not A Real State"
	if _, err := Apply(context.Background(), api, api.issue, result); err == nil {
		t.Fatal("Apply() accepted a value the bundle does not offer")
	}
}

func TestApplyCommentFailureStopsFieldWrites(t *testing.T) {
	api := &fakeAPI{issue: Issue{ID: "CKM-825", Fields: map[string]Field{
		FieldState: {Name: FieldState, Exists: true},
	}}, failComment: true}
	result, err := Validate(StageStart, baseFacts())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), api, api.issue, result); err == nil {
		t.Fatal("Apply() ignored a failed evidence comment")
	}
	if len(api.written) != 0 {
		t.Fatalf("Apply() wrote fields despite a failed comment: %#v", api.written)
	}
}
