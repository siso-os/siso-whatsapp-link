#!/bin/bash
# Write this laptop's siso-wa config: the gateway's tailnet address and the agent token (never the API token).
#   deploy/agent-env.sh    -> ~/.config/siso-secrets/whatsapp-agent.env (0600; SISO_WA_ENV overrides the path)
# The token goes straight from ssh into the file and is never printed. On the laptop the mini is WA_HOST=shaans-mac-mini.
set -euo pipefail
HOST=${WA_HOST:-mac-mini}
OUT=${SISO_WA_ENV:-$HOME/.config/siso-secrets/whatsapp-agent.env}
SSH=(ssh -o ConnectTimeout=10 -o RequestTTY=no -o RemoteCommand=none "$HOST")
TSIP=$("${SSH[@]}" '/Applications/Tailscale.app/Contents/MacOS/Tailscale ip -4 2>/dev/null | head -1')
[ -n "$TSIP" ] || { echo "no tailnet address on $HOST" >&2; exit 1; }
umask 077
mkdir -p "$(dirname "$OUT")"
tmp=$(mktemp "$OUT.XXXXXX"); trap 'rm -f "$tmp"' EXIT
{
  echo "# siso-wa: the WhatsApp gateway's agent token (reaches only the bound agent chat). Written by deploy/agent-env.sh."
  echo "WA_URL=http://$TSIP:5480"
  printf 'WA_AGENT_TOKEN='
  "${SSH[@]}" 'cat "$HOME/Library/Application Support/siso-whatsapp-link/agent-token"'
} > "$tmp"
grep -qE '^WA_AGENT_TOKEN=[0-9a-f]{64}$' "$tmp" || { echo "no agent token on $HOST yet (deploy 0.3+ first)" >&2; exit 1; }
chmod 600 "$tmp"; mv "$tmp" "$OUT"; trap - EXIT
echo "wrote $OUT (0600): WA_URL=http://$TSIP:5480, agent token copied (not shown)"
