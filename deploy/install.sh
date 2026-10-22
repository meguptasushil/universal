#!/bin/sh
# Universal installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/meguptasushil/universal/main/deploy/install.sh | sh
#       installs the app. Then run "universal" to open the portal.
#
#   curl -fsSL .../install.sh | sudo sh -s -- relay [your-domain.com] [20000-20100]
#       turns this server into a relay (no domain needed) and prints the
#       connection code to give to users.
#
#   curl -fsSL .../install.sh | sudo sh -s -- monitor [TOKEN]
#       runs the AI agent monitor as a service.
#
# It downloads a ready-made program from the latest GitHub release. If that
# isn't possible it builds from source, which needs Go 1.22+ and git.
set -eu

REPO="meguptasushil/universal"
mode="${1:-}"

say() { printf '%s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

if [ -z "${BIN_DIR:-}" ]; then
	if [ "$(id -u)" = 0 ]; then BIN_DIR=/usr/local/bin; else BIN_DIR="$HOME/.local/bin"; fi
fi
mkdir -p "$BIN_DIR"

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in linux|darwin) ;; *) fail "unsupported system $os (on Windows use install.ps1)";; esac
arch="$(uname -m)"
case "$arch" in x86_64|amd64) arch=amd64;; aarch64|arm64) arch=arm64;; *) fail "unsupported CPU $arch";; esac

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

download() {
	url="https://github.com/$REPO/releases/latest/download/universal-$os-$arch"
	say "Downloading Universal for $os/$arch..."
	if have curl; then curl -fsSL "$url" -o "$tmp/universal" 2>/dev/null
	elif have wget; then wget -q "$url" -O "$tmp/universal"
	else return 1; fi
}

build() {
	have go || fail "no ready-made download is available yet, and Go is not installed to build it (https://go.dev/dl)"
	have git || fail "git is needed to build from source"
	say "Building Universal from source..."
	git clone --depth 1 "https://github.com/$REPO" "$tmp/src" >/dev/null 2>&1
	(cd "$tmp/src" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/universal" ./cmd/universal)
}

download || build
chmod +x "$tmp/universal"
mv "$tmp/universal" "$BIN_DIR/universal"
say "Installed $BIN_DIR/universal"
case ":$PATH:" in *":$BIN_DIR:"*) ;; *) say "Note: add $BIN_DIR to your PATH, or run it as $BIN_DIR/universal";; esac

if [ -z "$mode" ]; then
	say ""
	say "Done! Run \"universal\" to open the portal in your browser."
	exit 0
fi

[ "$(id -u)" = 0 ] || fail "setting up a service needs root: run it with sudo"
have systemctl || fail "this system has no systemd; run \"universal $mode\" yourself instead"
id universal >/dev/null 2>&1 || useradd --system --home /var/lib/universal --shell /usr/sbin/nologin universal
mkdir -p /etc/universal /var/lib/universal
chown universal /var/lib/universal

case "$mode" in
relay)
	domain="${2:-}"
	ports="${3:-}"
	cat > /etc/universal/relay.env <<ENV
UNIVERSAL_DOMAIN=$domain
UNIVERSAL_TCP_PORTS=$ports
UNIVERSAL_EMAIL=${UNIVERSAL_EMAIL:-}
ENV
	chmod 600 /etc/universal/relay.env
	cat > /etc/systemd/system/universal-relay.service <<'UNIT'
[Unit]
Description=Universal relay
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/universal/relay.env
ExecStart=/usr/local/bin/universal relay --state /var/lib/universal
User=universal
AmbientCapabilities=CAP_NET_BIND_SERVICE
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/universal
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
	[ "$BIN_DIR" = /usr/local/bin ] || cp "$BIN_DIR/universal" /usr/local/bin/universal
	# open the firewall if ufw is in use
	if have ufw && ufw status 2>/dev/null | grep -q "Status: active"; then
		ufw allow 80/tcp >/dev/null; ufw allow 443/tcp >/dev/null
		[ -n "$ports" ] && ufw allow "$(echo "$ports" | tr - :)/tcp" >/dev/null
	fi
	systemctl daemon-reload
	systemctl enable --now universal-relay >/dev/null
	# wait for the service to create its token, then read the code from it
	i=0
	while [ ! -s /var/lib/universal/relay-token ] && [ $i -lt 30 ]; do sleep 1; i=$((i + 1)); done
	code=""
	if [ -s /var/lib/universal/relay-token ]; then
		code="$(UNIVERSAL_DOMAIN="$domain" /usr/local/bin/universal relay --state /var/lib/universal --show-code 2>/dev/null || true)"
	fi
	say ""
	say "Your relay is running."
	if [ -z "$domain" ]; then
		say "It uses a free address based on this server's IP, so no domain or DNS setup is needed."
	else
		say "Make sure DNS points $domain and *.$domain at this server."
	fi
	say "If your cloud provider has a firewall, open ports 80 and 443${ports:+ and $ports}."
	say ""
	say "Connection code (give it to anyone who should publish apps here):"
	say ""
	if [ -n "$code" ]; then
		say "    $code"
	else
		say "    (not ready yet; see: journalctl -u universal-relay)"
	fi
	say ""
	;;
monitor)
	echo "UNIVERSAL_MONITOR_TOKEN=${2:-}" > /etc/universal/monitor.env
	chmod 600 /etc/universal/monitor.env
	cat > /etc/systemd/system/universal-monitor.service <<'UNIT'
[Unit]
Description=Universal AI agent monitor
After=network-online.target

[Service]
EnvironmentFile=-/etc/universal/monitor.env
ExecStart=/usr/local/bin/universal monitor --data /var/lib/universal/monitor
User=universal
Restart=always
RestartSec=2
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/universal
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
	[ "$BIN_DIR" = /usr/local/bin ] || cp "$BIN_DIR/universal" /usr/local/bin/universal
	systemctl daemon-reload
	systemctl enable --now universal-monitor >/dev/null
	say "Monitor dashboard: http://127.0.0.1:8787/"
	;;
*)
	fail "unknown mode $mode (use relay or monitor, or nothing to just install)"
	;;
esac
