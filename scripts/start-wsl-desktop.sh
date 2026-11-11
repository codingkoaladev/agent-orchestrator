#!/usr/bin/env bash
# Launch the local desktop without executing the AppImage/FUSE runtime.
set +x
set -euo pipefail
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${1:-}" == "--help" ]]; then
  printf '%s\n' 'Usage: scripts/start-wsl-desktop.sh [--replace-service]' 'Uses the installed image as a data archive and the current source-built daemon.'
  exit 0
fi
replace_service=0
if [[ "${1:-}" == "--replace-service" && "$#" == 1 ]]; then
  replace_service=1
elif [[ "$#" != 0 ]]; then
  echo 'Unknown argument; use --help.' >&2
  exit 2
fi
image="${AO_LOCAL_DESKTOP_IMAGE:-$HOME/.ao/agent-orchestrator.AppImage}"
[[ -f "$image" && -x "$repo_dir/backend/bin/ao" ]] || { echo 'Installed desktop image or source-built daemon is missing.' >&2; exit 1; }
command -v unsquashfs >/dev/null || { echo 'unsquashfs is required.' >&2; exit 1; }
mkdir -p "$HOME/.ao"
exec 9>"$HOME/.ao/wsl-desktop.lock"
flock -n 9 || { echo 'The WSL desktop launcher is already running.' >&2; exit 1; }
app_dir="$(python3 - "$image" <<'EXTRACT'
import hashlib,mmap,subprocess,sys
from pathlib import Path
image=Path(sys.argv[1]); digest=hashlib.sha256(image.read_bytes()).hexdigest()[:16]
target=Path.home()/'.ao/desktop-extracted'/digest
if not (target/'AppRun').is_file():
    offset=None
    with image.open('rb') as file,mmap.mmap(file.fileno(),0,access=mmap.ACCESS_READ) as mapped:
        at=0
        while True:
            at=mapped.find(b'hsqs',at)
            if at<0:break
            check=subprocess.run(['unsquashfs','-s','-o',str(at),str(image)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5)
            if check.returncode==0:offset=at;break
            at+=4
    if offset is None:sys.exit('No valid embedded desktop filesystem found.')
    target.parent.mkdir(parents=True,exist_ok=True)
    subprocess.run(['unsquashfs','-quiet','-no-progress','-processors','1','-o',str(offset),'-d',str(target),str(image)],stdout=subprocess.DEVNULL,check=True)
print(target)
EXTRACT
)"
# Using the bundled launch path avoids packaged app.asar as an OS working directory.
install -m 755 "$repo_dir/backend/bin/ao" "$app_dir/resources/daemon/ao"
if systemctl --user is-active --quiet ao-daemon; then
  [[ "$replace_service" == 1 ]] || { echo 'Stop ao-daemon first, or use --replace-service when no workers are active.' >&2; exit 1; }
  # A failed/unknown probe must never authorize replacing a live worker's daemon.
  python3 - <<'CHECK'
import json,sys,urllib.request
try:
    with urllib.request.urlopen('http://127.0.0.1:3001/api/v1/sessions',timeout=5) as response:
        body=json.load(response)
    sessions=body['sessions']
    if not isinstance(sessions,list): raise ValueError()
    for session in sessions:
        if session.get('isTerminated'): continue
        activity=session.get('activity',{}).get('state')
        if activity not in ('idle','exited'):
            sys.exit('A worker is active or its state is unknown; leave the service running.')
except (OSError,ValueError,KeyError,TypeError):
    sys.exit('Cannot verify worker state; leave the service running.')
CHECK
  systemctl --user stop ao-daemon
fi
restore_service_on_failure() {
  if ! "$repo_dir/backend/bin/ao" status >/dev/null 2>&1; then
    systemctl --user start ao-daemon || true
  fi
}
trap restore_service_on_failure EXIT
unset APPIMAGE AO_APPIMAGE AO_DAEMON_COMMAND
export AO_OPENCODE_SUBSCRIPTION="${AO_OPENCODE_SUBSCRIPTION:-subscription-3}"
export AO_TELEMETRY_REMOTE=off
export AO_KEEP_DAEMON=1
args=(--disable-gpu)
if [[ -n "${AO_LOCAL_DESKTOP_DEBUG_PORT:-}" ]]; then
  [[ "$AO_LOCAL_DESKTOP_DEBUG_PORT" =~ ^[0-9]+$ ]] || exit 2
  args+=(--remote-debugging-address=127.0.0.1 "--remote-debugging-port=$AO_LOCAL_DESKTOP_DEBUG_PORT")
fi
"$app_dir/AppRun" "${args[@]}" 9>&-
