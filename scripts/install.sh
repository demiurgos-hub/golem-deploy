#!/usr/bin/env bash
# Minimal Golem Deploy installer for Ubuntu and Debian hosts.
#
#   sudo scripts/install.sh [options] /path/to/golem-deploy
#
# Installs an already built golem-deploy binary as the systemd service
# golem-deploy, running as the system user golem-deploy with its data in
# /var/lib/golem-deploy. It checks the host, installs git, git-lfs,
# openssh-client and ca-certificates when missing, and never installs Docker
# or changes firewall rules. Re-running it upgrades the binary and the unit and restarts the
# service; an existing settings file or data directory is kept.
# See docs/INSTALL.md.
set -euo pipefail

readonly SERVICE=golem-deploy
readonly SVC_USER=golem-deploy
readonly BIN_DEST=/usr/local/bin/golem-deploy
readonly DATA_DIR=/var/lib/golem-deploy
readonly CONF_DIR=/etc/golem-deploy
readonly ENV_FILE=$CONF_DIR/golem-deploy.env
readonly TLS_DIR=$CONF_DIR/tls
readonly TLS_CERT_DEST=$TLS_DIR/cert.pem
readonly TLS_KEY_DEST=$TLS_DIR/key.pem
readonly UNIT_DEST=/etc/systemd/system/golem-deploy.service
readonly DEFAULT_LISTEN_HTTP=127.0.0.1:8000
readonly DEFAULT_LISTEN_TLS=:8443
readonly DEFAULT_PORT_RANGE=7000-7099
readonly START_TIMEOUT=30

script_dir=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly UNIT_SRC=$script_dir/../deploy/golem-deploy.service

usage() {
	cat <<EOF
Usage: sudo $0 [options] /path/to/golem-deploy

Installs a built golem-deploy binary as the systemd service "golem-deploy" on
Ubuntu or Debian. Docker Engine must already be installed and running.

Options (they only shape a new $ENV_FILE; an existing one is kept):
  --public-host NAMES   comma-separated host names the panel is reached by
  --listen ADDR         panel listen address (default $DEFAULT_LISTEN_HTTP,
                        or $DEFAULT_LISTEN_TLS with TLS)
  --port-range LO-HI    host ports for game servers (default $DEFAULT_PORT_RANGE)
  --tls-cert FILE       TLS certificate (PEM), copied to $TLS_CERT_DEST
  --tls-key FILE        TLS private key (PEM), copied to $TLS_KEY_DEST
  -h, --help            show this help

Without TLS files the panel listens on loopback only (plain HTTP); reach it
through an SSH tunnel. Re-run with --tls-cert and --tls-key to install renewed
certificates.
EOF
}

say() { printf '==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# --- arguments ---------------------------------------------------------------

public_host=''
listen=''
port_range=''
tls_cert=''
tls_key=''
binary=''
config_opts=()

need_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		die "$1 needs a value (see --help)"
	fi
}

while [ "$#" -gt 0 ]; do
	case $1 in
	-h | --help)
		usage
		exit 0
		;;
	--public-host | --listen | --port-range | --tls-cert | --tls-key)
		need_value "$@"
		opt=$1 val=$2
		shift 2
		;;
	--public-host=* | --listen=* | --port-range=* | --tls-cert=* | --tls-key=*)
		opt=${1%%=*} val=${1#*=}
		shift
		[ -n "$val" ] || die "$opt needs a value (see --help)"
		;;
	--)
		shift
		break
		;;
	-*)
		die "unknown option $1 (see --help)"
		;;
	*)
		[ -z "$binary" ] || die "only one binary path is accepted (got $binary and $1)"
		binary=$1
		shift
		continue
		;;
	esac
	case $opt in
	--public-host) public_host=$val ;;
	--listen) listen=$val ;;
	--port-range) port_range=$val ;;
	--tls-cert) tls_cert=$val ;;
	--tls-key) tls_key=$val ;;
	esac
	case $opt in
	--public-host | --listen | --port-range) config_opts+=("$opt") ;;
	esac
done
if [ "$#" -gt 0 ]; then
	if [ -n "$binary" ] || [ "$#" -ne 1 ]; then
		die "only one binary path is accepted"
	fi
	binary=$1
fi
[ -n "$binary" ] || {
	usage >&2
	exit 2
}

# --- validation of the options -----------------------------------------------

if [ -n "$tls_cert" ] || [ -n "$tls_key" ]; then
	if [ -z "$tls_cert" ] || [ -z "$tls_key" ]; then
		die "--tls-cert and --tls-key must be given together"
	fi
	for f in "$tls_cert" "$tls_key"; do
		if [ ! -f "$f" ] || [ ! -r "$f" ] || [ ! -s "$f" ]; then
			die "$f is not a readable, non-empty file"
		fi
		grep -q -- '-----BEGIN ' "$f" || die "$f does not look like a PEM file"
	done
	grep -q -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "$tls_key" || die "$tls_key does not contain a PEM private key"
	grep -q -- '-----BEGIN CERTIFICATE-----' "$tls_cert" || die "$tls_cert does not contain a PEM certificate"
fi

if [ -z "$listen" ]; then
	if [ -n "$tls_cert" ]; then listen=$DEFAULT_LISTEN_TLS; else listen=$DEFAULT_LISTEN_HTTP; fi
fi
[ -n "$port_range" ] || port_range=$DEFAULT_PORT_RANGE

# Split HOST:PORT (HOST may be empty or a bracketed IPv6 address).
case $listen in
\[*\]:*)
	listen_host=${listen%:*}
	listen_host=${listen_host#[}
	listen_host=${listen_host%]}
	;;
*:*) listen_host=${listen%:*} ;;
*) die "--listen must be HOST:PORT, for example $DEFAULT_LISTEN_HTTP (got $listen)" ;;
esac
listen_port=${listen##*:}
if [[ ! $listen_port =~ ^[0-9]{1,5}$ ]] || [ "$((10#$listen_port))" -lt 1024 ] || [ "$((10#$listen_port))" -gt 65535 ]; then
	die "--listen port must be between 1024 and 65535 (the service runs without the capability to bind lower ports; got $listen)"
fi
[[ $listen_host =~ ^[A-Za-z0-9.:-]*$ ]] || die "--listen host contains unexpected characters (got $listen)"
if [ -z "$tls_cert" ]; then
	case $listen_host in
	127.* | ::1 | localhost) ;;
	*)
		die "without --tls-cert/--tls-key the panel serves plain HTTP and may only listen on a loopback address (got $listen);" \
			"configure TLS, or see docs/INSTALL.md for running it behind a TLS-terminating reverse proxy"
		;;
	esac
fi

[[ $port_range =~ ^([0-9]{1,5})-([0-9]{1,5})$ ]] || die "--port-range must be LO-HI, for example $DEFAULT_PORT_RANGE (got $port_range)"
port_lo=$((10#${BASH_REMATCH[1]}))
port_hi=$((10#${BASH_REMATCH[2]}))
if [ "$port_lo" -lt 1024 ] || [ "$port_hi" -gt 32767 ] || [ "$port_lo" -gt "$port_hi" ]; then
	die "--port-range must lie within 1024-32767 with LO <= HI (got $port_range)"
fi
if [ "$((10#$listen_port))" -ge "$port_lo" ] && [ "$((10#$listen_port))" -le "$port_hi" ]; then
	die "--port-range $port_range contains the panel's listen port $listen_port"
fi

if [ -n "$public_host" ]; then
	[[ $public_host =~ ^[A-Za-z0-9.,-]+$ ]] || die "--public-host takes comma-separated host names (letters, digits, dots and hyphens; got $public_host)"
fi

# --- host checks -------------------------------------------------------------

say "Checking the host"

[ "$(id -u)" -eq 0 ] || die "run this script as root (for example with sudo)"

[ -r /etc/os-release ] || die "cannot read /etc/os-release; only Ubuntu and Debian are supported"
# shellcheck source=/dev/null
os_id=$(. /etc/os-release && printf '%s' "${ID:-}")
# shellcheck source=/dev/null
os_name=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-$os_id}")
case $os_id in
ubuntu | debian) note "operating system: $os_name" ;;
*) die "unsupported operating system \"$os_name\"; this installer supports Ubuntu and Debian only" ;;
esac

if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
	die "systemd is not running as the init system; the installer needs systemd to run the service"
fi
note "systemd: $(systemctl --version | head -n 1)"

docker_hint="install Docker Engine following https://docs.docker.com/engine/install/$os_id/ and run this script again (this installer does not install Docker)"
command -v docker >/dev/null 2>&1 || die "the docker command was not found; $docker_hint"
if ! docker_version=$(docker version --format '{{.Server.Version}}' 2>/dev/null) || [ -z "$docker_version" ]; then
	die "the Docker daemon is not reachable (is it running? try: systemctl status docker); if Docker is not installed, $docker_hint"
fi
note "Docker Engine: $docker_version"
getent group docker >/dev/null ||
	die "the docker group does not exist; Docker Engine installed from Docker's packages creates it (a snap or rootless Docker is not supported): $docker_hint"

[ -f "$binary" ] || die "$binary is not a file"
[ "$(od -An -tx1 -N4 "$binary" | tr -d ' \n')" = 7f454c46 ] ||
	die "$binary is not a Linux executable; build it with GOOS=linux go build ./cmd/golem-deploy"
[ -x "$binary" ] || die "$binary is not executable; run: chmod +x $binary"
[ -f "$UNIT_SRC" ] || die "systemd unit not found at $UNIT_SRC; run the script from a Golem Deploy source checkout"
if ! new_version=$("$binary" version 2>&1) || [ -z "$new_version" ]; then
	die "$binary does not run on this host (is it built for linux/$(dpkg --print-architecture)? GOOS=linux GOARCH=... go build ./cmd/golem-deploy): $new_version"
fi
note "golem-deploy to install: $new_version"

# --- packages ----------------------------------------------------------------

missing=()
# openssh-client: git needs ssh for SSH repository URLs (apt leaves it out as
# a mere recommendation of git with --no-install-recommends).
for pkg in git git-lfs openssh-client ca-certificates; do
	if [ "$(dpkg-query -W -f='${db:Status-Status}' "$pkg" 2>/dev/null || true)" != installed ]; then
		missing+=("$pkg")
	fi
done
if [ "${#missing[@]}" -gt 0 ]; then
	say "Installing ${missing[*]} with apt"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q
	apt-get install -y -q --no-install-recommends "${missing[@]}"
else
	say "git, git-lfs, openssh-client and ca-certificates are installed"
fi

# --- service user ------------------------------------------------------------

if id -u "$SVC_USER" >/dev/null 2>&1; then
	say "Keeping the existing user $SVC_USER"
	home=$(getent passwd "$SVC_USER" | cut -d: -f6)
	[ "$home" = "$DATA_DIR" ] || warn "$SVC_USER has home directory $home instead of $DATA_DIR"
else
	say "Creating the system user and group $SVC_USER"
	nologin=/usr/sbin/nologin
	[ -x "$nologin" ] || nologin=/bin/false
	if getent group "$SVC_USER" >/dev/null; then
		group_opt=(--gid "$SVC_USER")
	else
		group_opt=(--user-group)
	fi
	useradd --system "${group_opt[@]}" --home-dir "$DATA_DIR" --no-create-home \
		--shell "$nologin" --comment "Golem Deploy" "$SVC_USER"
fi
svc_group=$(id -gn "$SVC_USER")

if id -nG "$SVC_USER" | tr ' ' '\n' | grep -x docker >/dev/null; then
	say "$SVC_USER is already a member of the docker group"
else
	say "Adding $SVC_USER to the docker group"
	usermod -aG docker "$SVC_USER"
fi
note "Membership of the docker group is root-equivalent: anyone who controls the"
note "$SVC_USER account (or the panel's admin accounts) can gain root on this host."

if ! runuser -u "$SVC_USER" -- docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
	die "$SVC_USER cannot use Docker even though it is in the docker group;" \
		"check the group of /var/run/docker.sock (ls -l /var/run/docker.sock) and that DOCKER_HOST is not set system-wide"
fi

# --- data directory ----------------------------------------------------------

if [ -e "$DATA_DIR" ] || [ -L "$DATA_DIR" ]; then
	if [ ! -d "$DATA_DIR" ] || [ -L "$DATA_DIR" ]; then
		die "$DATA_DIR exists and is not a directory"
	fi
	owner=$(stat -c %U "$DATA_DIR")
	mode=$(stat -c %a "$DATA_DIR")
	if [ "$owner" != "$SVC_USER" ] || [ "$mode" != 700 ]; then
		die "$DATA_DIR exists with owner $owner and mode $mode; the service needs it owned by $SVC_USER with mode 700." \
			"If it is meant to be the Golem Deploy data directory, run: chown $SVC_USER:$svc_group $DATA_DIR && chmod 700 $DATA_DIR"
	fi
	say "Keeping the existing data directory $DATA_DIR (contents not touched)"
else
	say "Creating the data directory $DATA_DIR (mode 0700, owner $SVC_USER)"
	install -d -m 0700 -o "$SVC_USER" -g "$svc_group" "$DATA_DIR"
fi

# --- binary ------------------------------------------------------------------

if [ -e "$BIN_DEST" ]; then
	old_version=$("$BIN_DEST" version 2>/dev/null || printf 'unknown')
	say "Upgrading $BIN_DEST ($old_version -> $new_version)"
else
	say "Installing $BIN_DEST ($new_version)"
fi
# Copy next to the destination, then rename: a running service keeps the old
# file until it restarts, and an interrupted copy never leaves half a binary.
install -m 0755 -o root -g root "$binary" "$BIN_DEST.new"
mv -f "$BIN_DEST.new" "$BIN_DEST"

# --- settings ----------------------------------------------------------------

install -d -m 0750 -o root -g "$svc_group" "$CONF_DIR"

if [ -n "$tls_cert" ]; then
	say "Installing the TLS certificate and key into $TLS_DIR"
	install -d -m 0750 -o root -g "$svc_group" "$TLS_DIR"
	install -m 0644 -o root -g root "$tls_cert" "$TLS_CERT_DEST.new"
	install -m 0640 -o root -g "$svc_group" "$tls_key" "$TLS_KEY_DEST.new"
	mv -f "$TLS_CERT_DEST.new" "$TLS_CERT_DEST"
	mv -f "$TLS_KEY_DEST.new" "$TLS_KEY_DEST"
fi

if [ -e "$ENV_FILE" ]; then
	say "Keeping the existing settings file $ENV_FILE"
	if [ "${#config_opts[@]}" -gt 0 ]; then
		warn "${config_opts[*]} not applied: the settings file already exists; edit $ENV_FILE and run: systemctl restart $SERVICE"
	fi
	if [ -n "$tls_cert" ] && ! grep -qx "GOLEM_DEPLOY_TLS_CERT=$TLS_CERT_DEST" "$ENV_FILE"; then
		warn "$ENV_FILE does not use the installed certificate; add these lines (and adjust GOLEM_DEPLOY_LISTEN):"
		printf '    GOLEM_DEPLOY_TLS_CERT=%s\n    GOLEM_DEPLOY_TLS_KEY=%s\n' "$TLS_CERT_DEST" "$TLS_KEY_DEST" >&2
	fi
else
	say "Writing $ENV_FILE"
	if [ -n "$tls_cert" ]; then
		tls_lines="GOLEM_DEPLOY_TLS_CERT=$TLS_CERT_DEST
GOLEM_DEPLOY_TLS_KEY=$TLS_KEY_DEST"
	else
		tls_lines="# No TLS: the panel serves plain HTTP and may only listen on loopback.
#GOLEM_DEPLOY_TLS_CERT=$TLS_CERT_DEST
#GOLEM_DEPLOY_TLS_KEY=$TLS_KEY_DEST"
	fi
	tmp_env=$(mktemp "$CONF_DIR/.golem-deploy.env.XXXXXX")
	cat >"$tmp_env" <<EOF
# Golem Deploy settings, read by golem-deploy.service (EnvironmentFile=).
# Every "golem-deploy serve" flag has a GOLEM_DEPLOY_* variable; see the
# "serve options" table in README.md. One KEY=value per line, no quotes.
# The installer never overwrites this file. After editing it, run:
#   systemctl restart golem-deploy
GOLEM_DEPLOY_DATA_DIR=$DATA_DIR
GOLEM_DEPLOY_LISTEN=$listen
GOLEM_DEPLOY_PUBLIC_HOST=$public_host
GOLEM_DEPLOY_PORT_RANGE=$port_range
$tls_lines
EOF
	chown root:"$svc_group" "$tmp_env"
	chmod 0640 "$tmp_env"
	mv "$tmp_env" "$ENV_FILE"
fi

# --- systemd unit ------------------------------------------------------------

say "Installing $UNIT_DEST"
install -m 0644 -o root -g root "$UNIT_SRC" "$UNIT_DEST"
systemctl daemon-reload
systemctl enable "$SERVICE" >/dev/null 2>&1 || die "systemctl enable $SERVICE failed"

say "Restarting $SERVICE"
restart_at=$(date +%s)
systemctl restart "$SERVICE" || true

# Wait until this run of the service logs that it is listening, or fails.
started=''
for _ in $(seq "$START_TIMEOUT"); do
	state=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
	# Restart=on-failure turns a crash into "activating (auto-restart)".
	[ "$(systemctl show -p SubState --value "$SERVICE")" != auto-restart ] || break
	if [ "$state" = active ]; then
		invocation=$(systemctl show -p InvocationID --value "$SERVICE")
		if [ -n "$invocation" ] &&
			journalctl --no-pager -q -o cat "_SYSTEMD_INVOCATION_ID=$invocation" 2>/dev/null | grep 'golem-deploy listening' >/dev/null; then
			started=yes
			break
		fi
	elif [ "$state" != activating ] && [ "$state" != reloading ]; then
		break
	fi
	sleep 1
done
if [ -z "$started" ]; then
	state=$(systemctl is-active "$SERVICE" 2>/dev/null || true)
	if [ "$state" = active ]; then
		warn "$SERVICE is active but did not log \"golem-deploy listening\" within ${START_TIMEOUT}s; check: journalctl -u $SERVICE"
	else
		printf '\n' >&2
		systemctl status --no-pager --lines=0 "$SERVICE" >&2 || true
		printf '\nLog since the restart:\n' >&2
		journalctl --no-pager -u "$SERVICE" --since "@$restart_at" -n 40 >&2 || true
		die "$SERVICE did not start (state: ${state:-unknown}/$(systemctl show -p SubState --value "$SERVICE")); see the log above and docs/INSTALL.md (Troubleshooting)"
	fi
else
	say "$SERVICE is running"
fi

# --- next steps --------------------------------------------------------------

# The listen address in effect comes from the settings file, which may be an
# older, edited one.
eff_listen=$(sed -n 's/^GOLEM_DEPLOY_LISTEN=//p' "$ENV_FILE" | tail -n 1)
eff_cert=$(sed -n 's/^GOLEM_DEPLOY_TLS_CERT=//p' "$ENV_FILE" | tail -n 1)
eff_hosts=$(sed -n 's/^GOLEM_DEPLOY_PUBLIC_HOST=//p' "$ENV_FILE" | tail -n 1)
eff_range=$(sed -n 's/^GOLEM_DEPLOY_PORT_RANGE=//p' "$ENV_FILE" | tail -n 1)
eff_listen=${eff_listen:-$DEFAULT_LISTEN_HTTP}
eff_range=${eff_range:-$DEFAULT_PORT_RANGE}
eff_port=${eff_listen##*:}

cat <<EOF

Golem Deploy $new_version is installed.

1. Create the first admin account, unless you already have one. It prompts for
   the password twice (add --password-stdin to pipe it instead). Run it as
   $SVC_USER so the files it creates stay readable by the service:

     runuser -u $SVC_USER -- $BIN_DEST admin create --data-dir $DATA_DIR --username admin

EOF
if [ -n "$eff_cert" ]; then
	first_host=${eff_hosts%%,*}
	cat <<EOF
2. Open the panel at https://${first_host:-<this host>}:$eff_port/ (allow TCP port
   $eff_port in your firewall). Certificates are not renewed automatically yet:
   after renewing, re-run this script with --tls-cert and --tls-key.

EOF
else
	cat <<EOF
2. The panel listens on $eff_listen with plain HTTP and is reachable from this
   host only. From your computer, open an SSH tunnel and browse to
   http://localhost:$eff_port/ :

     ssh -N -L $eff_port:$eff_listen <user>@<this host>

EOF
fi
cat <<EOF
3. Players connect to game servers on ports $eff_range, UDP and TCP. This script
   does not change firewall rules: allow that range for both UDP and TCP in your
   provider's firewall. Docker publishes the ports itself and bypasses ufw and
   firewalld (see docs/SERVERS.md, "Firewall").

Logs: journalctl -u $SERVICE -f    Settings: $ENV_FILE
EOF
