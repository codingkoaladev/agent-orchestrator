#!/usr/bin/env bash
# Install the Orchestrator daemon as a per-user systemd unit pinned to one
# OpenCode subscription scope. Manual pilot lifecycle: the unit is installed and
# enabled for the current boot only when you pass --start; it is never enabled
# at login unless you run `systemctl --user enable ao-daemon`.
#
# Usage:
#   scripts/install-ao-daemon.sh [--subscription subscription-3] [--start]
#
# The daemon keeps its default state root (~/.ao) and its loopback-only
# listener. No privileged step is required.
set +x
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
subscription="subscription-3"
start_now=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --subscription) subscription="$2"; shift 2 ;;
    --start) start_now=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [[ ! "$subscription" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]]; then
  echo "invalid subscription label: $subscription" >&2
  exit 2
fi

bindir="$HOME/.local/bin"
unit_dir="$HOME/.config/systemd/user"
mkdir -p "$bindir" "$unit_dir"

install -m 755 "$repo_dir/backend/bin/ao" "$bindir/ao"
install -m 755 "$repo_dir/scripts/opencode-subscription.sh" "$bindir/opencode-subscription"

# The daemon launches agent CLIs, so its PATH must contain them even though a
# user systemd manager starts with a minimal environment.
path_parts=()
for tool in opencode codex ao; do
  resolved="$(command -v "$tool" 2>/dev/null || true)"
  if [[ -n "$resolved" ]]; then
    path_parts+=("$(dirname "$resolved")")
  fi
done
path_parts+=("/usr/local/bin" "/usr/bin" "/bin")
daemon_path="$(IFS=:; echo "${path_parts[*]}" | tr ':' '\n' | awk '!seen[$0]++' | paste -sd: -)"

cat >"$unit_dir/ao-daemon.service" <<UNIT
[Unit]
Description=Orchestrator local daemon (loopback only)
Documentation=file://$repo_dir/docs/coordinator/runbook.md
After=network.target

[Service]
Type=simple
WorkingDirectory=$HOME
ExecStart=$bindir/ao daemon
Environment=AO_OPENCODE_SUBSCRIPTION=$subscription
Environment=PATH=$daemon_path
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload

# Restart any daemon the installer did not start, so every OpenCode 2 session it
# launches uses the pinned scope.
if "$bindir/ao" status >/dev/null 2>&1; then
  if ! systemctl --user is-active --quiet ao-daemon.service; then
    "$bindir/ao" stop --timeout 15s >/dev/null 2>&1 || true
  fi
fi

echo "installed $unit_dir/ao-daemon.service"
echo "subscription scope: $subscription"
echo "start: systemctl --user start ao-daemon"
echo "stop:  systemctl --user stop ao-daemon"

if [[ "$start_now" == "1" ]]; then
  systemctl --user start ao-daemon.service
  echo "started ao-daemon.service"
fi
