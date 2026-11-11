# Coordinator pilot evidence (CKM-825)

Recorded on 2026-11-11 on the owner's WSL2 host. Everything below was executed
against the real daemon, the real OpenCode and Codex installations, and the live
YouTrack ticket; nothing is simulated except the pilot repository itself.

## 1. Subscription isolation with concurrent sessions

Two scopes were adopted from existing OpenCode profiles, then two sessions were
started at the same time with the same ordinary `opencode` executable.

| Scope | Data home | Saved accounts |
| --- | --- | --- |
| `subscription-1` | `~/.local/share/opencode-subscriptions/subscription-1` | 1 |
| `subscription-3` | `~/.local/share/opencode-subscriptions/subscription-3` | 1 |

```bash
XDG_DATA_HOME=.../subscription-1 opencode run --standalone -m opencode-go/deepseek-v4.1-flash "Reply with PONG"
XDG_DATA_HOME=.../subscription-3 opencode run --standalone -m opencode-go/deepseek-v4.1-flash "Reply with PONG"
```

Both ran concurrently and both answered `PONG`.

| Probe | OpenCode session | Present in scope 1 | Present in scope 3 |
| --- | --- | --- | --- |
| A (scope 1) | `ses_e354c0c3cffeV0V6njAbI8gLKu` | yes | no |
| B (scope 3) | `ses_e354c0d5dffe1EzJxCuB6FbTNX` | no | yes |

Session B was then resumed inside its own scope:

```bash
XDG_DATA_HOME=.../subscription-3 opencode run --standalone --session ses_e354c0d5dffe1EzJxCuB6FbTNX "Reply with RESUMED"
```

It returned the same session id and answered `RESUMED`. Both `auth.json` files
were byte-identical before and after the runs, so no process switched another
worker's active account.

Observation: the existing profiles `opencode-profile-2` and `opencode-profile-3`
hold the **same** saved key (identical file hash), so only `subscription-1` and
`subscription-3` represent distinct accounts on this machine today.

## 2. Coder and reviewer availability

| Check | Result |
| --- | --- |
| `ao agent ls` → `opencode-v2` | installed, authorized (scope credentials found) |
| `ao agent ls` → `codex` | installed, authorized |
| `opencode models` | `opencode-go/deepseek-v4.1-flash` present |
| `codex exec -m gpt-6.1-sol -c model_reasoning_effort="low"` | answered `READY` |

## 3. Controlled pilot session

Fixture repository: `/home/pawel/projects/ao-pilot-fixture` (synthetic, no
application code). Registered as project `pilot` with the same worker and
reviewer configuration as the application projects.

```bash
ao spawn --project pilot --harness opencode-v2 --name "CKM-825 pilot worker" \
  --branch CKM-825-pilot --issue CKM-825 \
  --prompt "Create a file named PILOT.md containing exactly the line: coordinator pilot ok. Then stop."
```

| Fact | Value |
| --- | --- |
| Orchestrator session | `pilot-2` (idle, not terminated) |
| Harness | `opencode-v2` |
| Branch | `CKM-825-pilot` |
| Worktree | `/home/pawel/.ao/data/worktrees/pilot/pilot-2` |
| Submitted commit | `c45e208421f94d7028b473c50e32bcf4d5d2f853` |
| Reviewer | `gpt-6.1-sol`, effort `low`, `codex review --commit c45e2084…` |

The agent created `PILOT.md`, ran `cat -A PILOT.md`, confirmed `git status`
showed only that new file, and stopped without committing or pushing. The
coordinator committed the marker to produce a reviewable commit.

Independent review verdict: pass, no actionable findings, recorded on the ticket
as a pilot-scoped comment that explicitly disclaims review of the coordinator
implementation.

One refused launch preceded it: `pilot-1` failed because the fixture had no
primary remote or recorded default branch. After `ao project set-config pilot
--default-branch main`, `pilot-2` started normally. `pilot-1` was killed and its
workspace preserved.

## 4. Bridge guards exercised against the live ticket

| Case | Result |
| --- | --- |
| `start` dry run with the real session, branch and worktree | accepted, fields matched the ticket |
| `review` dry run for `c45e2084…` by `gpt-6.1-sol`/`low` | accepted, would set Code Review / Complete |
| Reviewer model equal to the coder model | refused: not independent |
| Reviewed commit not present in the worktree | refused |
| `prelive` without a reviewed commit or evidence | refused |

Pilot transitions were dry runs only: the ticket's own `AI Coding Session ID`
and commit fields describe the coordinator implementation, not the fixture.

## 5. Corrections after the failed independent review

The first independent review (gpt-6.1-sol, low) failed at `9d27d0f7b8`. The
corrections below were implemented and re-tested on the same session; the code
now enforces what the review found unproven.

| Finding | Correction and evidence |
| --- | --- |
| Pre-live and review trusted caller-supplied SHAs | Guards bind to the freshly read ticket. `ao-youtrack review` requires a verdict recorded by the coordinator run store for the commit the ticket reports; `prelive` requires a recorded passing review **and** a recorded check for the same commit. Regression tests: `TestValidateReviewBindsToTheLiveTicketAndRecordedVerdict`, `TestValidatePreliveRequiresPassingReviewOfTheSubmittedCommitAndRealEvidence`. |
| A failed review could still reach For Release | `AI Last Reviewed Commit` alone is never treated as a pass: the verdict is read from the run record, and a fail verdict is refused. `TestValidatePrelive…` covers the failed-review case explicitly. |
| Stale reviews could overwrite current fields | Compare-and-set on the submitted and reviewed commit, re-checked immediately before the write. `TestApplyAbortsWhenTheTicketSubmissionChangesMidFlight`. |
| AI Status completed at review | Passing review now sets `Code Review` + `AI In Progress`. `TestPassingReviewKeepsAIWorkInProgress`; only `prelive` sets `For Release` + `Complete`. |
| Duplicate protection failed open | `start` fails when the daemon cannot answer; both identities are persisted. `TestStartFailsClosedWhenLivenessIsUnknown`, `TestStartRefusesWhenTheTicketRecordsAnotherLiveSession`, `TestRestartResumesWithoutASecondDispatch`. |
| No wired coordinator | `ao-coordinator` runs the loop over the daemon and YouTrack APIs: spawn or reconcile, wait without model calls, review the exact commit, return findings to the same coder, re-review, gate pre-live. `TestReviewLoopReturnsFindingsAndPassingReviewKeepsAIInProgress`, `TestPreliveIsGatedOnARecordedPassAndSetsRelease`. |
| Subscription was per daemon | Per-project `OPENCODE_SUBSCRIPTION` plus a per-worker pin file, read by both the TUI adapter and the ACP chat driver. Live probe: session `pilot-5` (project pinned to `subscription-1`) created OpenCode session `ses_e3528845affe0WdHDyO8BXZu6j` in the `subscription-1` scope while `subscription-3` gained no session. `TestLabelForSessionPrefersProjectPinThenWorkerPinThenDaemonDefault`, `TestConfigureUsesTheWorkersPinnedSubscription`. |
| Desktop unverified | Two results. (a) The agreed interface **is** running through WSLg on this host: `~/.ao/agent-orchestrator.AppImage` (pid 1001850) maps the window `Orchestrator.inc` 1320x860 on `DISPLAY=:0`, and its packaged daemon holds `127.0.0.1:3001`. (b) Building *this checkout's* desktop with the isolated lab is blocked on the host toolchain: Electron 44 headers fail against g++ 11.4 (`v8config.h` / `v8-primitive.h` errors while rebuilding better-sqlite3). `g++-12` is available from apt (`12.3.0-1ubuntu1~22.04.3`) and is not installed; that is a privileged OS step left to the owner. Observed side effect: launching the app stopped the systemd daemon and took port 3001, so only one daemon owner at a time. |

## 6. Not proven

- The Electron desktop has not been launched through WSLg yet.
- No pre-live environment exists, so no pre-live transition has been attempted.
- Per-session subscription switching inside one daemon is not implemented.
- Reviewer bookkeeping fields (`AI Reviewer Model`, `AI Reviewer Effort`) cannot
  be written until their bundles gain the required choices.
- `AI Automation Enabled` is unwritten, and no pre-live transition has been
  attempted because no pre-live environment exists.
- The Electron desktop cannot build on this host until a newer C++ compiler is
  installed (see the corrections table).
- The coordinator lifecycle is proven against synthetic daemon, reviewer and
  tracker ports plus a live spawn probe; it has not yet run a full live
  ticket-to-review cycle, because that requires the owner's ticket dispatch.

## Superseding GUI evidence (2026-11-11)

Earlier AppImage/window success statements above are historical and superseded by the later owner-reported FUSE stall. Root reproduced a blank extracted desktop with `spawn ENOTDIR`, added a failing real-process regression test and corrected the configured packaged launch directory. The targeted frontend file now passes all 12 tests.

Verified a non-FUSE extracted release renderer with the source-built backend, first on isolated port 3301, then on real port 3001 after proving no workers were active. Playwright confirmed a nonblank GUI, daemon-ready indicator, and both CkAdmin and MageOS project entries. Readiness was HTTP 200 and no extracted desktop process held /dev/fuse. A screenshot was captured locally for this check. No full source-renderer build, passing coordinator implementation review, live ticket journey, or pre-live acceptance is claimed.

Software-rendering follow-up: the first capture was black despite correct DOM readiness. After --disable-gpu, the screenshot visibly contains CkAdmin and MageOS, the project list and settings. Closing/reopening exposed and fixed an inherited lock-descriptor issue in the new launcher; the persistent daemon no longer inherits that lock. Full frontend typecheck failed due to unresolved product-ui dependencies; no pass claimed.

## Core repair evidence (2026-11-11)

The owner confirmed the WSL GUI is visible. Root then repaired the coordinator
boundaries; this does not prove a full live-ticket journey or pre-live acceptance.

- Real API-shaped transport regressions first failed because `sessions` was
  decoded as `data`; after repair, live workers and native `agentSessionId` survive
  decoding, and an unknown envelope fails closed. No network service or model
  was used by these regressions.
- Exclusive on-disk ticket claims precede dispatch. Regression coverage verifies
  refusal after an ambiguous launch, known-worker recovery after a tracker write
  fails, native-ID duplicate detection, and safe retry after a confirmed
  subscription-preflight rejection. Fake sessions and tracker ports are used.
- A project-label change initially overrode a saved worker subscription; the
  red regression reproduced that switch. The worker pin now wins and the test
  passes, preserving account scope on resume.
- Pre-live writes deliberately keep AI Status In Progress until the owner's
  known auto-Done workflow is corrected. No real Complete/Done write occurred.
- `npm run api`: passed; source DTO, OpenAPI and frontend types regenerated.
- `go test ./internal/coordinator ./internal/tracker/youtrack`: passed.
- `go test -race ./internal/coordinator ./internal/tracker/youtrack`: passed.
- `go test ./internal/httpd/...`: passed, including controller and schema checks.
- `go test ./internal/adapters/agent/opencode -run 'Subscription|LabelForSession' -count=1`: passed.
- `go test ./internal/adapters/chatdriver/opencodev2acp`: passed.
- `go build ./...` and focused `go vet`: passed.
- OpenCode v2 adapter tests `TestV2RejectsFailuresWithoutSideEffects` and
  `TestV2ResolvesAgainBetweenAttempts` fail both on this repair and the unchanged
  pre-repair commit `cde5d299`; baseline checked in an isolated `/tmp` archive.
- Full backend suite was invoked; its final outcome and any limits are recorded
  in the ticket/PR. Do not infer a full-suite pass from the focused checks.
- Separate Sol 6.1 low-effort standards and specification reviews passed the
  final core repair diff after one recovery correction. These are static code
  reviews, not evidence of a live lifecycle or production release readiness.
