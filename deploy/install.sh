#!/bin/sh
# Install universal from source, and optionally set it up as a relay service.
#
#   sh deploy/install.sh                                  # just install the binary
#   sudo sh deploy/install.sh relay example.com SECRET [you@example.com] [20000-20100]
#   sudo sh deploy/install.sh monitor [TOKEN]
#
# Requires Go 1.22+ (https://go.dev/dl) and git.
set -eu

REPO="https://github.com/meguptasushil/universal"
BIN_DIR="${BIN_DIR:-/usr/local/bin}"

need() { command -v "$1" >/dev/null 2>&1 || { echo "error: $1 is required" >&2; exit 1; }; }
need go

src="$(cd "$(dirname "$0")/.." 2>/dev/null && pwd || true)"
if [ -z "$src" ] || ! grep -q "module github.com/meguptasushil/universal" "$src/go.mod" 2>/dev/null; then
	need git
	src="$(mktemp -d)"
	git clone --depth 1 "$REPO" "$src"
fi

echo "building universal from $src"
(cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$src/universal.bin" ./cmd/universal)
install -m 0755 "$src/universal.bin" "$BIN_DIR/universal" 2>/dev/null || {
	echo "cannot write to $BIN_DIR; re-run with sudo or set BIN_DIR" >&2; exit 1; }
rm -f "$src/universal.bin"
echo "installed $BIN_DIR/universal"

mode="${1:-}"
[ -z "$mode" ] && exit 0
need systemctl
id universal >/dev/null 2>&1 || useradd --system --home /var/lib/universal --shell /usr/sbin/nologin universal
mkdir -p /etc/universal

case "$mode" in
relay)
	domain="${2:?usage: install.sh relay <domain> <token> [email] [tcp-port-range]}"
	token="${3:?usage: install.sh relay <domain> <token> [email] [tcp-port-range]}"
	cat > /etc/universal/relay.env <<ENV
UNIVERSAL_DOMAIN=$domain
UNIVERSAL_TOKENS=$token
UNIVERSAL_EMAIL=${4:-}
UNIVERSAL_TCP_PORTS=${5:-}
ENV
	chmod 600 /etc/universal/relay.env
	install -m 0644 "$src/deploy/systemd/universal-relay.service" /etc/systemd/system/
	systemctl daemon-reload
	systemctl enable --now universal-relay
	echo
	echo "Relay is running. Make sure DNS points here:"
	echo "  A  $domain    -> <this server's IP>"
	echo "  A  *.$domain  -> <this server's IP>"
	echo "and ports 80 and 443 are open${5:+, plus TCP ports $5}."
	echo
	echo "On any machine:  universal expose 3000 --server https://$domain --token $token"
	;;
monitor)
	echo "UNIVERSAL_MONITOR_TOKEN=${2:-}" > /etc/universal/monitor.env
	chmod 600 /etc/universal/monitor.env
	install -m 0644 "$src/deploy/systemd/universal-monitor.service" /etc/systemd/system/
	systemctl daemon-reload
	systemctl enable --now universal-monitor
	echo "Monitor dashboard: http://127.0.0.1:8787/"
	;;
*)
	echo "unknown mode $mode (want relay or monitor)" >&2; exit 1
	;;
esac
