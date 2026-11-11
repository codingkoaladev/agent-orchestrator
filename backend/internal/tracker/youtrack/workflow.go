package youtrack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Ticket field names. Only fields that already exist on the project are
// written; the bridge reports the rest instead of inventing schema.
const (
	FieldState           = "State"
	FieldAIStatus        = "AI Status"
	FieldCoderModel      = "AI Coder Model ID"
	FieldReviewerModel   = "AI Reviewer Model"
	FieldAutomation      = "AI Automation Enabled"
	FieldBranch          = "AI Branch"
	FieldWorktreePath    = "AI Worktree Path"
	FieldCodingSession   = "AI Coding Session ID"
	FieldLastSubmitted   = "AI Last Submitted Commit"
	FieldReviewerEffort  = "AI Reviewer Effort"
	FieldReviewSession   = "AI Review Session ID"
	FieldLastReviewed    = "AI Last Reviewed Commit"
	FieldPreliveVerified = "AI Pre-live Verified Commit"
)

// Ticket states this bridge may set. They mirror the live CKM schema; a state
// the project does not offer is skipped and reported.
const (
	StateInProgress = "In Progress"
	StateCodeReview = "Code Review"
	StateForRelease = "For Release"

	AIStatusInProgress = "In Progress"
	AIStatusAwaiting   = "Awaiting AI Review"
	AIStatusFailed     = "Failed AI Review - Corrections Needed"
	// AIStatusComplete is set only by a verified pre-live acceptance. Passing
	// code review keeps the AI work active: deployment and pre-live testing are
	// still outstanding at that point.
	AIStatusComplete = "Complete"
)

// Verdicts a recorded independent review can carry.
const (
	VerdictPass = "pass"
	VerdictFail = "fail"
)

// Stage is one workflow transition the coordinator may request.
type Stage string

const (
	StageStart   Stage = "start"
	StageSubmit  Stage = "submit"
	StageReview  Stage = "review"
	StagePrelive Stage = "prelive"
)

// SessionFact is one Orchestrator session as the daemon reports it.
type SessionFact struct {
	NativeID     string
	ID           string
	Branch       string
	WorktreePath string
	Live         bool
}

// TicketFacts are the authoritative values read from YouTrack immediately
// before the transition. Transitions are bound to these, never to a caller's
// claim about what the ticket says.
type TicketFacts struct {
	State              string
	AIStatus           string
	SubmittedCommit    string
	LastReviewedCommit string
	VerifiedCommit     string
	RecordedSessionID  string
	CoderModel         string
}

// ReviewEvidence is a verdict recorded by the coordinator after it actually ran
// the independent reviewer. The bridge never accepts a caller-supplied verdict
// or reviewed commit: those are the facts a review would otherwise invent.
type ReviewEvidence struct {
	Commit     string
	Verdict    string
	Session    string
	Model      string
	Effort     string
	Source     string
	RecordedAt time.Time
}

// PreliveEvidence is a pre-live check the coordinator actually executed. Free
// text is not evidence: a commit, the command, its exit code and a digest of its
// output are.
type PreliveEvidence struct {
	Commit       string
	Command      string
	ExitCode     int
	OutputDigest string
	RecordedAt   time.Time
}

// PreliveVerified reports whether the check ran successfully.
func (e PreliveEvidence) PreliveVerified() bool {
	return e.ExitCode == 0 &&
		strings.TrimSpace(e.Command) != "" &&
		strings.TrimSpace(e.OutputDigest) != "" &&
		commitPattern.MatchString(strings.TrimSpace(e.Commit))
}

// Facts are the observed facts a transition must be consistent with.
type Facts struct {
	IssueID string
	// CoderSessionID is the agent-native id (for example an OpenCode session).
	// OrchestratorSessionID is the daemon session id. Both are persisted because
	// liveness is only answerable for the daemon id while the ticket records the
	// native one.
	CoderSessionID        string
	OrchestratorSessionID string
	Branch                string
	WorktreePath          string
	CoderModel            string

	Commit   string
	Ticket   TicketFacts
	Sessions []SessionFact
	// LivenessKnown is true only when the daemon answered the session listing.
	// Unknown liveness must never authorize another dispatch.
	LivenessKnown bool
}

// Result is a validated transition: the field values to write, the evidence
// comment body, and the ticket values that must still hold at write time.
type Result struct {
	Stage   Stage
	Fields  map[string]string
	Comment string
	// ExpectSubmitted and ExpectLastReviewed hold the values observed during
	// validation and are re-checked immediately before the field write, so a
	// concurrent submission cannot be overwritten.
	ExpectSubmitted    string
	ExpectLastReviewed string
}

var (
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	issueIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
)

// ValidIssueID reports whether id looks like a YouTrack issue id.
func ValidIssueID(id string) bool { return issueIDPattern.MatchString(strings.TrimSpace(id)) }

// Validate applies the workflow guards for stage and returns the write set.
// It never performs I/O, so every rule is unit-testable.
func Validate(stage Stage, facts Facts, review *ReviewEvidence, prelive *PreliveEvidence) (Result, error) {
	if !ValidIssueID(facts.IssueID) {
		return Result{}, fmt.Errorf("invalid issue id %q", facts.IssueID)
	}
	switch stage {
	case StageStart:
		return validateStart(facts)
	case StageSubmit:
		return validateSubmit(facts)
	case StageReview:
		return validateReview(facts, review)
	case StagePrelive:
		return validatePrelive(facts, review, prelive)
	default:
		return Result{}, fmt.Errorf("unknown stage %q", stage)
	}
}

func validateStart(facts Facts) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
	}
	if !facts.LivenessKnown {
		return Result{}, errors.New("session liveness is unknown; refusing to dispatch until the daemon answers")
	}
	if err := checkDuplicateDispatch(facts); err != nil {
		return Result{}, err
	}
	fields := map[string]string{
		FieldState:         StateInProgress,
		FieldAIStatus:      AIStatusInProgress,
		FieldCoderModel:    facts.CoderModel,
		FieldBranch:        facts.Branch,
		FieldWorktreePath:  facts.WorktreePath,
		FieldCodingSession: facts.CoderSessionID,
	}
	comment := fmt.Sprintf("Implementation started.\n\n- Orchestrator session: `%s`\n- Native session: `%s`\n- Branch: `%s`\n- Worktree: `%s`\n- Coder model: `%s`",
		facts.OrchestratorSessionID, facts.CoderSessionID, facts.Branch, facts.WorktreePath, facts.CoderModel)
	return Result{Stage: StageStart, Fields: fields, Comment: comment}, nil
}

func validateSubmit(facts Facts) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
	}
	if err := requireCommit(facts.Commit, "submitted commit"); err != nil {
		return Result{}, err
	}
	if facts.Ticket.State == StateForRelease {
		return Result{}, errors.New("ticket is already For Release; a new submission needs an owner-authorized state change")
	}
	if !facts.LivenessKnown {
		return Result{}, errors.New("session liveness is unknown; refusing to submit for review")
	}
	if err := checkDuplicateDispatch(facts); err != nil {
		return Result{}, err
	}
	fields := map[string]string{
		FieldState:         StateInProgress,
		FieldAIStatus:      AIStatusAwaiting,
		FieldLastSubmitted: facts.Commit,
		FieldBranch:        facts.Branch,
		FieldWorktreePath:  facts.WorktreePath,
		FieldCodingSession: facts.CoderSessionID,
	}
	comment := fmt.Sprintf("Submitted for AI review.\n\n- Commit: `%s`\n- Orchestrator session: `%s`\n- Native session: `%s`\n- Branch: `%s`\n- Worktree: `%s`\n\nIndependent review by a different model is still required.",
		facts.Commit, facts.OrchestratorSessionID, facts.CoderSessionID, facts.Branch, facts.WorktreePath)
	return Result{Stage: StageSubmit, Fields: fields, Comment: comment, ExpectSubmitted: facts.Ticket.SubmittedCommit}, nil
}

func validateReview(facts Facts, review *ReviewEvidence) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
	}
	if err := requireCommit(facts.Commit, "submitted commit"); err != nil {
		return Result{}, err
	}
	// The ticket is the authority on what is under review. A caller that names a
	// different commit is either stale or trying to review something the ticket
	// never submitted.
	if facts.Ticket.SubmittedCommit != facts.Commit {
		return Result{}, fmt.Errorf("ticket records submitted commit %q, not %q", facts.Ticket.SubmittedCommit, facts.Commit)
	}
	if review == nil {
		return Result{}, errors.New("no recorded independent review for this commit; run the reviewer first")
	}
	if review.Commit != facts.Commit {
		return Result{}, fmt.Errorf("recorded review covers %s, not the submitted commit %s", review.Commit, facts.Commit)
	}
	switch review.Verdict {
	case VerdictPass, VerdictFail:
	default:
		return Result{}, fmt.Errorf("recorded review verdict %q is not passing or failing", review.Verdict)
	}
	if strings.TrimSpace(review.Session) == "" {
		return Result{}, errors.New("recorded review has no reviewer session id")
	}
	if review.Session == facts.OrchestratorSessionID || review.Session == facts.CoderSessionID {
		return Result{}, errors.New("recorded review ran in the coding session: self-review is not independent")
	}
	if strings.TrimSpace(review.Model) == "" {
		return Result{}, errors.New("recorded review has no reviewer model")
	}
	if review.Model == facts.CoderModel {
		return Result{}, fmt.Errorf("reviewer model %q equals the coder model: not independent", review.Model)
	}
	reviewerFields := map[string]string{
		FieldReviewerModel:  review.Model,
		FieldReviewerEffort: review.Effort,
		FieldReviewSession:  review.Session,
		FieldLastReviewed:   review.Commit,
	}
	if review.Verdict == VerdictFail {
		fields := map[string]string{
			FieldState:    StateInProgress,
			FieldAIStatus: AIStatusFailed,
		}
		for name, value := range reviewerFields {
			fields[name] = value
		}
		comment := fmt.Sprintf("Independent review failed; corrections needed.\n\n- Reviewed commit: `%s`\n- Reviewer: `%s` (effort `%s`)\n- Reviewer session: `%s`\n\nCorrections must be submitted as a new commit; the next review must cite that commit.",
			review.Commit, review.Model, orDefault(review.Effort, "default"), review.Session)
		return Result{Stage: StageReview, Fields: fields, Comment: comment, ExpectSubmitted: facts.Ticket.SubmittedCommit}, nil
	}
	// A passing review is not completion. State moves to Code Review and the AI
	// work stays In Progress until pre-live acceptance is proven.
	fields := map[string]string{
		FieldState:    StateCodeReview,
		FieldAIStatus: AIStatusInProgress,
	}
	for name, value := range reviewerFields {
		fields[name] = value
	}
	comment := fmt.Sprintf("Independent review passed.\n\n- Reviewed commit: `%s`\n- Reviewer: `%s` (effort `%s`)\n- Reviewer session: `%s`\n\nAI work stays active: deployment and pre-live acceptance are still required before a release claim.",
		review.Commit, review.Model, orDefault(review.Effort, "default"), review.Session)
	return Result{Stage: StageReview, Fields: fields, Comment: comment, ExpectSubmitted: facts.Ticket.SubmittedCommit}, nil
}

func validatePrelive(facts Facts, review *ReviewEvidence, prelive *PreliveEvidence) (Result, error) {
	if err := requireCommit(facts.Commit, "pre-live verified commit"); err != nil {
		return Result{}, err
	}
	submitted := facts.Ticket.SubmittedCommit
	if !commitPattern.MatchString(strings.TrimSpace(submitted)) {
		return Result{}, errors.New("ticket has no submitted commit; nothing can be pre-live verified")
	}
	if facts.Commit != submitted {
		return Result{}, fmt.Errorf("pre-live commit %s is not the submitted commit %s", facts.Commit, submitted)
	}
	if facts.Ticket.LastReviewedCommit != submitted {
		return Result{}, fmt.Errorf("ticket records reviewed commit %q, not the submitted commit %s; a passing review of this commit must be recorded first", facts.Ticket.LastReviewedCommit, submitted)
	}
	if review == nil || review.Commit != submitted || review.Verdict != VerdictPass {
		return Result{}, errors.New("no recorded passing independent review for the submitted commit")
	}
	if prelive == nil {
		return Result{}, errors.New("no recorded pre-live check for this commit")
	}
	if prelive.Commit != submitted {
		return Result{}, fmt.Errorf("pre-live evidence covers %s, not the submitted commit %s", prelive.Commit, submitted)
	}
	if !prelive.PreliveVerified() {
		return Result{}, errors.New("pre-live evidence is not a successful recorded check (command, exit code 0 and output digest are required)")
	}
	fields := map[string]string{
		FieldState:           StateForRelease,
		FieldAIStatus:        AIStatusInProgress,
		FieldPreliveVerified: submitted,
	}
	comment := fmt.Sprintf("Pre-live acceptance verified; ready for go-live authorization.\n\n- Commit: `%s`\n- Check: `%s` (exit 0)\n- Output digest: `%s`\n\nAI Status remains In Progress because the current completion automation can mark State Done prematurely. Production deployment remains a separate owner-authorized step.",
		submitted, prelive.Command, prelive.OutputDigest)
	return Result{
		Stage:              StagePrelive,
		Fields:             fields,
		Comment:            comment,
		ExpectSubmitted:    submitted,
		ExpectLastReviewed: facts.Ticket.LastReviewedCommit,
	}, nil
}

func requireIdentity(facts Facts) error {
	if strings.TrimSpace(facts.CoderSessionID) == "" {
		return errors.New("native coding session id is required")
	}
	if strings.TrimSpace(facts.OrchestratorSessionID) == "" {
		return errors.New("Orchestrator session id is required")
	}
	if strings.TrimSpace(facts.Branch) == "" {
		return errors.New("branch is required")
	}
	if strings.TrimSpace(facts.WorktreePath) == "" {
		return errors.New("worktree path is required")
	}
	if strings.TrimSpace(facts.CoderModel) == "" {
		return errors.New("coder model is required")
	}
	return nil
}

func requireCommit(value, label string) error {
	if !commitPattern.MatchString(strings.TrimSpace(value)) {
		return fmt.Errorf("%s must be a full 40-character lowercase SHA, got %q", label, value)
	}
	return nil
}

// checkDuplicateDispatch refuses a dispatch while the ticket's recorded session
// is still live in any id space.
func checkDuplicateDispatch(facts Facts) error {
	recorded := strings.TrimSpace(facts.Ticket.RecordedSessionID)
	if recorded == "" {
		return nil
	}
	if recorded == facts.CoderSessionID || recorded == facts.OrchestratorSessionID {
		return nil // re-running the same session is idempotent, not a duplicate
	}
	for _, session := range facts.Sessions {
		if session.ID != recorded && session.ID != facts.OrchestratorSessionID {
			continue
		}
		if session.Live {
			return fmt.Errorf("ticket already has live session %s; refusing to dispatch %s", session.ID, facts.OrchestratorSessionID)
		}
	}
	return nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// resolveFieldValue maps an internal value onto a bundle choice.
//
// Agent CLIs need provider-qualified model ids ("opencode-go/deepseek-v4.1-flash")
// but the ticket bundle stores bare ones ("deepseek-v4.1-flash"), so a model
// field accepts either the exact value or, for provider-qualified ids, its bare
// suffix when the bundle offers it.
func resolveFieldValue(name, value string, allowed []string) (string, bool) {
	if len(allowed) == 0 {
		return value, true
	}
	if slices.Contains(allowed, value) {
		return value, true
	}
	if name == FieldCoderModel || name == FieldReviewerModel {
		if idx := strings.LastIndex(value, "/"); idx >= 0 {
			if bare := value[idx+1:]; slices.Contains(allowed, bare) {
				return bare, true
			}
		}
	}
	return "", false
}

// API is the subset of the YouTrack client Apply needs. It is an interface so
// the write-then-readback contract stays unit-testable.
type API interface {
	GetIssue(ctx context.Context, issueID string) (Issue, error)
	UpdateFields(ctx context.Context, issueID string, fields map[string]Value) error
	Comment(ctx context.Context, issueID, text string) error
	RecentComments(ctx context.Context, issueID string, limit int) ([]string, error)
}

// Apply writes the transition, skipping fields the project does not define and
// reporting each skipped field so an owner schema gap is visible, not silent.
//
// Order: compare-and-set against the ticket, evidence comment, fields, readback.
// The compare-and-set runs before the comment so a stale transition leaves no
// trace, and again before the field write so a submission that lands mid-flight
// cannot be overwritten.
func Apply(ctx context.Context, api API, issue Issue, result Result) (skipped []string, err error) {
	write := map[string]Value{}
	for name, value := range result.Fields {
		field, ok := issue.Fields[name]
		if !ok || !field.Exists {
			skipped = append(skipped, name)
			continue
		}
		resolved, ok := resolveFieldValue(name, value, field.Allowed)
		if !ok {
			return skipped, fmt.Errorf("field %q does not allow value %q; allowed: %s", name, value, strings.Join(field.Allowed, ", "))
		}
		if field.Kind == KindOther {
			return skipped, fmt.Errorf("field %q has a custom-field type this bridge cannot write", name)
		}
		write[name] = Value{Text: resolved, Kind: field.Kind}
	}
	if err := checkGuards(ctx, api, issue.ID, result); err != nil {
		return skipped, err
	}
	if result.Comment != "" && !commentAlreadyPresent(ctx, api, issue.ID, result.Comment) {
		if err := api.Comment(ctx, issue.ID, result.Comment); err != nil {
			return skipped, err
		}
	}
	if len(write) == 0 {
		return skipped, nil
	}
	// Re-check immediately before the write: the comment takes time, and a new
	// submission during it must win over this transition.
	if err := checkGuards(ctx, api, issue.ID, result); err != nil {
		return skipped, err
	}
	if err := api.UpdateFields(ctx, issue.ID, write); err != nil {
		return skipped, err
	}
	readback, err := api.GetIssue(ctx, issue.ID)
	if err != nil {
		return skipped, fmt.Errorf("read back %s: %w", issue.ID, err)
	}
	for name, want := range write {
		if got := readback.Fields[name].Value; got != want.Text {
			return skipped, fmt.Errorf("readback mismatch for %q: wrote %q, ticket shows %q", name, want.Text, got)
		}
	}
	return skipped, nil
}

// checkGuards re-reads the ticket and verifies the values this transition is
// bound to are unchanged.
func checkGuards(ctx context.Context, api API, issueID string, result Result) error {
	if result.ExpectSubmitted == "" && result.ExpectLastReviewed == "" {
		return nil
	}
	current, err := api.GetIssue(ctx, issueID)
	if err != nil {
		return fmt.Errorf("confirm ticket state: %w", err)
	}
	if result.ExpectSubmitted != "" {
		if got := current.Fields[FieldLastSubmitted].Value; got != result.ExpectSubmitted {
			return fmt.Errorf("ticket submitted commit changed to %q while this transition was in flight; re-read the ticket and retry", got)
		}
	}
	if result.ExpectLastReviewed != "" {
		if got := current.Fields[FieldLastReviewed].Value; got != result.ExpectLastReviewed {
			return fmt.Errorf("ticket reviewed commit changed to %q while this transition was in flight; re-read the ticket and retry", got)
		}
	}
	return nil
}

func commentAlreadyPresent(ctx context.Context, api API, issueID, body string) bool {
	recent, err := api.RecentComments(ctx, issueID, 3)
	if err != nil {
		return false
	}
	for _, existing := range recent {
		if strings.TrimSpace(existing) == strings.TrimSpace(body) {
			return true
		}
	}
	return false
}
