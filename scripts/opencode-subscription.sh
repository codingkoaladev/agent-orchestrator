#!/usr/bin/env bash
# Manage OpenCode subscription scopes for the local coordinator.
#
# A "scope" is one XDG data home holding OpenCode's saved accounts and sessions
# for exactly one subscription. Launching ordinary `opencode` with that
# XDG_DATA_HOME selects the subscription without the oc1/oc2/oc3 shell
# functions, and without `opencode auth switch` touching another worker's
# credential store.
#
# The script never prints credential contents. `adopt` copies an existing
# auth.json once, keeping mode 0600, and refuses to overwrite a scope that
# already has one.
set +x
set -euo pipefail

parent="${XDG_DATA_HOME:-$HOME/.local/share}"
scopes="$parent/opencode-subscriptions"

usage() {
  cat <<'USAGE'
usage: opencode-subscription.sh <command> [args]

commands:
  path <label>                     Print the scope data home for label.
  status [label...]                Show each scope's saved-account count (ids only).
  adopt <label> --from-profile <p> Copy <p>/opencode/auth.json into the scope once.
  login <label>                    Run `opencode auth login` inside the scope.
  list                             List scopes that exist on this host.

A label matches ^[a-z0-9][a-z0-9._-]{0,63}$ and is not a credential.
Set AO_OPENCODE_SUBSCRIPTION=<label> for the Orchestrator daemon so every
OpenCode 2 session it launches uses that scope.
USAGE
}

valid_label() {
  [[ "$1" =~ ^[a-z0-9][a-z0-9._-]{0,63}$ ]]
}

scope_path() {
  local label="$1"
  valid_label "$label" || { echo "invalid label: $label" >&2; exit 2; }
  printf '%s/%s\n' "$scopes" "$label"
}

scope_status() {
  local label="$1" home out
  home="$(scope_path "$label")"
  if [[ ! -d "$home" ]]; then
    printf '%-20s %s\n' "$label" "absent"
    return 0
  fi
  out="$(XDG_DATA_HOME="$home" timeout 60 opencode auth list --standalone --format json 2>/dev/null || true)"
  printf '%s' "$out" | python3 -c '
import json,sys
label=sys.argv[1]
raw=sys.stdin.read()
try:
    data=json.loads(raw)
except Exception:
    print(f"{label:<20} present, accounts unreadable"); raise SystemExit
total=sum(len(i.get("connections") or []) for i in data)
ids=",".join(c.get("id","?") for i in data for c in (i.get("connections") or []))
print(f"{label:<20} accounts={total} ids={ids}")
' "$label"
}

cmd="${1:-}"
case "$cmd" in
  path)
    [[ $# -eq 2 ]] || { usage; exit 2; }
    scope_path "$2"
    ;;
  status)
    shift || true
    if [[ $# -eq 0 ]]; then
      if [[ -d "$scopes" ]]; then
        while IFS= read -r d; do scope_status "$(basename "$d")"; done < <(find "$scopes" -mindepth 1 -maxdepth 1 -type d | sort)
      else
        echo "no scopes under $scopes"
      fi
    else
      for label in "$@"; do scope_status "$label"; done
    fi
    ;;
  adopt)
    [[ $# -eq 4 && "$3" == "--from-profile" ]] || { usage; exit 2; }
    label="$2"; source="$4"
    home="$(scope_path "$label")"
    src="$source/opencode/auth.json"
    [[ -f "$src" ]] || { echo "source profile has no opencode/auth.json" >&2; exit 1; }
    if [[ -e "$home/opencode/auth.json" ]]; then
      echo "scope $label already has a credential store; refusing to overwrite" >&2
      exit 1
    fi
    mkdir -p "$home/opencode"
    chmod 700 "$home"
    install -m 600 "$src" "$home/opencode/auth.json"
    echo "adopted existing saved account into scope $label (contents not read or printed)"
    ;;
  login)
    [[ $# -eq 2 ]] || { usage; exit 2; }
    home="$(scope_path "$2")"
    mkdir -p "$home"
    chmod 700 "$home"
    XDG_DATA_HOME="$home" exec opencode auth login
    ;;
  list)
    [[ -d "$scopes" ]] || { echo "no scopes under $scopes"; exit 0; }
    find "$scopes" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort
    ;;
  *)
    usage
    exit 2
    ;;
esac
