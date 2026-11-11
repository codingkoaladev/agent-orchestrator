package youtrack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
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
	AIStatusComplete   = "Complete"
)

// Stage is one workflow transition the coordinator may request.
type Stage string

const (
	StageStart   Stage = "start"
	StageSubmit  Stage = "submit"
	StageReview  Stage = "review"
	StagePrelive Stage = "prelive"
)

// SessionFact is one Orchestrator session as the daemon reports it. The bridge
// compares these against the ticket so a transition cannot claim a session or
// worktree the daemon does not know about.
type SessionFact struct {
	ID           string
	Branch       string
	WorktreePath string
	Live         bool
}

// Facts are the observed facts a transition must be consistent with.
type Facts struct {
	IssueID      string
	SessionID    string
	Branch       string
	WorktreePath string
	CoderModel   string

	Commit          string
	ReviewedCommit  string
	ReviewerSession string
	ReviewerModel   string
	ReviewerEffort  string
	Verdict         string // "pass" or "fail" for StageReview
	PreliveEvidence string

	// RecordedSessionID is the ticket's current AI Coding Session ID.
	RecordedSessionID string
	// Sessions are the daemon's known sessions, used for duplicate dispatch.
	Sessions []SessionFact
}

// Result is a validated transition: the field values to write and the evidence
// comment body.
type Result struct {
	Stage   Stage
	Fields  map[string]string
	Comment string
}

var (
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	issueIDPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[0-9]+$`)
	verdictPass    = "pass"
	verdictFail    = "fail"
)

// ValidIssueID reports whether id looks like a YouTrack issue id.
func ValidIssueID(id string) bool { return issueIDPattern.MatchString(strings.TrimSpace(id)) }

// Validate applies the workflow guards for stage and returns the write set.
// It never performs I/O, so every rule is unit-testable.
func Validate(stage Stage, facts Facts) (Result, error) {
	if !ValidIssueID(facts.IssueID) {
		return Result{}, fmt.Errorf("invalid issue id %q", facts.IssueID)
	}
	switch stage {
	case StageStart:
		return validateStart(facts)
	case StageSubmit:
		return validateSubmit(facts)
	case StageReview:
		return validateReview(facts)
	case StagePrelive:
		return validatePrelive(facts)
	default:
		return Result{}, fmt.Errorf("unknown stage %q", stage)
	}
}

func validateStart(facts Facts) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
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
		FieldCodingSession: facts.SessionID,
	}
	comment := fmt.Sprintf("Implementation started.\n\n- Session: `%s`\n- Branch: `%s`\n- Worktree: `%s`\n- Coder model: `%s`",
		facts.SessionID, facts.Branch, facts.WorktreePath, facts.CoderModel)
	return Result{Stage: StageStart, Fields: fields, Comment: comment}, nil
}

func validateSubmit(facts Facts) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
	}
	if err := requireCommit(facts.Commit, "submitted commit"); err != nil {
		return Result{}, err
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
		FieldCodingSession: facts.SessionID,
	}
	comment := fmt.Sprintf("Submitted for AI review.\n\n- Commit: `%s`\n- Session: `%s`\n- Branch: `%s`\n- Worktree: `%s`\n\nIndependent review by a different model is still required before any pre-live claim.",
		facts.Commit, facts.SessionID, facts.Branch, facts.WorktreePath)
	return Result{Stage: StageSubmit, Fields: fields, Comment: comment}, nil
}

func validateReview(facts Facts) (Result, error) {
	if err := requireIdentity(facts); err != nil {
		return Result{}, err
	}
	if err := requireCommit(facts.ReviewedCommit, "reviewed commit"); err != nil {
		return Result{}, err
	}
	if err := requireCommit(facts.Commit, "submitted commit"); err != nil {
		return Result{}, err
	}
	if facts.ReviewedCommit != facts.Commit {
		return Result{}, fmt.Errorf("reviewed commit %s is not the submitted commit %s", facts.ReviewedCommit, facts.Commit)
	}
	if strings.TrimSpace(facts.ReviewerSession) == "" {
		return Result{}, errors.New("reviewer session id is required")
	}
	if facts.ReviewerSession == facts.SessionID {
		return Result{}, errors.New("reviewer session must differ from the coding session: self-review is not independent")
	}
	if strings.TrimSpace(facts.ReviewerModel) == "" {
		return Result{}, errors.New("reviewer model is required")
	}
	if facts.ReviewerModel == facts.CoderModel {
		return Result{}, fmt.Errorf("reviewer model %q must differ from the coder model: a review by the same model is not independent", facts.ReviewerModel)
	}
	switch facts.Verdict {
	case verdictPass:
		fields := map[string]string{
			FieldState:          StateCodeReview,
			FieldAIStatus:       AIStatusComplete,
			FieldReviewerModel:  facts.ReviewerModel,
			FieldReviewerEffort: facts.ReviewerEffort,
			FieldReviewSession:  facts.ReviewerSession,
			FieldLastReviewed:   facts.ReviewedCommit,
		}
		comment := fmt.Sprintf("Independent review passed.\n\n- Reviewed commit: `%s`\n- Reviewer: `%s` (effort `%s`)\n- Reviewer session: `%s`\n\nPre-live verification remains required before a release claim.",
			facts.ReviewedCommit, facts.ReviewerModel, orDefault(facts.ReviewerEffort, "default"), facts.ReviewerSession)
		return Result{Stage: StageReview, Fields: fields, Comment: comment}, nil
	case verdictFail:
		fields := map[string]string{
			FieldState:          StateInProgress,
			FieldAIStatus:       AIStatusFailed,
			FieldReviewerModel:  facts.ReviewerModel,
			FieldReviewerEffort: facts.ReviewerEffort,
			FieldReviewSession:  facts.ReviewerSession,
			FieldLastReviewed:   facts.ReviewedCommit,
		}
		comment := fmt.Sprintf("Independent review failed; corrections needed.\n\n- Reviewed commit: `%s`\n- Reviewer: `%s` (effort `%s`)\n- Reviewer session: `%s`\n\nCorrections must be submitted as a new commit; the next review must cite that commit.",
			facts.ReviewedCommit, facts.ReviewerModel, orDefault(facts.ReviewerEffort, "default"), facts.ReviewerSession)
		return Result{Stage: StageReview, Fields: fields, Comment: comment}, nil
	default:
		return Result{}, fmt.Errorf("verdict must be %q or %q, got %q", verdictPass, verdictFail, facts.Verdict)
	}
}

func validatePrelive(facts Facts) (Result, error) {
	if err := requireCommit(facts.Commit, "pre-live verified commit"); err != nil {
		return Result{}, err
	}
	if !commitPattern.MatchString(strings.TrimSpace(facts.ReviewedCommit)) {
		return Result{}, errors.New("pre-live verification requires the reviewed commit; independent review must be recorded first")
	}
	if facts.ReviewedCommit != facts.Commit {
		return Result{}, fmt.Errorf("pre-live verified commit %s is not the independently reviewed commit %s", facts.Commit, facts.ReviewedCommit)
	}
	if strings.TrimSpace(facts.PreliveEvidence) == "" {
		return Result{}, errors.New("pre-live evidence is required; do not claim verification without it")
	}
	fields := map[string]string{
		FieldState:           StateForRelease,
		FieldAIStatus:        AIStatusComplete,
		FieldPreliveVerified: facts.Commit,
	}
	comment := fmt.Sprintf("Pre-live verified and ready for go-live authorization.\n\n- Commit: `%s`\n- Evidence: %s\n\nProduction deployment and the Done state remain a separate owner-authorized step.",
		facts.Commit, facts.PreliveEvidence)
	return Result{Stage: StagePrelive, Fields: fields, Comment: comment}, nil
}

func requireIdentity(facts Facts) error {
	if strings.TrimSpace(facts.SessionID) == "" {
		return errors.New("coding session id is required")
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

// checkDuplicateDispatch refuses a second session for a ticket whose recorded
// session is still live and different.
func checkDuplicateDispatch(facts Facts) error {
	recorded := strings.TrimSpace(facts.RecordedSessionID)
	if recorded == "" || recorded == facts.SessionID {
		return nil
	}
	for _, session := range facts.Sessions {
		if session.ID == recorded && session.Live {
			return fmt.Errorf("ticket already has live session %s; refusing to dispatch %s", recorded, facts.SessionID)
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
// The comment is written first: a comment that lands before the fields is
// recoverable evidence, while fields without the comment are not. Field values
// a bundle rejects are an error, never a silent skip. Every written field is
// read back and compared.
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
	if result.Comment != "" {
		// A retried transition must not stack identical evidence comments. If the
		// newest comment already carries this body, the comment half is done.
		duplicate := false
		if recent, err := api.RecentComments(ctx, issue.ID, 3); err == nil {
			for _, body := range recent {
				if strings.TrimSpace(body) == strings.TrimSpace(result.Comment) {
					duplicate = true
					break
				}
			}
		}
		if !duplicate {
			if err := api.Comment(ctx, issue.ID, result.Comment); err != nil {
				return skipped, err
			}
		}
	}
	if len(write) == 0 {
		return skipped, nil
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
