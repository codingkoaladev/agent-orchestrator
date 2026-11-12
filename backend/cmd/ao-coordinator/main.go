// Command ao-coordinator runs one ticket through the local lifecycle:
// dispatch the coder, wait for a real ticket submission without burning model
// turns, run the independent reviewer on that exact commit, return findings to
// the same coder over its chat, re-review, and gate release readiness on
// recorded pre-live evidence.
//
// The run record and its exclusive lock live in the coordinator store, so a
// restart resumes instead of dispatching a second worker or repeating a review
// that may already have started. Liveness always comes from the daemon; when the
// daemon cannot answer, the command fails instead of guessing.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/coordinator"
	"github.com/aoagents/agent-orchestrator/backend/internal/tracker/youtrack"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("command is required")
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	var (
		issue          = fs.String("issue", "", "YouTrack issue id")
		project        = fs.String("project", "", "registered Orchestrator project id")
		branch         = fs.String("branch", "", "branch for the worker")
		model          = fs.String("model", "", "provider-qualified coder model")
		effort         = fs.String("effort", "", "coder reasoning effort")
		subscription   = fs.String("subscription", "", "OpenCode subscription label for this worker")
		prompt         = fs.String("prompt", "", "initial task message for the coder")
		approved       = fs.Bool("approved", false, "the owner authorized this dispatch")
		runDir         = fs.String("run-dir", defaultRunDir(), "run-state directory")
		aoDataDir      = fs.String("ao-data-dir", defaultDataDir(), "Orchestrator data directory")
		runFile        = fs.String("ao-run-file", defaultRunFile(), "Orchestrator run file")
		wait           = fs.Duration("wait", 0, "how long to wait for one verified submission (0 means no limit)")
		poll           = fs.Duration("poll", 10*time.Second, "how often to poll the ticket and checkout")
		confirm        = fs.Duration("confirm", time.Minute, "how long one submission may fail worktree/PR checks before blocking")
		identityWait   = fs.Duration("identity-wait", 30*time.Second, "how long to wait for a native chat identity after spawn")
		rounds         = fs.Int("rounds", 3, "maximum review rounds")
		reviewerBin    = fs.String("reviewer-bin", "codex", "independent reviewer executable")
		reviewerModel  = fs.String("reviewer-model", "gpt-6.1-sol", "independent reviewer model")
		reviewerEffort = fs.String("reviewer-effort", "low", "independent reviewer effort")
		preliveCommand = fs.String("prelive-command", "", "pre-live check command (required to claim acceptance)")
		noWatch        = fs.Bool("no-watch", false, "dispatch or adopt the coder, then exit without supervising")
		timeout        = fs.Duration("timeout", 0, "overall command timeout (0 means no limit)")
		interval       = fs.Duration("interval", 5*time.Minute, "scheduled review polling interval (minimum one minute)")
		window         = fs.Duration("review-window", 72*time.Hour, "oldest Review Requested At value the scheduler may act on")
		ticketTimeout  = fs.Duration("ticket-timeout", time.Hour, "maximum time one scheduled review may take")
		ticketTimezone = fs.String("ticket-timezone", "", "IANA timezone for YouTrack date-time fields (default local)")
		once           = fs.Bool("once", false, "run one scheduled-review sweep and exit")
		baseURL        = fs.String("base-url", "", "YouTrack REST base URL")
		tokenEnv       = fs.String("token-env", "YOUTRACK_TOKEN", "environment variable holding the YouTrack token")
		jsonOut        = fs.Bool("json", false, "print the run record as JSON")
	)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if command != "schedule" && *issue == "" {
		return errors.New("--issue is required")
	}
	ctx := context.Background()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	store := coordinator.Store{Dir: *runDir}
	if command == "status" {
		run, err := store.Load(*issue)
		if err != nil {
			return err
		}
		return printRun(run, true)
	}

	daemon, err := coordinator.NewDaemon(*runFile, *aoDataDir)
	if err != nil {
		return err
	}
	service := &coordinator.Service{
		Tickets:        &ticketAdapter{client: youtrack.NewClient(*baseURL, os.Getenv(*tokenEnv))},
		Sessions:       daemon,
		Reviewer:       coordinator.CodexReviewer{Binary: *reviewerBin, Snapshot: coordinator.GitSnapshot{}},
		Checkout:       coordinator.GitCheckout{},
		PullRequests:   coordinator.GHPullRequests{},
		PreliveRunner:  coordinator.CommandPrelive{},
		Store:          store,
		PollEvery:      *poll,
		WaitFor:        *wait,
		ConfirmFor:     *confirm,
		IdentityWait:   *identityWait,
		MaxRounds:      *rounds,
		PreliveCommand: *preliveCommand,
		ReviewerModel:  *reviewerModel,
		ReviewerEffort: *reviewerEffort,
		Log: func(event string, fields map[string]string) {
			fmt.Fprintf(os.Stderr, "event=%s %v\n", event, fields)
		},
	}

	switch command {
	case "start":
		request := coordinator.StartRequest{
			Issue: *issue, Project: *project, Branch: *branch, Model: *model,
			Effort: *effort, Subscription: *subscription, Prompt: *prompt, Approved: *approved,
		}
		var run coordinator.Run
		if *noWatch {
			run, err = service.Start(ctx, request)
		} else {
			run, err = service.StartAndFollow(ctx, request)
		}
		if err != nil && !errors.Is(err, coordinator.ErrAlreadyRunning) {
			_ = printRun(run, *jsonOut)
			return err
		}
		return printRun(run, *jsonOut)
	case "review":
		run, err := service.Supervise(ctx, *issue)
		if err != nil {
			_ = printRun(run, *jsonOut)
			return err
		}
		return printRun(run, *jsonOut)
	case "prelive":
		run, err := service.Prelive(ctx, *issue, *preliveCommand)
		if err != nil {
			_ = printRun(run, *jsonOut)
			return err
		}
		return printRun(run, *jsonOut)
	case "schedule":
		location := time.Local
		if strings.TrimSpace(*ticketTimezone) != "" {
			location, err = time.LoadLocation(*ticketTimezone)
			if err != nil {
				return fmt.Errorf("load ticket timezone: %w", err)
			}
		}
		scheduler := &coordinator.Scheduler{
			Service:       service,
			Interval:      *interval,
			Window:        *window,
			TicketTimeout: *ticketTimeout,
			Location:      location,
			Log: func(event string, fields map[string]string) {
				fmt.Fprintf(os.Stderr, "event=%s %v\n", event, fields)
			},
		}
		if *once {
			report := scheduler.Sweep(ctx)
			if err := ctx.Err(); err != nil {
				return err
			}
			return printSweep(report, *jsonOut)
		}
		return scheduler.Run(ctx)
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func printSweep(report coordinator.SweepReport, asJSON bool) error {
	if asJSON {
		data, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("scanned: %d\neligible: %d\nreviewed: %d\nskipped: %d\nfailed: %d\n",
		report.Scanned, report.Eligible, report.Reviewed, report.Skipped, report.Failed)
	for _, result := range report.Results {
		fmt.Printf("- %s: %s", result.Issue, result.Status)
		if result.Commit != "" {
			fmt.Printf(" commit=%s", result.Commit)
		}
		if result.ReviewID != "" {
			fmt.Printf(" reviewer=%s", result.ReviewID)
		}
		if result.Reason != "" {
			fmt.Printf(" (%s)", result.Reason)
		}
		fmt.Println()
	}
	return nil
}

// ticketAdapter is the YouTrack side of the lifecycle. It validates with the
// same guards the CLI uses, so the coordinator cannot write a transition the
// bridge would refuse.
type ticketAdapter struct {
	client *youtrack.Client
}

func (t *ticketAdapter) Read(ctx context.Context, issue string) (coordinator.TicketState, error) {
	current, err := t.client.GetIssue(ctx, issue)
	if err != nil {
		return coordinator.TicketState{}, err
	}
	fields := map[string]string{}
	for name, field := range current.Fields {
		fields[name] = field.Value
	}
	return coordinator.TicketState{
		Facts: youtrack.TicketFacts{
			State:              fields[youtrack.FieldState],
			AIStatus:           fields[youtrack.FieldAIStatus],
			SubmittedCommit:    fields[youtrack.FieldLastSubmitted],
			LastReviewedCommit: fields[youtrack.FieldLastReviewed],
			VerifiedCommit:     fields[youtrack.FieldPreliveVerified],
			RecordedSessionID:  fields[youtrack.FieldCodingSession],
			CoderModel:         fields[youtrack.FieldCoderModel],
			Branch:             fields[youtrack.FieldBranch],
			WorktreePath:       fields[youtrack.FieldWorktreePath],
			ReviewRequestedAt:  fields[youtrack.FieldReviewRequestedAt],
		},
		Fields:      fields,
		Summary:     current.Summary,
		Description: current.Description,
	}, nil
}

func (t *ticketAdapter) Apply(ctx context.Context, stage youtrack.Stage, facts youtrack.Facts, review *youtrack.ReviewEvidence, prelive *youtrack.PreliveEvidence) ([]string, error) {
	result, err := youtrack.Validate(stage, facts, review, prelive)
	if err != nil {
		return nil, err
	}
	current, err := t.client.GetIssue(ctx, facts.IssueID)
	if err != nil {
		return nil, err
	}
	return youtrack.Apply(ctx, t.client, current, result)
}

func printRun(run coordinator.Run, asJSON bool) error {
	if asJSON {
		data, _ := json.MarshalIndent(run, "", "  ")
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("issue: %s\nproject: %s\nphase: %s\nsession: %s\nnative: %s\nbranch: %s\nworktree: %s\nsubmitted: %s\nrounds: %d\nnote: %s\n",
		run.Issue, run.Project, run.Phase, run.Coder.OrchestratorID, run.Coder.NativeID,
		run.Coder.Branch, run.Coder.WorktreePath, run.SubmittedCommit, run.ReviewRounds, run.Note)
	if review, ok := run.LatestReview(run.SubmittedCommit); ok {
		fmt.Printf("review: %s at %s by %s (%s)\n", review.Verdict, review.Commit, review.Session, review.Model)
	}
	if run.PreliveCheck != nil {
		fmt.Printf("prelive: exit %d `%s` (%s)\n", run.PreliveCheck.ExitCode, run.PreliveCheck.Command, run.PreliveCheck.Commit)
	}
	return nil
}

func defaultDataDir() string {
	if dir := strings.TrimSpace(os.Getenv("AO_DATA_DIR")); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ao/data"
	}
	return filepath.Join(home, ".ao", "data")
}

func defaultRunFile() string {
	if file := strings.TrimSpace(os.Getenv("AO_RUN_FILE")); file != "" {
		return file
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ao/running.json"
	}
	return filepath.Join(home, ".ao", "running.json")
}

func defaultRunDir() string {
	return coordinator.DefaultStoreDir(defaultDataDir())
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: ao-coordinator <start|review|prelive|schedule|status> --issue CKM-825 [flags]

start     dispatch or adopt the coder, then supervise the lifecycle (requires --approved)
review    resume supervision of an existing run without dispatching
prelive   run the configured pre-live check and only then claim release readiness
schedule  poll explicitly approved runs for Review Requested At and review once (no --issue)
status    print the durable run record

start also accepts --no-watch to dispatch and exit. Supervision is single-owner:
a second process for the same ticket is refused. The scheduler uses
Review Requested At, never the issue's general updated time.`)
}
