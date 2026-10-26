#!/usr/bin/env bash
# Installs, updates or removes the MPA Bridge on a Pelican Wings node.
#
#   install.sh [--allowed-ips LIST] [--port PORT] [--binary FILE] [--version X.Y.Z] [--yes]
#   install.sh --uninstall [--purge]
#
# Run as root. Without --binary the script takes mpa-bridge-linux-amd64 from its
# own folder (an unpacked release), else downloads it from the GitHub release and
# checks its SHA-256. An existing /etc/pelican/mpa-bridge.yml is kept, so running
# the script again updates the bridge. A config without a token gets one; the
# script prints the token for the panel at the end.
set -euo pipefail

REPO="FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin"
ASSET="mpa-bridge-linux-amd64"
BIN_PATH="/usr/local/bin/mpa-bridge"
CONFIG_PATH="/etc/pelican/mpa-bridge.yml"
WINGS_CONFIG="/etc/pelican/config.yml"
UNIT_PATH="/etc/systemd/system/mpa-bridge.service"

allowed_ips=""
port=""
binary=""
version=""
assume_yes=0
uninstall=0
purge=0

usage() {
	sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'
	exit "${1:-0}"
}

die() {
	echo "error: $*" >&2
	exit 1
}

info() {
	echo "==> $*"
}

# ask PROMPT DEFAULT: reads an answer from the terminal, also when the script is piped into bash.
ask() {
	local answer
	if [ ! -r /dev/tty ]; then
		die "no terminal to ask \"$1\", pass the value as an option (see --help)"
	fi
	read -r -p "$1" answer </dev/tty
	echo "${answer:-$2}"
}

# new_token: 32 random bytes as 64 hex characters, the bridge token the panel sends.
new_token() {
	if command -v openssl >/dev/null; then
		openssl rand -hex 32
	else
		head -c 32 /dev/urandom | od -An -v -tx1 | tr -d ' \n'
		echo
	fi
}

# config_token: the token value in the config, without quotes and comment ("" if none).
config_token() {
	sed -n 's/^token:[[:space:]]*//p' "$CONFIG_PATH" | head -n1 |
		sed 's/[[:space:]]\{1,\}#.*$//; s/[[:space:]]*$//; s/^"\(.*\)"$/\1/'"; s/^'\(.*\)'\$/\1/"
}

while [ $# -gt 0 ]; do
	case "$1" in
	--allowed-ips) allowed_ips="${2:?--allowed-ips needs a value}"; shift 2 ;;
	--allowed-ips=*) allowed_ips="${1#*=}"; shift ;;
	--port) port="${2:?--port needs a value}"; shift 2 ;;
	--port=*) port="${1#*=}"; shift ;;
	--binary) binary="${2:?--binary needs a value}"; shift 2 ;;
	--binary=*) binary="${1#*=}"; shift ;;
	--version) version="${2:?--version needs a value}"; shift 2 ;;
	--version=*) version="${1#*=}"; shift ;;
	-y | --yes) assume_yes=1; shift ;;
	--uninstall) uninstall=1; shift ;;
	--purge) purge=1; shift ;;
	-h | --help) usage 0 ;;
	*) echo "unknown option: $1" >&2; usage 1 ;;
	esac
done

[ "$(id -u)" -eq 0 ] || die "run as root"
[ "$(uname -s)" = "Linux" ] || die "the bridge runs on Linux only"
command -v systemctl >/dev/null || die "systemd is required"

if [ "$uninstall" -eq 1 ]; then
	info "Removing the MPA Bridge"
	systemctl disable --now mpa-bridge.service 2>/dev/null || true
	rm -f "$UNIT_PATH" "$BIN_PATH"
	systemctl daemon-reload
	if [ "$purge" -eq 1 ]; then
		rm -f "$CONFIG_PATH"
	elif [ -f "$CONFIG_PATH" ]; then
		echo "Kept $CONFIG_PATH (remove it with --purge)."
	fi
	echo "Done. Profiler files the bridge left in the containers' /tmp are removed with the next server restart."
	exit 0
fi

case "$(uname -m)" in
x86_64 | amd64) ;;
*) die "only linux-amd64 builds exist, this node is $(uname -m)" ;;
esac
[ -f "$WINGS_CONFIG" ] || die "$WINGS_CONFIG not found: install and configure Wings first"
if [ ! -S /var/run/docker.sock ] && [ -z "${DOCKER_HOST:-}" ]; then
	echo "warning: /var/run/docker.sock not found, the bridge needs Docker" >&2
fi

if [ -n "$port" ] && ! [[ "$port" =~ ^[0-9]+$ && "$port" -ge 1 && "$port" -le 65535 ]]; then
	die "--port must be a number from 1 to 65535"
fi

# 1. Binary
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [ -z "$binary" ] && [ -f "$script_dir/$ASSET" ]; then
	binary="$script_dir/$ASSET"
fi
if [ -z "$binary" ]; then
	if [ -n "$version" ]; then
		base="https://github.com/$REPO/releases/download/v${version#v}"
	else
		base="https://github.com/$REPO/releases/latest/download"
	fi
	info "Downloading $ASSET from $base"
	if command -v curl >/dev/null; then
		curl -fsSL -o "$workdir/$ASSET" "$base/$ASSET"
		curl -fsSL -o "$workdir/$ASSET.sha256" "$base/$ASSET.sha256"
	elif command -v wget >/dev/null; then
		wget -qO "$workdir/$ASSET" "$base/$ASSET"
		wget -qO "$workdir/$ASSET.sha256" "$base/$ASSET.sha256"
	else
		die "curl or wget is required to download the bridge (or pass --binary)"
	fi
	(cd "$workdir" && sha256sum -c --quiet "$ASSET.sha256") || die "SHA-256 of the download does not match"
	binary="$workdir/$ASSET"
elif [ -f "$binary.sha256" ]; then
	expected="$(cut -d' ' -f1 "$binary.sha256")"
	actual="$(sha256sum "$binary" | cut -d' ' -f1)"
	[ "$expected" = "$actual" ] || die "SHA-256 of $binary does not match $binary.sha256"
fi
[ -f "$binary" ] || die "$binary not found"
chmod 0755 "$binary"
new_version="$("$binary" -version 2>/dev/null)" || die "$binary does not run on this node"
info "Installing $new_version to $BIN_PATH"
install -m 0755 "$binary" "$BIN_PATH.new"
mv -f "$BIN_PATH.new" "$BIN_PATH"

# 2. Config (holds the bridge token, so only root may read it)
if [ -f "$CONFIG_PATH" ]; then
	info "Keeping $CONFIG_PATH"
	[ -z "$allowed_ips$port" ] || echo "warning: --allowed-ips and --port only apply to a new config, edit $CONFIG_PATH instead" >&2
	chmod 0600 "$CONFIG_PATH"
	if [ -z "$(config_token)" ]; then
		# Bridges before 1.1.0 used the Wings token; drop an empty token line, then add a new one.
		info "Adding a token to $CONFIG_PATH"
		sed -i '/^token:/d' "$CONFIG_PATH"
		{
			echo "# Token the panel sends (Admin -> Nodes -> node -> MPA Bridge), not the Wings token."
			echo "token: \"$(new_token)\""
		} >>"$CONFIG_PATH"
	fi
else
	if [ -z "$allowed_ips" ]; then
		[ "$assume_yes" -eq 0 ] || die "--yes needs --allowed-ips for a new config"
		echo "The bridge only accepts connections from the panel. Which addresses does the panel"
		echo "connect from? IPs or CIDRs, comma separated, e.g. 10.0.0.2 or 10.0.0.0/24."
		allowed_ips="$(ask "Panel IP(s): " "")"
		[ -n "$allowed_ips" ] || die "the panel's IP is required (allowed_ips)"
	fi
	if [ -z "$port" ]; then
		if [ "$assume_yes" -eq 1 ]; then
			port=8090
		else
			port="$(ask "Port [8090]: " "8090")"
		fi
		[[ "$port" =~ ^[0-9]+$ && "$port" -ge 1 && "$port" -le 65535 ]] || die "port must be a number from 1 to 65535"
	fi
	info "Writing $CONFIG_PATH"
	(umask 077 && : >"$CONFIG_PATH")
	{
		echo "# MPA Bridge, see https://github.com/$REPO"
		echo "# Restart after changes: systemctl restart mpa-bridge"
		echo "# Token the panel sends (Admin -> Nodes -> node -> MPA Bridge), not the Wings token."
		echo "token: \"$(new_token)\""
		echo "port: $port"
		echo "host: 0.0.0.0"
		echo "# Addresses allowed to connect (IPs or CIDRs), i.e. the panel. Empty = everyone."
		echo "allowed_ips:"
		IFS=',' read -ra ips <<<"$allowed_ips"
		for ip in "${ips[@]}"; do
			ip="$(echo "$ip" | tr -d '[:space:]')"
			[ -z "$ip" ] || echo "  - \"$ip\""
		done
		echo "max_concurrent_profiles: 2"
		echo "max_profile_seconds: 120"
		echo "max_tick_monitors: 10"
	} >"$CONFIG_PATH"
	chmod 0600 "$CONFIG_PATH"
fi

# 3. Service
info "Installing $UNIT_PATH"
cat >"$UNIT_PATH" <<'UNIT'
[Unit]
Description=MPA Bridge (Minecraft Performance Analyser)
Documentation=https://github.com/FreshDoktor/Pelican-Minecraft_Performance_Analyser-WingPlugin
After=docker.service network-online.target
Wants=network-online.target
Requires=docker.service
PartOf=docker.service
StartLimitIntervalSec=180
StartLimitBurst=30

[Service]
# Root: the bridge reads /proc/<pid>/root of the server containers, signals
# their JVMs and talks to the Docker socket, like Wings.
User=root
WorkingDirectory=/etc/pelican
ExecStart=/usr/local/bin/mpa-bridge
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable mpa-bridge.service >/dev/null 2>&1
systemctl restart mpa-bridge.service

# 4. Check
info "Checking the bridge"
listen_port="$(sed -n 's/^port:[[:space:]]*\([0-9]*\).*/\1/p' "$CONFIG_PATH" | head -n1)"
listen_port="${listen_port:-8090}"
ok=0
for _ in $(seq 1 10); do
	sleep 1
	if systemctl is-active --quiet mpa-bridge.service && command -v curl >/dev/null; then
		# Any HTTP answer will do: 401 without a token, 403 when localhost isn't in allowed_ips.
		code="$(curl -sk -o /dev/null -w '%{http_code}' "http://127.0.0.1:$listen_port/api/v1/status" || true)"
		[ "$code" = "000" ] && code="$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:$listen_port/api/v1/status" || true)"
		if [ "$code" != "000" ]; then
			ok=1
			break
		fi
	elif systemctl is-active --quiet mpa-bridge.service; then
		ok=1
		break
	fi
done
if [ "$ok" -ne 1 ]; then
	journalctl -u mpa-bridge.service -n 20 --no-pager >&2 || true
	die "the bridge did not start, see the log above (journalctl -u mpa-bridge)"
fi

scheme="http"
if sed -n '/^api:/,/^[^[:space:]]/p' "$WINGS_CONFIG" | sed -n '/ssl:/,/enabled:/p' | grep -q 'enabled:[[:space:]]*true'; then
	scheme="https"
fi
token="$(config_token)"
case "$token" in
'' | *'$'* | file://*) token="(as set by token in $CONFIG_PATH)" ;;
esac
cat <<EOF

$new_version is running on port $listen_port ($scheme, like Wings).

Bridge token (keep it secret, it is not the Wings token):

    $token

Next steps:
  - Let the panel reach this port, and only the panel (firewall).
  - In the panel: Admin -> Nodes -> this node -> tab "MPA Bridge". Enter the address the panel
    reaches this node under, e.g. $scheme://<node FQDN>:$listen_port or $scheme://10.0.0.5:$listen_port,
    paste the bridge token above, then "Test connection" and save.
  - Log: journalctl -u mpa-bridge -f
EOF
