package coordinator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

// ScheduledReviewEligibility is the scheduler's prefilter. It is deliberately
// cheap and side-effect free; ReviewOnce repeats every safety gate against the
// live ticket, worktree and PR before launching anything.
type ScheduledReviewEligibility struct {
	Eligible bool
	Reason   string
}

// ParseReviewRequestedAt reads the YouTrack date-time field. YouTrack returns
// the value in the current user's timezone without an offset, so the caller
// supplies the location instead of guessing UTC.
func ParseReviewRequestedAt(value string, loc *time.Location) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("Review Requested At is empty")
	}
	if loc == nil {
		return time.Time{}, errors.New("Review Requested At needs an explicit timezone")
	}
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if parsed, err := time.ParseInLocation(layout, value, loc); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("Review Requested At %q is not a supported date-time", value)
}

// EvaluateScheduledReview decides whether one approved run may be reviewed now.
// The request timestamp, not the issue's general updated time, is authoritative.
func EvaluateScheduledReview(run Run, ticket TicketState, now time.Time, window time.Duration, loc *time.Location) ScheduledReviewEligibility {
	if !run.Approved {
		return ScheduledReviewEligibility{Reason: "run has no explicit owner approval"}
	}
	switch run.Phase {
	case PhaseCoding, PhaseCorrecting, PhaseAwaitingReview:
	case PhaseReviewing:
		return ScheduledReviewEligibility{Reason: "review already in flight"}
	default:
		return ScheduledReviewEligibility{Reason: "run phase " + string(run.Phase) + " does not need a review"}
	}
	if run.ReviewInFlight != "" {
		return ScheduledReviewEligibility{Reason: "review in flight for " + run.ReviewInFlight}
	}
	if ticket.Facts.AIStatus != youtrack.AIStatusAwaiting {
		return ScheduledReviewEligibility{Reason: "AI Status is " + firstNonEmpty(ticket.Facts.AIStatus, "empty")}
	}
	commit := strings.TrimSpace(ticket.Facts.SubmittedCommit)
	if !commitPattern.MatchString(commit) {
		return ScheduledReviewEligibility{Reason: "submitted commit is not a full SHA"}
	}
	if _, ok := run.LatestReview(commit); ok {
		return ScheduledReviewEligibility{Reason: "commit " + commit + " was already reviewed"}
	}
	requestedAt, err := ParseReviewRequestedAt(ticket.Facts.ReviewRequestedAt, loc)
	if err != nil {
		return ScheduledReviewEligibility{Reason: err.Error() + "; the YouTrack workflow must set it on transition to Awaiting AI Review"}
	}
	if strings.TrimSpace(run.LastReviewRequestAt) != "" {
		lastRequestedAt, err := ParseReviewRequestedAt(run.LastReviewRequestAt, loc)
		if err != nil || !requestedAt.After(lastRequestedAt) {
			return ScheduledReviewEligibility{Reason: "Review Requested At has not verifiably advanced since the last review"}
		}
	}
	if requestedAt.After(now) {
		return ScheduledReviewEligibility{Reason: "Review Requested At is in the future"}
	}
	if window > 0 && requestedAt.Before(now.Add(-window)) {
		return ScheduledReviewEligibility{Reason: fmt.Sprintf("Review Requested At %s is older than the %s window", requestedAt.Format(time.RFC3339), window)}
	}
	return ScheduledReviewEligibility{Eligible: true, Reason: "eligible review request at " + requestedAt.Format(time.RFC3339)}
}

// ScheduledReviewResult is one scheduler decision for one run.
type ScheduledReviewResult struct {
	Issue    string `json:"issue"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Commit   string `json:"commit,omitempty"`
	ReviewID string `json:"reviewSessionId,omitempty"`
}

// SweepReport is one bounded scheduler pass.
type SweepReport struct {
	Scanned  int                     `json:"scanned"`
	Eligible int                     `json:"eligible"`
	Reviewed int                     `json:"reviewed"`
	Skipped  int                     `json:"skipped"`
	Failed   int                     `json:"failed"`
	Results  []ScheduledReviewResult `json:"results"`
}

// Scheduler polls the durable run store for approved work whose ticket asks for
// review. It never discovers or launches an unapproved ticket, never trusts the
// issue's general updated time, and never dispatches a coder.
type Scheduler struct {
	Service *Service
	// Interval is the polling cadence. Five minutes is supported; the minimum
	// is one minute because a faster poll has no useful additional granularity.
	Interval time.Duration
	// Window bounds how old a Review Requested At value may be. Older requests
	// need a separate audit/requeue rather than an automatic launch.
	Window time.Duration
	// TicketTimeout bounds one ticket's review so one stubborn reviewer cannot
	// stall the next scheduled pass.
	TicketTimeout time.Duration
	Location      *time.Location
	Now           func() time.Time
	Log           func(event string, fields map[string]string)
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Scheduler) log(event string, fields map[string]string) {
	if s.Log != nil {
		s.Log(event, fields)
	}
}

func (s *Scheduler) validate() error {
	if s.Service == nil {
		return errors.New("coordinator: scheduler needs a service")
	}
	if s.Interval <= 0 {
		s.Interval = 5 * time.Minute
	}
	if s.Interval < time.Minute {
		return errors.New("coordinator: scheduler interval cannot be faster than one minute")
	}
	if s.Window <= 0 {
		s.Window = 72 * time.Hour
	}
	if s.TicketTimeout <= 0 {
		s.TicketTimeout = time.Hour
	}
	if s.Location == nil {
		s.Location = time.Local
	}
	return nil
}

// Run polls immediately and then on each interval until ctx ends. A slow sweep
// delays the next tick instead of overlapping itself.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.validate(); err != nil {
		return err
	}
	report := s.Sweep(ctx)
	s.log("scheduled_sweep", map[string]string{
		"scanned": fmt.Sprint(report.Scanned), "eligible": fmt.Sprint(report.Eligible),
		"reviewed": fmt.Sprint(report.Reviewed), "skipped": fmt.Sprint(report.Skipped), "failed": fmt.Sprint(report.Failed),
	})
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			report := s.Sweep(ctx)
			s.log("scheduled_sweep", map[string]string{
				"scanned": fmt.Sprint(report.Scanned), "eligible": fmt.Sprint(report.Eligible),
				"reviewed": fmt.Sprint(report.Reviewed), "skipped": fmt.Sprint(report.Skipped), "failed": fmt.Sprint(report.Failed),
			})
		}
	}
}

// Sweep performs one bounded pass over explicitly approved runs.
func (s *Scheduler) Sweep(ctx context.Context) SweepReport {
	report := SweepReport{Results: []ScheduledReviewResult{}}
	if err := s.validate(); err != nil {
		report.Failed++
		report.Results = append(report.Results, ScheduledReviewResult{Status: "failed", Reason: err.Error()})
		return report
	}
	runs, err := s.Service.Store.List(ctx)
	if err != nil {
		report.Failed++
		report.Results = append(report.Results, ScheduledReviewResult{Status: "failed", Reason: err.Error()})
		return report
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Issue < runs[j].Issue })
	now := s.now()
	for _, run := range runs {
		if ctx.Err() != nil {
			report.Failed++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "failed", Reason: ctx.Err().Error()})
			return report
		}
		report.Scanned++
		if !run.Approved {
			report.Skipped++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "skipped", Reason: "run has no explicit owner approval"})
			continue
		}
		ticket, err := s.Service.Tickets.Read(ctx, run.Issue)
		if err != nil {
			report.Failed++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "failed", Reason: err.Error()})
			continue
		}
		eligibility := EvaluateScheduledReview(run, ticket, now, s.Window, s.Location)
		if !eligibility.Eligible {
			report.Skipped++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "skipped", Reason: eligibility.Reason})
			continue
		}
		report.Eligible++
		reviewCtx, cancel := context.WithTimeout(ctx, s.TicketTimeout)
		updated, reviewed, err := s.Service.ReviewOnce(reviewCtx, run.Issue, ScheduledReviewRequest{
			Window:   s.Window,
			Location: s.Location,
			Now:      now,
		})
		cancel()
		switch {
		case err == nil && reviewed:
			session := ""
			if review, ok := updated.LatestReview(updated.SubmittedCommit); ok {
				session = review.Session
			}
			report.Reviewed++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "reviewed", Commit: updated.SubmittedCommit, ReviewID: session})
		case errors.Is(err, ErrNotEligible), errors.Is(err, ErrAlreadyRunning):
			report.Skipped++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "skipped", Reason: err.Error()})
			s.log("scheduled_review_skipped", map[string]string{"issue": run.Issue, "reason": err.Error()})
		case err == nil:
			report.Failed++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "failed", Reason: "no review was performed"})
		default:
			report.Failed++
			report.Results = append(report.Results, ScheduledReviewResult{Issue: run.Issue, Status: "failed", Reason: err.Error()})
			s.log("scheduled_review_failed", map[string]string{"issue": run.Issue, "reason": err.Error()})
		}
	}
	return report
}
