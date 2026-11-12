# Local coordinator runbook

Operating instructions for the local Orchestrator that runs CkAdmin and MageOS
tickets: one coder agent, then one independent review by a different model, then
a pre-live gate, stopping short of production.

This runbook describes the state installed on the owner's PC. It is not an
upstream distribution guide; see the repository `README.md` for that.

## Hosting

| Item | Value |
| --- | --- |
| Host | Existing WSL2 Ubuntu 22.04 distribution, WSLg display |
| Interface | Linux desktop through WSLg, extracted without FUSE from the installed release image; its backend executable is replaced by this checkout's source-built daemon. Start with `scripts/start-wsl-desktop.sh --replace-service`. Verified ready with both registered projects on 2026-11-11. |
| Daemon | `ao daemon` on loopback only (`127.0.0.1:3001`, `[::1]:3001`), started either by the desktop app or by the systemd user unit — never both |
| Source | `/home/pawel/projects/koala-orchestrator` |
| State | `~/.ao` (`~/.ao/data`, `~/.ao/data/worktrees`, `~/.ao/running.json`) |
| Network | No public endpoint, no Cloud Agents |
| Startup | Manual; see Start and stop |
| Application projects | `ckadmin` → `/home/pawel/projects/CkAdmin/master`, `magento` → `/home/pawel/projects/CkMagento/master` |
| Application services | DDEV only when a worker needs it; never for Orchestrator itself |

## Pinned source and toolchain

| Component | Version |
| --- | --- |
| Upstream | `OrchestratorInc/agent-orchestrator` @ `34f6c898142f754ac20e43933cf6a81f595a7f6e` |
| Go | 1.27.2 (module requires 1.27.1) |
| Node / npm | 24.14.0 / 11.19.1 |
| OpenCode | v2.2.7 (`opencode-v2` harness resolved by executable name plus major-version probe) |
| Codex CLI | 0.166.0 |
| git / tmux | 2.34.1 / 3.2a |

Build from source (no npm CLI install; the `@aoagents/ao` package is frozen):

```bash
npm ci
npm --prefix frontend ci
cd backend && go build -o bin/ao ./cmd/ao
```

## Start and stop

The daemon runs as a per-user systemd unit so the subscription pin survives a
restart. Install or refresh it with:

```bash
scripts/install-ao-daemon.sh --subscription subscription-3
```

```bash
systemctl --user start ao-daemon     # start
systemctl --user status ao-daemon    # status
systemctl --user stop ao-daemon      # stop
```

The unit is **not** enabled at login during the pilot. To change that, run
`systemctl --user enable ao-daemon`. While the PC or WSL is stopped, no work
runs.

Only one process can own port 3001. When the desktop app starts, its managed
daemon takes the port and stops the systemd one; observed on this host. Pick one
interface for a session and stop the other first:

```bash
systemctl --user stop ao-daemon          # before launching the desktop app
# or, before using the systemd unit:
# quit the desktop app, then start ao-daemon
```

Both satisfy the agreed arrangement (desktop through WSLg, daemon on loopback);
the difference is who supervises the daemon.

Verify a healthy install:

```bash
ao status
ao doctor
ao agent ls | grep -E '^(ID|codex|opencode-v2)'
ao project ls
```

`ao doctor` prints `WARN opencode-v2: opencode-v2 not found in PATH`. That check
looks for an executable *named* `opencode-v2`; the OpenCode 2 harness is served
by the ordinary `opencode` executable and passes a major-version probe, which
`ao agent ls` reports as installed and authorized. Treat the doctor warning as a
naming artifact, not a missing harness.

## Subscription selection

OpenCode 2 stores saved accounts in `<XDG_DATA_HOME>/opencode/auth.json` and
`opencode auth switch` changes the active account inside that one store. The
coordinator therefore pins a subscription by pinning the **data home**, so no
process ever switches another worker's credential.

| Piece | Location |
| --- | --- |
| Daemon default label | `AO_OPENCODE_SUBSCRIPTION` |
| Per-project label | project config env `OPENCODE_SUBSCRIPTION` (the `AO_` prefix is reserved) |
| Per-worker pin | `~/.ao/data/coordinator/subscriptions/<session-id>` |
| Scope data home | `~/.local/share/opencode-subscriptions/<label>` |
| Scope state home | `~/.local/state/opencode-subscription-<label>` |
| Managed by | `scripts/opencode-subscription.sh` |

```bash
scripts/opencode-subscription.sh list
scripts/opencode-subscription.sh status subscription-3
scripts/opencode-subscription.sh login subscription-1     # interactive login in that scope
scripts/opencode-subscription.sh adopt subscription-4 --from-profile ~/.local/share/opencode-profile-4
```

`adopt` copies an existing `opencode/auth.json` once, keeps mode `0600`, refuses
to overwrite a scope, and never prints credential contents. The script and the
adapter resolve the same path: `opencode.V2DataHome` returns
`<parent>/opencode-subscriptions/<label>` when the label is set and the previous
`<parent>/opencode-v2-home` otherwise.

Resolution order for a worker, applied identically by the TUI adapter and the
ACP chat driver:

1. the worker's own pin file, written at launch and read on resume
2. project config env `OPENCODE_SUBSCRIPTION`
3. the daemon default `AO_OPENCODE_SUBSCRIPTION`

The pin is what makes a restart or resume deterministic: changing a project's
label later does not move an existing worker into a different account scope, and
two projects pinned to different subscriptions run side by side in one daemon.

Rules that keep this safe:

- Never run `opencode auth switch` against a scope another worker is using.
  Selecting an account means selecting a data home, not switching a shared store.
- Manual CLI work outside the daemon should pass `--standalone`, which is what
  the adapter uses; without it OpenCode tries to reach the shared managed
  service on port 49376.
- Two daemons are no longer required for two subscriptions, but if you do run
  two, give each its own `AO_DATA_DIR`/`AO_RUN_FILE`, port and `XDG_STATE_HOME`.

## Coordinator lifecycle

`ao-coordinator` is the entry point that takes a ticket and runs the lifecycle.
It is a real service over the daemon's HTTP API and the YouTrack REST API, not a
collection of marker commands:

```bash
# Dispatch (or adopt) the coder and supervise until review/pre-live readiness
ao-coordinator start --issue CKM-825 --project ckadmin --branch CKM-825 \
  --model opencode-go/deepseek-v4.1-flash --approved \
  --prompt "Implement CKM-825 per the ticket contract"

# Resume supervision of an existing run (never dispatches a second worker)
ao-coordinator review --issue CKM-825

# After deployment to the pre-live environment
ao-coordinator prelive --issue CKM-825 --prelive-command "<check command>"
ao-coordinator status --issue CKM-825
```

`start` now stays alive: it dispatches only when needed, then holds an exclusive
per-ticket OS file lock and advances the durable run. `start --no-watch` is the
old dispatch-and-exit behaviour and is intended for diagnostics, not the
unattended workflow. The lock is released by the kernel on crash, so a restart
reclaims a stale lock without another worker being launched. A second supervisor
for the same ticket is refused. `review` resumes the same run without dispatching.

What the service does:

| Phase | Behaviour |
| --- | --- |
| `coding` | spawns a worker on the ticket's branch; records Orchestrator session id, chat-native id, worktree, model, effort and subscription |
| `awaiting_review` | waits for the ticket itself to say `Awaiting AI Review` and name one full submitted SHA, then proves that SHA against the coder, checkout and open PR; no model calls and no local-commit guessing |
| `reviewing` | runs the independent reviewer on a private snapshot of that exact commit, records the reviewer's real native session id, model and effort |
| `correcting` | returns findings to the same coder over the chat API, waits for a new ticket submission, submits nothing on the coder's behalf, and re-reviews (bounded rounds) |
| `awaiting_prelive` | review passed; AI work stays active. With no configured pre-live command the run stops honestly here |
| `prelive_verified` | the configured pre-live check exited 0 for the reviewed commit |

Guard rails that are enforced in code, not prose:

- Duplicate dispatch is refused while the recorded session is live. If the
  daemon cannot answer, `start` fails: unknown liveness never authorises a
  dispatch. Restarting the command reconciles the existing run instead of
  spawning a second worker.
- A coder that disappears before any submission blocks the run. The coordinator
  never restarts unknown work.
- A review starts only when the ticket is `Awaiting AI Review`, the submitted
  commit is a full lowercase SHA, HEAD and the open PR head both equal it, the PR
  head branch is the intended branch, and the coder is idle/exited/finished.
- Staged changes are refused because they are not in the submitted commit.
  Unstaged and untracked environment files are tolerated but excluded: the
  reviewer receives a private `git clone --shared` of the repository checked out
  at the exact commit, never the dirty worktree.
- A review verdict exists only after the reviewer process ran; a caller cannot
  assert one. The verdict is bound to the commit the ticket records.
- The reviewer's native session id comes from the reviewer's own JSON event
  stream. A missing id blocks the run instead of recording a generated handle.
- Every transition re-reads the ticket, refuses to overwrite a newer submission,
  refuses to skip a required evidence field, and reads every written field back.
- A reviewer whose process may have started is never re-run after a restart: the
  attempt is persisted before launch, so an interrupted review blocks for
  reconciliation instead of producing a duplicate review.
- No transition writes `AI Status: Complete` or `State: Done`.


## Scheduled reviews

`ao-coordinator schedule` is the deterministic review poller. It consumes no
model tokens and never dispatches a coder:

```bash
# Default: an immediate sweep, then every five minutes while the process runs
ao-coordinator schedule --interval 5m --review-window 72h

# One bounded sweep for verification or a manual cron invocation
ao-coordinator schedule --once --json
```

Eligibility is deliberately narrow:

- The ticket must already have a durable run created by `start --approved`. The
  run store is the explicit authorization set, so a broad YouTrack query can
  never launch unrelated work.
- The ticket must be `Awaiting AI Review` with a full 40-character submitted
  commit that has no recorded review.
- `Review Requested At` must be present, parseable and no older than
  `--review-window` (default 72h). The issue's general updated time is never
  used: unrelated edits and stale queues must not become review requests.
- The live worktree/PR/coder checks are repeated by `ReviewOnce`; the same
  per-ticket lock and in-flight marker protect against a second process or a
  crash-resumed duplicate review.
- A block, a missing timestamp or a review already in flight is reported as a
  skip, not re-launched. Corrections go to the same coder through its chat; the
  next `Review Requested At` starts the next bounded review.

### YouTrack workflow requirement

Both CKA and CKM now expose a `Review Requested At` date-time field. Its
presence is **not** proof that a workflow populates it. The project workflow must
set it on every transition into `AI Status = Awaiting AI Review`, including
resubmissions after corrections, and must leave it unchanged on unrelated edits.
Until that workflow is configured and tested in both projects, the scheduler
skips issues with an empty/invalid value and reports them; it never invents a
timestamp or falls back to the updated time. Old issues are not backfilled.

### Cadence assessment

The existing Orchestrator automation observer polls every 15 seconds
(`observe/automations.DefaultTickInterval`), and its RRULE canonicalizer accepts
a five-minute recurrence (`FREQ=MINUTELY;INTERVAL=5`). Its cron translation does
not accept `*/5`, and — more importantly — an automation run spawns a model
session. It is therefore not a token-free ticket poller. `ao-coordinator
schedule` uses its own five-minute ticker and the durable run store instead. A
schedule only runs while its host and process are running.

## Persisted defaults

| Role | Tool | Model | Effort |
| --- | --- | --- | --- |
| Coder | OpenCode 2 (`opencode-v2`) | `opencode-go/deepseek-v4.1-flash` | provider default (unset) |
| Independent reviewer | Codex (`codex`) | `gpt-6.1-sol` | `low` |
| Coordinator | configurable | unset in this installation; pick a lightweight model per project | provider default |

Both application projects carry the coder and reviewer settings:

```bash
ao project set-config ckadmin --config-json '{
  "worker": {"agent": "opencode-v2", "agentConfig": {"model": "opencode-go/deepseek-v4.1-flash"}},
  "reviewers": [{"harness": "codex", "agentConfig": {"model": "gpt-6.1-sol", "effort": "low"}}]
}'
```

Model ids were read from the installed catalog (`opencode models`) and from a
live Codex probe, not guessed. Verify a reviewer model before trusting it:

```bash
codex exec -m gpt-6.1-sol -c 'model_reasoning_effort="low"' --skip-git-repo-check "Reply with READY"
```

## YouTrack bridge

YouTrack is not an Orchestrator tracker integration. `ao-youtrack` is a focused
bridge: the coordinator observes facts, the bridge validates them, writes one
evidence comment, then the supported fields, then reads them back.

```bash
# Read-only validation against the live ticket
ao-youtrack start --issue CKM-825 --session <id> --branch <branch> \
  --worktree <path> --model opencode-go/deepseek-v4.1-flash --dry-run

# Real transitions
ao-youtrack submit --issue CKM-825 --session <id> --branch <branch> \
  --worktree <path> --model opencode-go/deepseek-v4.1-flash --commit <sha>
ao-youtrack review --issue CKM-825 --session <id> --branch <branch> \
  --worktree <path> --model opencode-go/deepseek-v4.1-flash --commit <sha> \
  --reviewed-commit <sha> --reviewer-session <id> \
  --reviewer-model gpt-6.1-sol --reviewer-effort low --verdict pass
ao-youtrack prelive --issue CKM-825 --commit <sha> --reviewed-commit <sha> \
  --evidence "<pre-live run and result>"
```

The token is read from `$YOUTRACK_TOKEN` (override with `--token-env`) and is
never printed. Inject it per command:

```bash
cd /home/pawel/projects/CkMagento/master
./Scripts/secrets/ck-secret run -- ao-youtrack submit ...
```

Credential finding from this installation: the documented key
`YOUTRACK_AUTH_TOKEN` is present but returns `HTTP 401` from the REST API, while
the injected `YOUTRACK_TOKEN` authenticates. The bridge uses the working one; the
stale key is an owner follow-up.

### Guards

| Stage | Refuses when |
| --- | --- |
| `start` | session, branch, worktree or coder model missing; the ticket's recorded session is live and different (duplicate dispatch) |
| `submit` | commit is not a full 40-character SHA; the worktree is not on the recorded branch or does not contain the commit |
| `review` | no recorded review for the commit; ticket submitted commit differs from the commit under review; reviewer session equals the coding session (self-review); reviewer model equals the coder model |
| `prelive` | no recorded passing review; ticket reviewed commit differs from the submitted one; no recorded check; check exited non-zero; evidence covers another commit |

`Apply` writes the comment first (skipping an identical one already at the top of
the ticket, so a retry does not stack evidence), skips fields the project does
not define and reports them, refuses a value a bundle does not offer, and reads
every written field back.

YouTrack needs a different payload shape per custom-field type, and rejects a
value of the wrong shape with `$type is required`:

| Project field type | Issue field type | Value |
| --- | --- | --- |
| `StateProjectCustomField` | `StateIssueCustomField` | `{"$type":"StateBundleElement","name":"…"}` |
| `EnumProjectCustomField` | `SingleEnumIssueCustomField` | `{"$type":"EnumBundleElement","name":"…"}` |
| `SimpleProjectCustomField` | `SimpleIssueCustomField` | plain string |

### State mapping

| Transition | State | AI Status |
| --- | --- | --- |
| start | In Progress | In Progress |
| submit | In Progress | Awaiting AI Review |
| review fail | In Progress | Failed AI Review - Corrections Needed |
| review pass | Code Review | In Progress (AI work stays active) |
| prelive verified | For Release | In Progress (completion automation unsafe) |

The bridge never writes `Done`. During this pilot, pre-live acceptance writes
`For Release` with AI Status `In Progress`: the owner's current automation can
move State to Done when AI Status becomes Complete. Do not enable completion
writes until that automation has been corrected and independently verified.
This is an interim safety limit; the agreed final mapping remains For Release /
Complete, with Done reserved for verified production deployment.

Chat workers expose their native identity as `providerConversationId`, a
harness-tagged handle such as `ao-harness:opencode-v2:ses_abc`; they do not
populate `agentSessionId`. The coordinator accepts that identity only when the
tag prefix and harness match the session, and refuses an untagged or
mismatched handle instead of guessing. TUI workers continue to use
`agentSessionId`. Both the daemon session id and the resolved native id are
persisted and used when checking whether a ticket already has a live worker. A
missing or malformed `sessions` envelope fails closed.

Corrections are delivered through the session's real interface: Chat workers use
`POST /api/v1/sessions/{id}/conversation/messages` with a stable
`clientMessageId`, while TUI workers keep the terminal `send` route. A crash
between finding and delivery therefore retries idempotently instead of posting a
second turn.

Before spawning, the coordinator exclusively reserves the ticket's run record.
It saves the returned worker identity before updating YouTrack. Retrying a known
worker resumes ticket recording without another launch. An ambiguous POST failure
leaves a `dispatching` claim and requires operator reconciliation: inspect the
daemon sessions and ticket, then repair the claim with the confirmed worker
identity or archive it only after confirming no worker was created. Never delete
such a claim just to retry. A confirmed subscription-preflight rejection sends no
POST and releases its claim, allowing a corrected configuration to be retried.
Terminal runs also require explicit archival before replacement.

`start --subscription <label>` currently verifies the project's explicit
`config.env.OPENCODE_SUBSCRIPTION` pin; it does not override shared daemon or
project settings. Set the project pin before dispatch. The request is refused
when the configured pin differs. Existing workers keep their persisted pin even
when the project configuration changes.

## Ticket fields

The evidence fields exist on CKM and are written only from observed facts:
`AI Reviewer Effort`, `AI Review Session ID`, `AI Last Reviewed Commit`,
`AI Pre-live Verified Commit`. `Review Requested At` is a date-time field read
by the scheduler; its value is set by the project workflow, not by this bridge.
The final target after proven pre-live acceptance is State `For Release` + AI
Status `Complete`; the pilot deliberately keeps AI Status In Progress until the
Done automation is corrected.

Field gaps that still block parts of the workflow:

- `AI Reviewer Model` now offers `gpt-6.1-sol` and `AI Reviewer Effort` is a
  free-text field. A project whose bundle still omits the selected reviewer
  causes the transition to fail closed instead of claiming the review.
- `Review Requested At` must be populated by the YouTrack workflow on every
  transition into `Awaiting AI Review`. Field presence alone does not prove that
  workflow exists; the scheduler skips and reports empty values.
- There is no `Ready for Go Live` state and no `Test Failed` state. The mapping
  above is the agreed replacement.
- `AI Automation Enabled` is not written by the bridge.

## Pending owner work

- Add reviewer choices (`gpt-6.1-sol`) and the `low` effort value to the CKM and
  CKA project field bundles. Until then `ao-youtrack` reports `AI Reviewer Model`
  and `AI Reviewer Effort` as skipped instead of substituting a reviewer.
- Repair or retire the `YOUTRACK_AUTH_TOKEN` entry; `YOUTRACK_TOKEN` is the one
  that authenticates today.
- Provide the pre-live environment and its evidence procedure; no pre-live claim
  can be made today.
- Install a newer C++ toolchain to build this checkout's desktop: Electron 44's
  headers fail against g++ 11.4 (`g++-12` is available from apt, 12.3.0, and
  needs a privileged install). The installed release AppImage already runs
  through WSLg, so this blocks a fork build, not the agreed interface.
- Decide whether the pilot keeps using the systemd unit or the desktop-managed
  daemon once the desktop builds.

## Security notes

- No credential contents, tokens, cookies or auth headers are logged by the
  bridge, the subscription script or this runbook.
- Credential stores keep mode `0600`; scope directories are `0700`.
- The daemon listener stays on loopback; do not expose it.
- Reviewer agents run with their own MCP configuration. A reviewer in this
  installation could post YouTrack comments but was denied field updates by its
  approval policy; treat reviewer write access as something to constrain
  explicitly, not something Orchestrator blocks for you.

## WSL desktop repair (2026-11-11)

The earlier AppImage window observation did not establish reliable GUI operation: the owner subsequently reported a high-CPU orphaned FUSE runtime. Direct AppImage launch is no longer the recommended local path.

Use `scripts/start-wsl-desktop.sh --replace-service` or the installed `Koala Coordinator (WSL)` desktop entry. The script reads the image as a SquashFS data archive using `unsquashfs`; it never executes the AppImage runtime. Extraction is cached under a content hash in `~/.ao/desktop-extracted/`. It installs this checkout's source-built backend into that private extracted payload and launches AppRun. This uses the release renderer with the custom source backend; it is not a full rebuild of the custom renderer. Future renderer builds still need a suitable C++ compiler.

The launcher refuses a second launch and refuses to stop the systemd service if any worker activity is active or unknown. Preparation occurs before service shutdown. A failed GUI launch restores the source service when the backend is unhealthy. `AO_KEEP_DAEMON=1` keeps the backend alive when the window closes. Only one supervisor should manage the same state and port.

An isolated launch exposed a separate `spawn ENOTDIR` defect: configured packaged-daemon launches used app.asar as an OS working directory. The source resolver now chooses the real home directory for this case. The regression test performs a real subprocess launch with an archive fixture; it failed with ENOTDIR before the fix and passed afterward. The extracted local deployment uses the bundled-daemon path, avoiding this bug in the release renderer.

Verification: isolated port 3301 and scratch state first; then actual coordinator on port 3001 after confirming all existing workers were exited. Playwright inspected the real Electron window through a temporary loopback debugging port: nonblank renderer, daemon ready, CkAdmin and Magento entries visible. Backend readiness returned HTTP 200; the executable resolved to the extracted payload's source-built ao; extracted desktop processes had zero /dev/fuse descriptors. Desktop access does not establish coordinator lifecycle correctness or pre-live acceptance; remaining dispatch/subscription/automation findings still block a real task.

WSL graphics verification found a black compositor capture despite ready DOM content. The WSL launcher now uses Electron software rendering (`--disable-gpu`); a subsequent real screenshot visibly showed both projects and the working interface. It also closes the launcher lock descriptor in the child so a backend kept alive after GUI close cannot retain the lock and prevent reopening. Normal shortcut launches have no remote debugging port; a loopback debug port was used only for the verification session.

Additional check limitation: `npm run typecheck` failed on unresolved React/clsx/tailwind-merge dependencies in the product-ui package (plus resulting type errors). No broad typecheck pass is claimed. The optional Biome executable was not present; no formatter was installed. The narrow 12-test daemon-launch file and shell syntax checks passed.
