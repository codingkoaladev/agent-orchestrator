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
| Interface | Desktop app from this checkout (Electron, WSLg); currently blocked on the host C++ toolchain, daemon path in use |
| Daemon | Native `ao daemon`, loopback only (`127.0.0.1:3001` and `[::1]:3001`) |
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

1. project config env `OPENCODE_SUBSCRIPTION`
2. the worker's own pin file, written at launch and read on resume
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
# Dispatch (or reconcile) the coder for a ticket
ao-coordinator start --issue CKM-825 --project ckadmin --branch CKM-825 \
  --model opencode-go/deepseek-v4.1-flash --approved \
  --prompt "Implement CKM-825 per the ticket contract"

# Wait for a commit without spending model turns, then review it
ao-coordinator review --issue CKM-825 --base <base-sha>

# After deployment to the pre-live environment
ao-coordinator prelive --issue CKM-825 --prelive-command "<check command>"
ao-coordinator status --issue CKM-825
```

What the service does:

| Phase | Behaviour |
| --- | --- |
| `coding` | spawns a worker on the ticket's branch; records Orchestrator session id, native id, worktree, model, effort and subscription |
| `awaiting_review` | polls the checkout for a committed, clean tree; no model calls |
| `reviewing` | runs the independent reviewer on the exact submitted commit, records verdict, session, model, effort |
| `correcting` | sends the findings to the same coder session, waits for a new commit, submits it, reviews again (bounded rounds) |
| `awaiting_prelive` | review passed; AI work stays active |
| `prelive_verified` | the configured pre-live check exited 0 for the reviewed commit |

Guard rails that are enforced in code, not prose:

- Duplicate dispatch is refused while the recorded session is live. If the
  daemon cannot answer, `start` fails: unknown liveness never authorises a
  dispatch. Restarting the command reconciles the existing run instead of
  spawning a second worker.
- A run records both identities, so a ticket that stores the native OpenCode id
  is still matched against daemon liveness.
- A review verdict exists only after the reviewer process ran; a caller cannot
  assert one. The verdict is bound to the commit the ticket records.
- Every transition re-reads the ticket and refuses to overwrite a newer
  submission (compare-and-set on the submitted and reviewed commit).

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
| prelive verified | For Release | Complete |

`Done` is never set by the bridge, and only a verified pre-live acceptance sets
`Complete`. CKM has no `Ready for Go Live` value, so a verified pre-live handoff
maps to `For Release` + `Complete`. The owner confirmed this mapping for both
workflow tickets.

Known id-space caveat: `AI Coding Session ID` holds the OpenCode session id,
while the daemon reports Orchestrator session ids (`pilot-2`). Duplicate-dispatch
detection compares against the daemon, so it only fires when the ticket records
an Orchestrator session id. Record both ids in the evidence comment while the
field holds the OpenCode one.

## Ticket fields

The four proposed fields now exist on CKM and are written only from observed
evidence: `AI Reviewer Effort`, `AI Review Session ID`, `AI Last Reviewed Commit`,
`AI Pre-live Verified Commit`. The owner confirmed during this session that a
proven pre-live acceptance maps to State `For Release` + AI Status `Complete`.

Field gaps that still block reviewer bookkeeping:

- `AI Reviewer Model`, `AI Reviewer Effort` and `AI Automation Enabled` are
  bundle-backed but their bundles are **empty**. A live write of
  `gpt-6.1-sol` and `low` was rejected as "not in the field choices", so the
  bridge cannot record reviewer identity yet. Add the choices in the project
  settings; the bridge does not substitute another reviewer.
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
- Install a newer C++ toolchain to build the desktop: Electron 44's headers fail
  against g++ 11.4 (`g++-12` is available from apt, 12.3.0, and needs a
  privileged install). Until then the daemon path is the verified interface.
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
