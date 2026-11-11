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

## 5. Not proven

- The Electron desktop has not been launched through WSLg yet.
- No pre-live environment exists, so no pre-live transition has been attempted.
- Per-session subscription switching inside one daemon is not implemented.
- Reviewer bookkeeping fields (`AI Reviewer Model`, `AI Reviewer Effort`) cannot
  be written until their bundles gain the required choices.
- `AI Automation Enabled` is unwritten, and no pre-live transition has been
  attempted because no pre-live environment exists.
