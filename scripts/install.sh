#!/usr/bin/env bash
# Golem Deploy installer for Ubuntu and Debian hosts with systemd.
#
#   curl -fsSL https://github.com/demiurgos-hub/golem-deploy/releases/latest/download/install.sh | sudo bash
#   sudo ./install.sh [options]
#
# Downloads a release archive for this host's architecture, checks its
# SHA-256 against the release's SHA256SUMS, and installs it as the systemd
# service golem-deploy, running as the system user golem-deploy with its data
# in /var/lib/golem-deploy. It installs Docker Engine from Docker's apt
# repository when Docker is missing, raises the UDP buffer limits for QUIC,
# and never changes firewall rules. Re-running it upgrades the binary and the
# unit and restarts the service; an existing settings file or data directory
# is kept. See docs/INSTALL.md.
#
# The script is usually piped into bash, so everything happens in functions
# and the only command that does work is on the last line: a truncated
# download runs nothing.
set -euo pipefail

# The release this script belongs to. scripts/release.sh sets it to the tag;
# it is empty in the repository, where the latest release is the default.
readonly RELEASE_VERSION=''

readonly REPO_URL=https://github.com/demiurgos-hub/golem-deploy
readonly SERVICE=golem-deploy
readonly SVC_USER=golem-deploy
readonly BIN_DEST=/usr/local/bin/golem-deploy
readonly UNINSTALL_DEST=/usr/local/sbin/golem-deploy-uninstall
readonly DATA_DIR=/var/lib/golem-deploy
readonly CONF_DIR=/etc/golem-deploy
readonly ENV_FILE=$CONF_DIR/golem-deploy.env
readonly TLS_DIR=$CONF_DIR/tls
readonly TLS_CERT_DEST=$TLS_DIR/cert.pem
readonly TLS_KEY_DEST=$TLS_DIR/key.pem
readonly UNIT_DEST=/etc/systemd/system/golem-deploy.service
readonly SYSCTL_FILE=/etc/sysctl.d/60-golem-deploy.conf
readonly DEFAULT_LISTEN_HTTP=127.0.0.1:8000
readonly DEFAULT_LISTEN_TLS=:8443
readonly DEFAULT_PORT_RANGE=7000-7099
readonly DEFAULT_INSPECTOR_PORT=8444
readonly START_TIMEOUT=30
readonly DOCKER_TIMEOUT=60
# The receive and send buffer limit quic-go asks for (7.5 MB).
readonly UDP_BUFFER=7500000
readonly ARCHIVE_MAX_BYTES=268435456
readonly ARCHIVE_MEMBERS='LICENSE golem-deploy golem-deploy.service uninstall.sh'
readonly VERSION_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
# Docker's apt repository signing key (https://download.docker.com/linux/*/gpg).
readonly DOCKER_KEY_FINGERPRINT=9DC858229FC7DD38854AE2D88D81803C0EBFCD88
readonly DOCKER_KEYRING=/etc/apt/keyrings/docker.asc
readonly DOCKER_SOURCES=/etc/apt/sources.list.d/docker.sources
readonly DOCKER_PACKAGES='docker-ce docker-ce-cli containerd.io docker-buildx-plugin'
# Distribution packages that conflict with Docker's (Docker's install pages).
readonly DOCKER_CONFLICTS='docker.io docker-compose docker-compose-v2 docker-doc docker-buildx podman-docker containerd runc'

usage() {
	cat <<EOF
Usage: sudo bash install.sh [options]

Installs Golem Deploy as the systemd service "golem-deploy" on Ubuntu or
Debian. By default it downloads the release archive for this host from
GitHub and checks it against the release's SHA256SUMS.

Where the program comes from (default: download ${RELEASE_VERSION:-the latest release}):
  --version TAG         download release TAG (for example v0.1.0)
  --archive FILE        install from a downloaded release archive (offline);
                        checked against --sha256 or a SHA256SUMS next to it
  --sha256 HEX          expected SHA-256 of the archive
  --binary FILE         install a built binary from a source checkout (the
                        unit and the uninstaller come from the checkout)

Host changes:
  --no-docker           never install Docker Engine or the buildx plugin
  --no-sysctl           leave the UDP buffer limits alone

Settings (they only shape a new $ENV_FILE; an existing one is kept):
  --public-host NAMES   comma-separated host names the panel is reached by
                        (default: this host's public address, if it has one)
  --listen ADDR         panel listen address (default :443 with :80 for HTTP
                        when both ports are free, $DEFAULT_LISTEN_HTTP
                        otherwise, or $DEFAULT_LISTEN_TLS with TLS files)
  --port-range LO-HI    host ports for game servers (default $DEFAULT_PORT_RANGE)
  --tls-cert FILE       TLS certificate (PEM), copied to $TLS_CERT_DEST
  --tls-key FILE        TLS private key (PEM), copied to $TLS_KEY_DEST
  -h, --help            show this help

Environment: GOLEM_DEPLOY_RELEASE_URL replaces the download location with a
directory (https:// or file://) holding one release's files.
EOF
}

say() { printf '==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

need_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		die "$1 needs a value (see --help)"
	fi
}

# Print socket ownership when ss is available. Minimal Linux hosts can
# still identify listening ports (state 0A) through the kernel tables.
tcp_listener() {
	local port=$1 owner=''
	if command -v ss >/dev/null 2>&1; then
		owner=$(ss -H -ltnp "sport = :$port" 2>/dev/null || true)
	fi
	if [ -n "$owner" ]; then
		printf '%s' "$owner"
		return 0
	fi
	awk -v port="$(printf '%04X' "$port")" \
		'$2 ~ (":" port "$") && $4 == "0A" { found=1 } END { exit !found }' /proc/net/tcp /proc/net/tcp6 2>/dev/null
}

pkg_installed() {
	[ "$(dpkg-query -W -f='${db:Status-Status}' "$1" 2>/dev/null || true)" = installed ]
}

docker_server_version() {
	docker version --format '{{.Server.Version}}' 2>/dev/null
}

# Docker's apt repository is configured when any apt source points at it.
docker_repo_configured() {
	grep -rsE '^[^#]*download\.docker\.com/linux' /etc/apt/sources.list /etc/apt/sources.list.d/ >/dev/null
}

# fetch URL DEST MAX_BYTES downloads over HTTPS only (file:// for a local
# GOLEM_DEPLOY_RELEASE_URL), with retries and a size cap.
fetch() {
	local proto='=https'
	case $1 in file://*) proto='=file' ;; esac
	curl --proto "$proto" --proto-redir '=https' --tlsv1.2 -fsSL \
		--retry 3 --retry-delay 2 --retry-connrefused --connect-timeout 20 \
		--max-filesize "$3" -o "$2" "$1"
}

sha256_of() {
	sha256sum <"$1" | awk '{ print tolower($1) }'
}

# The source address the kernel would use for outgoing traffic. "ip route
# get" only looks up the route; it sends nothing.
outgoing_address() {
	local out addr=''
	command -v ip >/dev/null 2>&1 || return 0
	if out=$(ip -4 route get 1.1.1.1 2>/dev/null); then
		addr=$(printf '%s\n' "$out" | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n 1)
	fi
	if [ -z "$addr" ] && out=$(ip -6 route get 2606:4700:4700::1111 2>/dev/null); then
		addr=$(printf '%s\n' "$out" | sed -n 's/.* src \([0-9a-fA-F:]*\).*/\1/p' | head -n 1)
	fi
	printf '%s' "$addr"
}

# is_public_ip ADDR succeeds for a globally routable unicast address: not
# private, shared (CGNAT), link-local, loopback, documentation, benchmark,
# multicast or reserved.
is_public_ip() {
	local a=$1 o1 o2 o3 o4 h1 h2
	if [[ $a =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})$ ]]; then
		o1=$((10#${BASH_REMATCH[1]})) o2=$((10#${BASH_REMATCH[2]}))
		o3=$((10#${BASH_REMATCH[3]})) o4=$((10#${BASH_REMATCH[4]}))
		if [ "$o1" -gt 255 ] || [ "$o2" -gt 255 ] || [ "$o3" -gt 255 ] || [ "$o4" -gt 255 ]; then
			return 1
		fi
		case $o1 in 0 | 10 | 127) return 1 ;; esac
		[ "$o1" -lt 224 ] || return 1
		[ "$o1" -eq 100 ] && [ "$o2" -ge 64 ] && [ "$o2" -le 127 ] && return 1
		[ "$o1" -eq 169 ] && [ "$o2" -eq 254 ] && return 1
		[ "$o1" -eq 172 ] && [ "$o2" -ge 16 ] && [ "$o2" -le 31 ] && return 1
		[ "$o1" -eq 192 ] && [ "$o2" -eq 168 ] && return 1
		[ "$o1" -eq 192 ] && [ "$o2" -eq 0 ] && { [ "$o3" -eq 0 ] || [ "$o3" -eq 2 ]; } && return 1
		[ "$o1" -eq 198 ] && { [ "$o2" -eq 18 ] || [ "$o2" -eq 19 ]; } && return 1
		[ "$o1" -eq 198 ] && [ "$o2" -eq 51 ] && [ "$o3" -eq 100 ] && return 1
		[ "$o1" -eq 203 ] && [ "$o2" -eq 0 ] && [ "$o3" -eq 113 ] && return 1
		return 0
	fi
	a=$(printf '%s' "$a" | tr 'A-F' 'a-f')
	[[ $a =~ ^[0-9a-f:]+$ && $a == *:* ]] || return 1
	h1=${a%%:*}
	h2=${a#*:}
	h2=${h2%%:*}
	[[ $h1 =~ ^[0-9a-f]{1,4}$ ]] || return 1
	[[ $h2 =~ ^[0-9a-f]{0,4}$ ]] || return 1
	h1=$((16#$h1)) h2=$((16#${h2:-0}))
	# Global unicast is 2000::/3; 2001:db8::/32 and 3fff::/20 are for
	# documentation.
	[ $((h1 & 0xe000)) -eq $((0x2000)) ] || return 1
	[ "$h1" -eq $((0x2001)) ] && [ "$h2" -eq $((0xdb8)) ] && return 1
	[ "$h1" -eq $((0x3fff)) ] && [ "$h2" -lt $((0x1000)) ] && return 1
	return 0
}

is_loopback_host() {
	case $1 in
	127.* | ::1 | localhost) return 0 ;;
	*) return 1 ;;
	esac
}

# Split HOST:PORT into split_host and split_port (HOST may be empty or a
# bracketed IPv6 address).
split_hostport() {
	case $1 in
	\[*\]:*)
		split_host=${1%:*}
		split_host=${split_host#[}
		split_host=${split_host%]}
		;;
	*:*) split_host=${1%:*} ;;
	*) return 1 ;;
	esac
	split_port=${1##*:}
	[[ $split_port =~ ^[0-9]{1,5}$ ]] && [ "$((10#$split_port))" -ge 1 ] && [ "$((10#$split_port))" -le 65535 ]
}

env_value() {
	sed -n "s/^$1=//p" "$ENV_FILE" | tail -n 1
}

# extract_archive ARCHIVE DIR unpacks exactly the expected members of a
# release archive into DIR. Anything else (other names, directories, links,
# devices, duplicates, oversized contents) is refused before extraction.
extract_archive() {
	local archive=$1 dir=$2 names listing line total f
	names=$(tar -tzf "$archive" 2>/dev/null) || die "$archive is not a readable .tar.gz archive"
	# shellcheck disable=SC2086 # the member names are fixed words
	if [ "$(printf '%s\n' "$names" | LC_ALL=C sort)" != "$(printf '%s\n' $ARCHIVE_MEMBERS)" ]; then
		die "$archive does not contain exactly the files of a Golem Deploy release ($ARCHIVE_MEMBERS)"
	fi
	listing=$(tar -tvzf "$archive" 2>/dev/null) || die "$archive is not a readable .tar.gz archive"
	while IFS= read -r line; do
		case $line in
		-*) ;;
		*) die "$archive contains something other than plain files: $line" ;;
		esac
	done <<<"$listing"
	total=$(printf '%s\n' "$listing" | awk '{ s += $3 } END { printf "%d", s }')
	[ "$total" -le "$ARCHIVE_MAX_BYTES" ] || die "$archive unpacks to more than $ARCHIVE_MAX_BYTES bytes"
	# shellcheck disable=SC2086 # the member names are fixed words
	tar -xzf "$archive" -C "$dir" --no-same-owner --no-same-permissions -- $ARCHIVE_MEMBERS ||
		die "could not extract $archive"
	for f in $ARCHIVE_MEMBERS; do
		if [ ! -f "$dir/$f" ] || [ -L "$dir/$f" ]; then
			die "$archive did not unpack $f as a plain file"
		fi
	done
}

# check_docker_key FILE stops unless FILE holds exactly one OpenPGP key with
# Docker's documented fingerprint. It needs gpg; apt itself only uses the
# key to check signatures.
check_docker_key() {
	local file=$1 home fprs
	home=$(mktemp -d "$work/gnupg.XXXXXX")
	if ! fprs=$(GNUPGHOME=$home gpg --batch --no-autostart --show-keys --with-colons "$file" 2>/dev/null |
		awk -F: '$1 == "pub" { p = 1; next } $1 == "fpr" && p { print $10; p = 0 }'); then
		die "the downloaded Docker repository key could not be read by gpg"
	fi
	if [ "$fprs" != "$DOCKER_KEY_FINGERPRINT" ]; then
		die "the downloaded Docker repository key does not have the fingerprint Docker documents" \
			"($DOCKER_KEY_FINGERPRINT); got: $(printf '%s' "$fprs" | tr '\n' ' '). Nothing was changed for Docker."
	fi
	note "the key has Docker's fingerprint $DOCKER_KEY_FINGERPRINT"
}

wait_for_docker() {
	local i
	for i in $(seq "$DOCKER_TIMEOUT"); do
		if docker_version=$(docker_server_version) && [ -n "$docker_version" ]; then
			return 0
		fi
		[ "$i" -lt "$DOCKER_TIMEOUT" ] && sleep 1
	done
	die "Docker did not answer within ${DOCKER_TIMEOUT}s; check: systemctl status docker; journalctl -u docker"
}

install_docker() {
	local suite
	export DEBIAN_FRONTEND=noninteractive
	if docker_repo_configured; then
		say "Docker's apt repository is already configured"
	else
		say "Adding Docker's apt repository"
		fetch "https://download.docker.com/linux/$os_id/gpg" "$work/docker.asc" 1048576 ||
			die "could not download Docker's repository key from https://download.docker.com/linux/$os_id/gpg"
		if command -v gpg >/dev/null 2>&1; then
			check_docker_key "$work/docker.asc"
		else
			note "gpg is not installed, so the key's fingerprint is not checked (apt checks the packages against the key)"
		fi
		if [ "$os_id" = ubuntu ]; then
			# shellcheck source=/dev/null
			suite=$(. /etc/os-release && printf '%s' "${UBUNTU_CODENAME:-${VERSION_CODENAME:-}}")
		else
			# shellcheck source=/dev/null
			suite=$(. /etc/os-release && printf '%s' "${VERSION_CODENAME:-}")
		fi
		[[ $suite =~ ^[a-z]+$ ]] || die "cannot tell this release's code name from /etc/os-release; install Docker Engine by hand: $docker_hint_url"
		install -d -m 0755 -o root -g root /etc/apt/keyrings
		install -m 0644 -o root -g root "$work/docker.asc" "$DOCKER_KEYRING"
		cat >"$work/docker.sources" <<EOF
# Docker's apt repository, added by the Golem Deploy installer.
Types: deb
URIs: https://download.docker.com/linux/$os_id
Suites: $suite
Components: stable
Architectures: $arch
Signed-By: $DOCKER_KEYRING
EOF
		install -m 0644 -o root -g root "$work/docker.sources" "$DOCKER_SOURCES"
		note "wrote $DOCKER_KEYRING and $DOCKER_SOURCES"
	fi
	say "Installing Docker Engine ($DOCKER_PACKAGES)"
	apt-get update -q </dev/null || die "apt-get update failed; see the output above"
	# shellcheck disable=SC2086 # fixed package names
	apt-get install -y -q $DOCKER_PACKAGES </dev/null || die "installing Docker Engine failed; see the output above"
}

start_docker() {
	say "Enabling and starting Docker"
	systemctl enable --now docker.service >/dev/null 2>&1 || die "systemctl enable --now docker failed; check: systemctl status docker"
	wait_for_docker
	note "Docker Engine: $docker_version"
}

install_buildx() {
	say "Installing the Docker buildx plugin (docker-buildx-plugin)"
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -q </dev/null || die "apt-get update failed; see the output above"
	apt-get install -y -q docker-buildx-plugin </dev/null || die "installing docker-buildx-plugin failed; see the output above"
	docker buildx version >/dev/null 2>&1 || die "docker buildx still does not work after installing docker-buildx-plugin"
}

apply_sysctl() {
	local key cur want lines=''
	for key in net.core.rmem_max net.core.wmem_max; do
		cur=$(cat "/proc/sys/${key//.//}" 2>/dev/null || true)
		[[ $cur =~ ^[0-9]+$ ]] || cur=0
		want=$UDP_BUFFER
		[ "$cur" -le "$want" ] || want=$cur
		lines+="$key = $want"$'\n'
	done
	say "Raising the UDP buffer limits for QUIC in $SYSCTL_FILE"
	cat >"$work/sysctl.conf" <<EOF
# Written by the Golem Deploy installer; removed by golem-deploy-uninstall.
# Games use QUIC (WebTransport), which works better with UDP buffers larger
# than the kernel's default. $UDP_BUFFER bytes is what quic-go recommends;
# a value that was already higher is kept.
$lines
EOF
	install -m 0644 -o root -g root "$work/sysctl.conf" "$SYSCTL_FILE.new"
	mv -f "$SYSCTL_FILE.new" "$SYSCTL_FILE"
	printf '%s' "$lines" | sed 's/^/    /'
	local f
	if command -v sysctl >/dev/null 2>&1; then
		sysctl -q -p "$SYSCTL_FILE" >/dev/null 2>&1 || true
	else
		for f in /usr/lib/systemd/systemd-sysctl /lib/systemd/systemd-sysctl; do
			if [ -x "$f" ]; then
				"$f" "$SYSCTL_FILE" >/dev/null 2>&1 || true
				break
			fi
		done
	fi
	# sysctl exits 0 even when the kernel refuses a value; read them back.
	for key in net.core.rmem_max net.core.wmem_max; do
		cur=$(cat "/proc/sys/${key//.//}" 2>/dev/null || true)
		if [[ ! $cur =~ ^[0-9]+$ ]] || [ "$cur" -lt "$UDP_BUFFER" ]; then
			warn "could not apply $SYSCTL_FILE now (a container may not allow it); it applies at the next boot"
			break
		fi
	done
}

# panel_origin sets origin (scheme://host[:port]) from the effective
# settings file, plus origin_loopback, origin_managed_tls and
# origin_host_source. origin stays empty when no host is known.
panel_origin() {
	local scheme host port hosts first
	origin='' origin_loopback='' origin_managed_tls='' origin_host_source='' origin_port=''
	split_hostport "$eff_listen" || return 0
	port=$((10#$split_port))
	origin_port=$port
	if is_loopback_host "$split_host"; then
		origin_loopback=yes
	fi
	if [ -n "$eff_cert" ]; then
		scheme=https
	elif [ -n "$origin_loopback" ] || [ "$eff_insecure" = true ] || [ "$eff_insecure" = 1 ]; then
		scheme=http
	else
		scheme=https
		origin_managed_tls=yes
	fi
	hosts=$eff_hosts
	first=${hosts%%,*}
	if [ -n "$origin_loopback" ]; then
		host=localhost
		origin_host_source=tunnel
	elif [ -n "$first" ]; then
		host=$first
		origin_host_source=setting
	elif [ -n "$detected_addr" ]; then
		host=$detected_addr
		origin_host_source=detected
	elif [ -n "$split_host" ] && [ "$split_host" != 0.0.0.0 ] && [ "$split_host" != :: ]; then
		host=$split_host
		origin_host_source=listen
	else
		return 0
	fi
	[[ $host =~ ^[A-Za-z0-9.:-]+$ ]] || return 0
	case $host in *:*) host="[$host]" ;; esac
	origin=$scheme://$host
	if { [ "$scheme" = https ] && [ "$port" -ne 443 ]; } || { [ "$scheme" = http ] && [ "$port" -ne 80 ]; }; then
		origin=$origin:$port
	fi
}

# The panel's self-signed certificate fingerprint from this start's log, in
# the colon-separated form browsers show.
panel_fingerprint() {
	local invocation fp
	invocation=$(systemctl show -p InvocationID --value "$SERVICE" 2>/dev/null || true)
	[ -n "$invocation" ] || return 0
	fp=$(journalctl --no-pager -q -o cat "_SYSTEMD_INVOCATION_ID=$invocation" 2>/dev/null |
		sed -n 's/.*msg="panel self-signed TLS fingerprint" sha256=\([0-9a-f]\{64\}\).*/\1/p' | tail -n 1)
	[ -n "$fp" ] || return 0
	printf '%s' "$fp" | tr 'a-f' 'A-F' | sed 's/../&:/g; s/:$//'
}

main() {
	local public_host='' listen='' http_listen='' port_range='' tls_cert='' tls_key=''
	local version='' binary='' archive='' sha256='' no_docker='' no_sysctl=''
	local config_opts=() opt val

	while [ "$#" -gt 0 ]; do
		case $1 in
		-h | --help)
			usage
			exit 0
			;;
		--no-docker)
			no_docker=yes
			shift
			continue
			;;
		--no-sysctl)
			no_sysctl=yes
			shift
			continue
			;;
		--public-host | --listen | --port-range | --tls-cert | --tls-key | --version | --binary | --archive | --sha256)
			need_value "$@"
			opt=$1 val=$2
			shift 2
			;;
		--public-host=* | --listen=* | --port-range=* | --tls-cert=* | --tls-key=* | --version=* | --binary=* | --archive=* | --sha256=*)
			opt=${1%%=*} val=${1#*=}
			shift
			[ -n "$val" ] || die "$opt needs a value (see --help)"
			;;
		-*)
			die "unknown option $1 (see --help)"
			;;
		*)
			die "unexpected argument $1; give a built binary with --binary FILE (see --help)"
			;;
		esac
		case $opt in
		--public-host) public_host=$val ;;
		--listen) listen=$val ;;
		--port-range) port_range=$val ;;
		--tls-cert) tls_cert=$val ;;
		--tls-key) tls_key=$val ;;
		--version) version=$val ;;
		--binary) binary=$val ;;
		--archive) archive=$val ;;
		--sha256) sha256=$val ;;
		esac
		case $opt in
		--public-host | --listen | --port-range) config_opts+=("$opt") ;;
		esac
	done

	# --- validation of the options -------------------------------------------

	local from=download
	if [ -n "$binary" ] && [ -n "$archive" ]; then
		die "--binary and --archive cannot be used together"
	fi
	[ -z "$binary" ] || from=binary
	[ -z "$archive" ] || from=archive
	if [ "$from" != download ] && [ -n "$version" ]; then
		die "--version only applies to downloads, not to --$from"
	fi
	if [ "$from" = binary ] && [ -n "$sha256" ]; then
		die "--sha256 applies to release archives, not to --binary"
	fi
	if [ -n "$sha256" ]; then
		sha256=$(printf '%s' "$sha256" | tr 'A-F' 'a-f')
		[[ $sha256 =~ ^[0-9a-f]{64}$ ]] || die "--sha256 must be 64 hexadecimal characters"
	fi
	[ -n "$version" ] || version=$RELEASE_VERSION
	if [ -n "$version" ] && [[ ! $version =~ $VERSION_RE ]]; then
		die "--version must be a release tag such as v0.1.0 or v0.2.0-rc.1 (got $version)"
	fi
	local release_url=${GOLEM_DEPLOY_RELEASE_URL:-}
	if [ "$from" = download ] && [ -n "$release_url" ]; then
		[[ $release_url =~ ^(https|file)://[A-Za-z0-9._~:/@%+=-]+$ ]] ||
			die "GOLEM_DEPLOY_RELEASE_URL must be an https:// or file:// URL of a directory (got $release_url)"
		release_url=${release_url%/}
	fi

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
		if [ -n "$tls_cert" ]; then
			listen=$DEFAULT_LISTEN_TLS
		elif [ ! -e "$ENV_FILE" ]; then
			listen=:443
			http_listen=:80
		else
			listen=$DEFAULT_LISTEN_HTTP
		fi
	fi
	[ -n "$port_range" ] || port_range=$DEFAULT_PORT_RANGE

	local split_host split_port listen_host listen_port
	split_hostport "$listen" ||
		die "--listen must be HOST:PORT with a port between 1 and 65535, for example $DEFAULT_LISTEN_HTTP (got $listen)"
	listen_host=$split_host listen_port=$((10#$split_port))
	[[ $listen_host =~ ^[A-Za-z0-9.:-]*$ ]] || die "--listen host contains unexpected characters (got $listen)"

	[[ $port_range =~ ^([0-9]{1,5})-([0-9]{1,5})$ ]] || die "--port-range must be LO-HI, for example $DEFAULT_PORT_RANGE (got $port_range)"
	local port_lo=$((10#${BASH_REMATCH[1]})) port_hi=$((10#${BASH_REMATCH[2]}))
	if [ "$port_lo" -lt 1024 ] || [ "$port_hi" -gt 32767 ] || [ "$port_lo" -gt "$port_hi" ]; then
		die "--port-range must lie within 1024-32767 with LO <= HI (got $port_range)"
	fi
	if [ "$listen_port" -ge "$port_lo" ] && [ "$listen_port" -le "$port_hi" ]; then
		die "--port-range $port_range contains the panel's listen port $listen_port"
	fi

	if [ -n "$public_host" ]; then
		[[ $public_host =~ ^[A-Za-z0-9.,:-]+$ ]] || die "--public-host takes comma-separated DNS names or IP addresses (got $public_host)"
	fi

	# --- host checks ---------------------------------------------------------

	say "Checking the host"

	[ "$(id -u)" -eq 0 ] || die "run this script as root (for example with sudo)"

	[ -r /etc/os-release ] || die "cannot read /etc/os-release; only Ubuntu and Debian are supported"
	# shellcheck source=/dev/null
	os_id=$(. /etc/os-release && printf '%s' "${ID:-}")
	local os_name
	# shellcheck source=/dev/null
	os_name=$(. /etc/os-release && printf '%s' "${PRETTY_NAME:-$os_id}")
	case $os_id in
	ubuntu | debian) note "operating system: $os_name" ;;
	*) die "unsupported operating system \"$os_name\"; this installer supports Ubuntu and Debian only" ;;
	esac

	arch=$(dpkg --print-architecture 2>/dev/null || true)
	case $arch in
	amd64 | arm64) note "architecture: $arch" ;;
	*) die "unsupported architecture \"$arch\"; releases exist for amd64 and arm64" ;;
	esac

	if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
		die "systemd is not running as the init system; the installer needs systemd to run the service"
	fi
	note "systemd: $(systemctl --version | head -n 1)"

	if [ -n "$http_listen" ]; then
		local busy='' port owner
		for port in 443 80; do
			if owner=$(tcp_listener "$port"); then
				warn "TCP port $port is already in use${owner:+ ($owner)}"
				busy=yes
			fi
		done
		if [ -n "$busy" ]; then
			listen=''
			http_listen=''
			for port in {8000..8099}; do
				if [ "$port" -ge "$port_lo" ] && [ "$port" -le "$port_hi" ]; then continue; fi
				if ! tcp_listener "$port" >/dev/null; then
					listen=127.0.0.1:$port
					break
				fi
			done
			[ -n "$listen" ] || die "no free fallback loopback port in 8000-8099; set --listen explicitly"
			warn "Using $listen. Automatic certificates need TCP port 80 or 443 reachable from the internet."
		fi
	fi

	# Docker: decide what to do without changing anything yet.
	docker_hint_url=https://docs.docker.com/engine/install/$os_id/
	local docker_hint="install Docker Engine following $docker_hint_url and run this script again"
	local docker_plan='' buildx_plan='' docker_path conflicts=() pkg
	docker_version=''
	for pkg in $DOCKER_CONFLICTS; do
		if pkg_installed "$pkg"; then conflicts+=("$pkg"); fi
	done
	local conflict_msg="packages that conflict with Docker's are installed (${conflicts[*]}) and Docker does not work."
	conflict_msg+=" This script never removes packages. Remove them (images, containers and volumes in /var/lib/docker are kept) and run it again:"
	conflict_msg+=$'\n'"    apt-get remove ${conflicts[*]}"
	if docker_path=$(command -v docker 2>/dev/null); then
		# /snap/bin/docker is a link to /usr/bin/snap.
		case $docker_path:$(readlink -f "$docker_path") in
		/snap/* | *:/snap/* | *:/usr/bin/snap) die "Docker from a snap is not supported; remove it (snap remove docker) and run this script again, or $docker_hint" ;;
		esac
		if docker_version=$(docker_server_version) && [ -n "$docker_version" ]; then
			note "Docker Engine: $docker_version"
		elif [ "${#conflicts[@]}" -gt 0 ]; then
			die "$conflict_msg"
		elif [ -z "$no_docker" ] && pkg_installed docker-ce && systemctl cat docker.service >/dev/null 2>&1; then
			docker_plan=start
			note "Docker Engine is installed but not running; it will be started"
		else
			die "the Docker daemon is not reachable (is it running? try: systemctl status docker); if Docker is not installed, $docker_hint"
		fi
	else
		if [ -n "$no_docker" ]; then
			die "the docker command was not found and --no-docker was given; $docker_hint"
		fi
		[ "${#conflicts[@]}" -eq 0 ] || die "$conflict_msg"
		docker_plan=install
		note "Docker Engine is not installed; it will be installed from Docker's apt repository"
	fi
	if [ -z "$docker_plan" ]; then
		case $(docker info --format '{{join .SecurityOptions " "}}' 2>/dev/null || true) in
		*name=rootless*) die "rootless Docker is not supported; $docker_hint" ;;
		esac
		getent group docker >/dev/null ||
			die "the docker group does not exist; Docker Engine installed from Docker's packages creates it (a snap or rootless Docker is not supported): $docker_hint"
		if ! docker buildx version >/dev/null 2>&1; then
			if [ -z "$no_docker" ] && pkg_installed docker-ce && docker_repo_configured; then
				buildx_plan=install
				note "the Docker buildx plugin is missing; docker-buildx-plugin will be installed"
			else
				die "the Docker buildx plugin is missing (docker buildx version fails) and builds need it." \
					"With Docker's packages run: apt-get install docker-buildx-plugin. With the distribution's" \
					"docker.io, install its docker-buildx package where the distribution has one."
			fi
		fi
	fi

	if [ "$from" = download ] && ! command -v curl >/dev/null 2>&1; then
		die "curl is needed to download the release; install it (apt-get install curl) or use --archive"
	fi

	# --- the program to install ----------------------------------------------

	# Everything is unpacked or copied into a private directory first and
	# installed from there.
	work=$(mktemp -d)
	trap 'rm -rf -- "$work"' EXIT
	local payload=$work/payload
	mkdir -m 0700 "$payload"

	case $from in
	download)
		local asset=golem-deploy_linux_$arch.tar.gz base
		if [ -n "$release_url" ]; then
			base=$release_url
		elif [ -n "$version" ]; then
			base=$REPO_URL/releases/download/$version
		else
			base=$REPO_URL/releases/latest/download
		fi
		say "Downloading ${version:-the latest release} for linux/$arch from $base"
		fetch "$base/$asset" "$work/$asset" "$ARCHIVE_MAX_BYTES" || die "could not download $base/$asset; nothing was changed"
		local want got
		if [ -n "$sha256" ]; then
			want=$sha256
			note "checking the archive against --sha256"
		else
			fetch "$base/SHA256SUMS" "$work/SHA256SUMS" 1048576 || die "could not download $base/SHA256SUMS; nothing was changed"
			want=$(awk -v n="$asset" 'NF == 2 && ($2 == n || $2 == "*" n) { print tolower($1) }' "$work/SHA256SUMS")
			[[ $want =~ ^[0-9a-f]{64}$ ]] || die "SHA256SUMS has no single entry for $asset; nothing was changed"
		fi
		got=$(sha256_of "$work/$asset")
		[ "$got" = "$want" ] || die "the SHA-256 of $asset is $got, but $want was expected; nothing was changed"
		note "SHA-256 $got matches"
		extract_archive "$work/$asset" "$payload"
		;;
	archive)
		[ -f "$archive" ] || die "$archive is not a file"
		local sums got want=''
		sums=$(dirname -- "$archive")/SHA256SUMS
		got=$(sha256_of "$archive")
		if [ -n "$sha256" ]; then
			want=$sha256
		elif [ -f "$sums" ]; then
			want=$(awk -v n="$(basename -- "$archive")" 'NF == 2 && ($2 == n || $2 == "*" n) { print tolower($1) }' "$sums")
			[[ $want =~ ^[0-9a-f]{64}$ ]] || die "$sums has no single entry for $(basename -- "$archive"); pass --sha256"
		fi
		if [ -n "$want" ]; then
			[ "$got" = "$want" ] || die "the SHA-256 of $archive is $got, but $want was expected; nothing was changed"
			say "Using $archive (SHA-256 $got matches)"
		else
			warn "$archive is not verified: no --sha256 given and no SHA256SUMS next to it (SHA-256 $got)"
			say "Using $archive"
		fi
		extract_archive "$archive" "$payload"
		;;
	binary)
		local src=${BASH_SOURCE[0]:-} script_dir
		[ -f "$src" ] || die "--binary needs the script run from a Golem Deploy source checkout (sudo scripts/install.sh --binary FILE)"
		script_dir=$(CDPATH='' cd -- "$(dirname -- "$src")" && pwd)
		[ -f "$binary" ] || die "$binary is not a file"
		[ -f "$script_dir/../deploy/golem-deploy.service" ] ||
			die "systemd unit not found at $script_dir/../deploy/golem-deploy.service; run the script from a Golem Deploy source checkout"
		[ -f "$script_dir/uninstall.sh" ] || die "uninstaller not found at $script_dir/uninstall.sh"
		cp -- "$binary" "$payload/golem-deploy"
		cp -- "$script_dir/../deploy/golem-deploy.service" "$payload/golem-deploy.service"
		cp -- "$script_dir/uninstall.sh" "$payload/uninstall.sh"
		say "Installing $binary from a source checkout"
		;;
	esac

	# Modes in an archive (or a Windows checkout) are not trusted; set them.
	chmod 0700 "$payload/golem-deploy"
	[ "$(od -An -tx1 -N4 "$payload/golem-deploy" | tr -d ' \n')" = 7f454c46 ] ||
		die "the golem-deploy binary is not a Linux executable; build it with GOOS=linux go build ./cmd/golem-deploy"
	grep -qx 'ExecStart=/usr/local/bin/golem-deploy serve' "$payload/golem-deploy.service" ||
		die "golem-deploy.service does not look like Golem Deploy's systemd unit"
	[ "$(head -n 1 "$payload/uninstall.sh")" = '#!/usr/bin/env bash' ] ||
		die "uninstall.sh does not look like Golem Deploy's uninstaller"
	local new_version
	if ! new_version=$("$payload/golem-deploy" version 2>&1) || [ -z "$new_version" ]; then
		die "the golem-deploy binary does not run on this host (built for another architecture than linux/$arch," \
			"or the temporary directory $work is mounted noexec; set TMPDIR to another directory): $new_version"
	fi
	if [ "$from" = download ] && [ -n "$version" ] && [ "$new_version" != "$version" ]; then
		die "the downloaded binary reports version $new_version, not $version; nothing was changed"
	fi
	note "golem-deploy to install: $new_version"

	# --- packages ------------------------------------------------------------

	local missing=()
	# openssh-client: git needs ssh for SSH repository URLs (apt leaves it out as
	# a mere recommendation of git with --no-install-recommends).
	for pkg in git git-lfs openssh-client ca-certificates; do
		pkg_installed "$pkg" || missing+=("$pkg")
	done
	if [ "$docker_plan" = install ] && ! command -v curl >/dev/null 2>&1; then
		missing+=(curl)
	fi
	if [ "${#missing[@]}" -gt 0 ]; then
		say "Installing ${missing[*]} with apt"
		export DEBIAN_FRONTEND=noninteractive
		apt-get update -q </dev/null || die "apt-get update failed; see the output above"
		apt-get install -y -q --no-install-recommends "${missing[@]}" </dev/null || die "apt-get install failed; see the output above"
	else
		say "git, git-lfs, openssh-client and ca-certificates are installed"
	fi

	# --- Docker --------------------------------------------------------------

	case $docker_plan in
	install)
		install_docker
		start_docker
		;;
	start) start_docker ;;
	*) say "Keeping the installed Docker Engine $docker_version" ;;
	esac
	if [ -n "$docker_plan" ]; then
		getent group docker >/dev/null || die "the docker group does not exist after installing Docker; $docker_hint"
		if ! docker buildx version >/dev/null 2>&1; then
			if docker_repo_configured; then buildx_plan=install; else die "the Docker buildx plugin is missing; run: apt-get install docker-buildx-plugin"; fi
		fi
	fi
	[ "$buildx_plan" != install ] || install_buildx

	# --- kernel settings -----------------------------------------------------

	if [ -n "$no_sysctl" ]; then
		say "Leaving the UDP buffer limits alone (--no-sysctl)"
	else
		apply_sysctl
	fi

	# --- service user --------------------------------------------------------

	if id -u "$SVC_USER" >/dev/null 2>&1; then
		say "Keeping the existing user $SVC_USER"
		local home
		home=$(getent passwd "$SVC_USER" | cut -d: -f6)
		[ "$home" = "$DATA_DIR" ] || warn "$SVC_USER has home directory $home instead of $DATA_DIR"
	else
		say "Creating the system user and group $SVC_USER"
		local nologin=/usr/sbin/nologin group_opt
		[ -x "$nologin" ] || nologin=/bin/false
		if getent group "$SVC_USER" >/dev/null; then
			group_opt=(--gid "$SVC_USER")
		else
			group_opt=(--user-group)
		fi
		useradd --system "${group_opt[@]}" --home-dir "$DATA_DIR" --no-create-home \
			--shell "$nologin" --comment "Golem Deploy" "$SVC_USER"
	fi
	local svc_group
	svc_group=$(id -gn "$SVC_USER")

	if id -nG "$SVC_USER" | tr ' ' '\n' | grep -x docker >/dev/null; then
		say "$SVC_USER is already a member of the docker group"
	else
		say "Adding $SVC_USER to the docker group"
		usermod -aG docker "$SVC_USER"
	fi
	note "Membership of the docker group is root-equivalent: anyone who controls the"
	note "$SVC_USER account (or the panel's admin accounts) can gain root on this host."

	if ! runuser -u "$SVC_USER" -- docker version --format '{{.Server.Version}}' >/dev/null 2>&1 </dev/null; then
		die "$SVC_USER cannot use Docker even though it is in the docker group;" \
			"check the group of /var/run/docker.sock (ls -l /var/run/docker.sock) and that DOCKER_HOST is not set system-wide"
	fi

	# --- data directory ------------------------------------------------------

	if [ -e "$DATA_DIR" ] || [ -L "$DATA_DIR" ]; then
		if [ ! -d "$DATA_DIR" ] || [ -L "$DATA_DIR" ]; then
			die "$DATA_DIR exists and is not a directory"
		fi
		local owner mode
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

	# --- binary and uninstaller ----------------------------------------------

	if [ -e "$BIN_DEST" ]; then
		local old_version
		old_version=$("$BIN_DEST" version 2>/dev/null || printf 'unknown')
		say "Upgrading $BIN_DEST ($old_version -> $new_version)"
	else
		say "Installing $BIN_DEST ($new_version)"
	fi
	# Copy next to the destination, then rename: a running service keeps the old
	# file until it restarts, and an interrupted copy never leaves half a binary.
	install -m 0755 -o root -g root "$payload/golem-deploy" "$BIN_DEST.new"
	mv -f "$BIN_DEST.new" "$BIN_DEST"

	say "Installing the uninstaller as $UNINSTALL_DEST"
	install -d -m 0755 -o root -g root "$(dirname "$UNINSTALL_DEST")"
	install -m 0755 -o root -g root "$payload/uninstall.sh" "$UNINSTALL_DEST.new"
	mv -f "$UNINSTALL_DEST.new" "$UNINSTALL_DEST"

	# --- settings ------------------------------------------------------------

	install -d -m 0750 -o root -g "$svc_group" "$CONF_DIR"

	if [ -n "$tls_cert" ]; then
		say "Installing the TLS certificate and key into $TLS_DIR"
		install -d -m 0750 -o root -g "$svc_group" "$TLS_DIR"
		install -m 0644 -o root -g root "$tls_cert" "$TLS_CERT_DEST.new"
		install -m 0640 -o root -g "$svc_group" "$tls_key" "$TLS_KEY_DEST.new"
		mv -f "$TLS_CERT_DEST.new" "$TLS_CERT_DEST"
		mv -f "$TLS_KEY_DEST.new" "$TLS_KEY_DEST"
	fi

	detected_addr=''
	local detected_public=''
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
		if [ -z "$public_host" ]; then
			detected_addr=$(outgoing_address)
			if [ -n "$detected_addr" ] && is_public_ip "$detected_addr"; then
				detected_public=yes
				public_host=$detected_addr
				say "Using this host's public address $detected_addr as GOLEM_DEPLOY_PUBLIC_HOST"
				note "Players and the panel use it. To use a DNS name instead, edit $ENV_FILE later."
			elif [ -n "$detected_addr" ]; then
				warn "this host's address $detected_addr is private (behind NAT), so GOLEM_DEPLOY_PUBLIC_HOST stays empty." \
					"Pass --public-host with the public name or address on a new installation," \
					"or set the game host in the panel's settings."
			else
				warn "could not find this host's address; GOLEM_DEPLOY_PUBLIC_HOST stays empty." \
					"Set the game host in the panel's settings, or GOLEM_DEPLOY_PUBLIC_HOST in $ENV_FILE."
			fi
		fi
		say "Writing $ENV_FILE"
		local tls_lines
		if [ -n "$tls_cert" ]; then
			tls_lines="GOLEM_DEPLOY_TLS_CERT=$TLS_CERT_DEST
GOLEM_DEPLOY_TLS_KEY=$TLS_KEY_DEST"
		else
			tls_lines="# Optional operator TLS files. Non-loopback listeners otherwise use managed TLS.
#GOLEM_DEPLOY_TLS_CERT=$TLS_CERT_DEST
#GOLEM_DEPLOY_TLS_KEY=$TLS_KEY_DEST"
		fi
		local tmp_env
		tmp_env=$(mktemp "$CONF_DIR/.golem-deploy.env.XXXXXX")
		cat >"$tmp_env" <<EOF
# Golem Deploy settings, read by golem-deploy.service (EnvironmentFile=).
# Every "golem-deploy serve" flag has a GOLEM_DEPLOY_* variable; see the
# "serve options" table in README.md. One KEY=value per line, no quotes.
# The installer never overwrites this file. After editing it, run:
#   systemctl restart golem-deploy
GOLEM_DEPLOY_DATA_DIR=$DATA_DIR
GOLEM_DEPLOY_LISTEN=$listen
GOLEM_DEPLOY_HTTP_LISTEN=$http_listen
GOLEM_DEPLOY_PUBLIC_HOST=$public_host
GOLEM_DEPLOY_PORT_RANGE=$port_range
$tls_lines
EOF
		chown root:"$svc_group" "$tmp_env"
		chmod 0640 "$tmp_env"
		mv "$tmp_env" "$ENV_FILE"
	fi

	# --- systemd unit --------------------------------------------------------

	say "Installing $UNIT_DEST"
	install -m 0644 -o root -g root "$payload/golem-deploy.service" "$UNIT_DEST"
	systemctl daemon-reload
	systemctl enable "$SERVICE" >/dev/null 2>&1 || die "systemctl enable $SERVICE failed"

	say "Restarting $SERVICE"
	local restart_at state started='' invocation
	restart_at=$(date +%s)
	# Clear the unit's start limit (5 starts in 5 minutes), which repeated
	# runs of this script would otherwise hit.
	systemctl reset-failed "$SERVICE" >/dev/null 2>&1 || true
	systemctl restart "$SERVICE" || true

	# Wait until this run of the service logs that it is listening, or fails.
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

	# --- next steps ----------------------------------------------------------

	# The settings in effect come from the settings file, which may be an
	# older, edited one.
	eff_listen=$(env_value GOLEM_DEPLOY_LISTEN)
	eff_http=$(env_value GOLEM_DEPLOY_HTTP_LISTEN)
	eff_cert=$(env_value GOLEM_DEPLOY_TLS_CERT)
	eff_insecure=$(env_value GOLEM_DEPLOY_INSECURE_HTTP)
	eff_hosts=$(env_value GOLEM_DEPLOY_PUBLIC_HOST)
	local eff_range eff_inspector
	eff_range=$(env_value GOLEM_DEPLOY_PORT_RANGE)
	eff_inspector=$(env_value GOLEM_DEPLOY_INSPECTOR_LISTEN)
	eff_listen=${eff_listen:-$DEFAULT_LISTEN_HTTP}
	eff_range=${eff_range:-$DEFAULT_PORT_RANGE}
	if [ -z "$eff_hosts" ] && [ -z "$detected_addr" ]; then
		detected_addr=$(outgoing_address)
	fi
	panel_origin

	local fingerprint=''
	if [ -n "$origin_managed_tls" ]; then
		fingerprint=$(panel_fingerprint)
	fi

	# The setup link is a secret: it is held in a variable and printed, never
	# written to a file or logged.
	local link='' link_rc=0 link_err=$work/setup-link.err
	if [ -n "$origin" ]; then
		set +e
		link=$(cd / && runuser -u "$SVC_USER" -- "$BIN_DEST" admin setup-link \
			--data-dir "$DATA_DIR" --origin "$origin" --ttl 1h 2>"$link_err" </dev/null)
		link_rc=$?
		set -e
		if [ "$link_rc" -eq 0 ]; then
			local token=${link#"$origin/setup#t="}
			if [ "$token" = "$link" ] || [[ ! $token =~ ^[A-Za-z0-9._~-]+$ ]]; then
				warn "golem-deploy admin setup-link printed something other than a setup link; it is not shown"
				link='' link_rc=1
			fi
		elif [ "$link_rc" -eq 1 ]; then
			warn "golem-deploy admin setup-link failed:"
			sed 's/^/    /' "$link_err" >&2 || true
		fi
		[ "$link_rc" -eq 0 ] || link=''
	else
		link_rc=1
	fi

	printf '\nGolem Deploy %s is installed.\n\n' "$new_version"

	local step=1
	if [ -n "$origin_loopback" ]; then
		cat <<EOF
$step. The panel listens on $eff_listen and is reachable from this host only.
   From your computer, open an SSH tunnel and keep it open:

     ssh -N -L $origin_port:$eff_listen <user>@<this host>

EOF
		step=$((step + 1))
	fi
	local link_cmd="sudo runuser -u $SVC_USER -- $BIN_DEST admin setup-link --data-dir $DATA_DIR --origin ${origin:-<panel address>}"
	if [ "$link_rc" -eq 0 ]; then
		cat <<EOF
$step. Create the first admin account: open this link in your browser.
   It works once and for one hour.

     $link

EOF
		if [ -s "$link_err" ]; then
			sed 's/^/   /' "$link_err"
			printf '\n'
		fi
		cat <<EOF
   To make a new link:

     $link_cmd

EOF
	elif [ "$link_rc" -eq 3 ]; then
		cat <<EOF
$step. An admin account already exists. Open the panel at $origin/

EOF
	else
		cat <<EOF
$step. Create the first admin account, unless you already have one. It prompts for
   the password twice (add --password-stdin to pipe it instead). Run it as
   $SVC_USER so the files it creates stay readable by the service:

     sudo runuser -u $SVC_USER -- $BIN_DEST admin create --data-dir $DATA_DIR --username admin

   Then open the panel at ${origin:-https://<this host>:${eff_listen##*:}}/

EOF
	fi
	if [ -n "$fingerprint" ]; then
		cat <<EOF
   The panel uses a self-signed certificate until you set up an automatic one
   in its settings, so the browser warns about it. Continue only if the
   certificate's SHA-256 fingerprint is:

     $fingerprint

EOF
	elif [ -n "$origin_managed_tls" ]; then
		cat <<EOF
   The panel uses a self-signed certificate until you set up an automatic one.
   Compare the fingerprint in journalctl -u $SERVICE ("panel self-signed TLS
   fingerprint") with the one the browser shows.

EOF
	fi
	if [ "$origin_host_source" = detected ] && ! is_public_ip "$detected_addr"; then
		cat <<EOF
   $detected_addr is this host's private address. If you reach the host by
   another address or name, use that instead, and set the game host in the
   panel's settings so players can connect.

EOF
	fi
	step=$((step + 1))

	if [ -z "$origin_loopback" ] && [ -n "$origin_port" ]; then
		local panel_ports=$origin_port
		if [ -n "$eff_http" ] && split_hostport "$eff_http"; then panel_ports="$panel_ports $((10#$split_port))"; fi
		cat <<EOF
$step. Allow the panel's TCP port(s) $panel_ports in your provider's firewall. This
   script does not change firewall rules.
EOF
		if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | head -n 1 | grep -x 'Status: active' >/dev/null; then
			printf '   ufw is active on this host; allow them with:\n\n'
			for port in $panel_ports; do printf '     ufw allow %s/tcp\n' "$port"; done
			if [ "$eff_inspector" != off ]; then
				local insp_port=$DEFAULT_INSPECTOR_PORT
				if [ -n "$eff_inspector" ] && split_hostport "$eff_inspector"; then insp_port=$((10#$split_port)); fi
				printf '     ufw allow %s/tcp      # only when you turn the Inspector on\n' "$insp_port"
			fi
		fi
		printf '\n'
		step=$((step + 1))
	fi

	cat <<EOF
$step. Players connect to game servers on ports $eff_range, UDP and TCP. Allow that
   range for both UDP and TCP in your provider's firewall. Docker publishes the
   ports itself and bypasses ufw and firewalld (see docs/SERVERS.md, "Firewall").

Logs: journalctl -u $SERVICE -f    Settings: $ENV_FILE
Uninstall: sudo $UNINSTALL_DEST
EOF
	if [ -n "$detected_public" ]; then
		printf 'The panel and the games use the detected address %s.\n' "$detected_addr"
	fi
}

# Grouped so that a download cut off in this line is a syntax error.
{ main "$@"; }
