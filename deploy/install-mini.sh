#!/bin/bash
# Build for the mini (darwin/arm64), copy the binary and the launchd job, (re)load it, and check /healthz.
#   deploy/install-mini.sh            install or upgrade and start (KeepAlive: it runs 24/7)
#   deploy/install-mini.sh --stop     unload the job (the link and the store stay on disk)
# Listens on the mini's loopback and its tailnet address only. Send and read receipts stay off (plist env).
set -euo pipefail
cd "$(dirname "$0")/.."
HOST=${WA_HOST:-mac-mini}
SSH=(ssh -o ConnectTimeout=10 -o RequestTTY=no -o RemoteCommand=none "$HOST")
LABEL=com.siso.whatsapp-link
if [ "${1:-}" = "--stop" ]; then
  "${SSH[@]}" "launchctl bootout gui/\$(id -u)/$LABEL 2>/dev/null; echo stopped"
  exit 0
fi
TSIP=$("${SSH[@]}" '/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4 2>/dev/null | head -1')
[ -n "$TSIP" ] || { echo "no tailnet address on $HOST" >&2; exit 1; }
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/siso-whatsapp-link" .
RHOME=$("${SSH[@]}" 'echo $HOME')
sed -e "s#__HOME__#$RHOME#g" -e "s#__LISTEN__#127.0.0.1:5480,$TSIP:5480#" deploy/com.siso.whatsapp-link.plist > "$tmp/$LABEL.plist"
"${SSH[@]}" 'mkdir -p ~/.local/siso-whatsapp-link ~/Library/LaunchAgents && chmod 700 ~/.local/siso-whatsapp-link'
scp -q -o RequestTTY=no -o RemoteCommand=none "$tmp/siso-whatsapp-link" "$HOST:.local/siso-whatsapp-link/siso-whatsapp-link.new"
scp -q -o RequestTTY=no -o RemoteCommand=none "$tmp/$LABEL.plist" "$HOST:Library/LaunchAgents/$LABEL.plist"
"${SSH[@]}" "set -e; cd ~/.local/siso-whatsapp-link; mv siso-whatsapp-link.new siso-whatsapp-link; \
  launchctl bootout gui/\$(id -u)/$LABEL 2>/dev/null || true; sleep 1; \
  launchctl bootstrap gui/\$(id -u) ~/Library/LaunchAgents/$LABEL.plist; sleep 3; \
  curl -sf -m 5 http://127.0.0.1:5480/healthz && launchctl print gui/\$(id -u)/$LABEL | grep -E '^\s*(state|pid) '"
echo "installed on $HOST, tailnet http://$TSIP:5480"
