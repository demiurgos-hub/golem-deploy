#!/usr/bin/env bash
# Golem Deploy uninstaller, installed by install.sh as
# /usr/local/sbin/golem-deploy-uninstall.
#
#   sudo golem-deploy-uninstall [--purge [--instance ID]] [--yes]
#
# Without --purge it removes the service, the program, the cache directory
# and the UDP buffer settings, and keeps the data, the settings, the service
# user and every Docker object: game servers keep running. With --purge it
# also deletes this installation's Docker containers, volumes, image tags
# and network, the data and settings directories and the service user.
# Docker objects are only deleted when this installation's instance id is
# known; when a Docker deletion fails, everything else is kept. Docker
# itself, its apt repository and other packages are never removed, and no
# prune command is ever run. See docs/INSTALL.md.
#
# Everything happens in functions called on the last line, so the script can
# remove itself and a truncated copy runs nothing.
set -euo pipefail

readonly SERVICE=golem-deploy
readonly SVC_USER=golem-deploy
readonly BIN_DEST=/usr/local/bin/golem-deploy
readonly UNINSTALL_DEST=/usr/local/sbin/golem-deploy-uninstall
readonly DATA_DIR=/var/lib/golem-deploy
readonly CACHE_DIR=/var/cache/golem-deploy
readonly CONF_DIR=/etc/golem-deploy
readonly ENV_FILE=$CONF_DIR/golem-deploy.env
readonly UNIT_DEST=/etc/systemd/system/golem-deploy.service
readonly UNIT_DROPINS=/etc/systemd/system/golem-deploy.service.d
readonly UNIT_WANTS=/etc/systemd/system/multi-user.target.wants/golem-deploy.service
readonly SYSCTL_FILE=/etc/sysctl.d/60-golem-deploy.conf
# The service talks to the Docker daemon on the default local socket.
readonly DOCKER_SOCKET=unix:///var/run/docker.sock
# Labels from internal/docker (validate.go, data.go). Every object the
# daemon creates has the managed and instance labels. Containers the daemon
# creates also have the server-id key (servers, helpers and, empty, the
# Inspector hub), and newer ones the inspector-hub and helper keys. Build
# images never carry these keys (their labels are checked to be exactly the
# build's), so they tell the daemon's containers apart from containers
# someone else started from its images, which inherit the image's labels.
readonly LABEL_MANAGED=dev.golemengine.deploy.managed
readonly LABEL_INSTANCE=dev.golemengine.deploy.instance
readonly CONTAINER_KEYS='dev.golemengine.deploy.server-id dev.golemengine.deploy.inspector-hub dev.golemengine.deploy.helper'
# Build images are tagged golem-deploy/<project>:<tag> (internal/docker,
# build.go). Only tags in this namespace are removed.
readonly IMAGE_NAMESPACE=golem-deploy/

usage() {
	cat <<EOF
Usage: sudo golem-deploy-uninstall [--purge [--instance ID]] [--yes]

Removes the golem-deploy service, $BIN_DEST, $CACHE_DIR,
$SYSCTL_FILE and this uninstaller. The data directory, the settings,
the user $SVC_USER and all Docker objects are kept, and game servers keep
running.

  --purge        also delete this installation's Docker containers, volumes
                 (the game servers' saved worlds), image tags and network,
                 $DATA_DIR, $CONF_DIR and the user and
                 group $SVC_USER. It asks for confirmation and refuses
                 without a terminal unless --yes is given.
  --instance ID  the installation's instance id (32 lower-case hexadecimal
                 characters, the Docker label $LABEL_INSTANCE).
                 Only needed when it cannot be read from the installation.
  --yes          do not ask for confirmation
  -h, --help     show this help

Docker itself, its apt repository and other packages are never removed.
EOF
}

say() { printf '==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
warn() { printf 'WARNING: %s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

have_systemd() {
	command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

# docker runs the docker CLI against the Docker daemon the service uses,
# whatever DOCKER_HOST, DOCKER_CONTEXT or root's Docker contexts select.
docker() {
	env -u DOCKER_HOST -u DOCKER_CONTEXT -u DOCKER_TLS_VERIFY -u DOCKER_CERT_PATH -u DOCKER_API_VERSION \
		docker --host "$DOCKER_SOCKET" "$@"
}

# shell_quote WORD prints WORD quoted for a POSIX shell when it needs it.
shell_quote() {
	local q="'\\''"
	case $1 in
	'' | *[!A-Za-z0-9_./:=@+-]*) printf "'%s'" "${1//\'/"$q"}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# as_service_user CMD... runs CMD as the service user when it exists (so no
# root-owned files appear in the data directory), otherwise as root.
as_service_user() {
	if id -u "$SVC_USER" >/dev/null 2>&1 && command -v runuser >/dev/null 2>&1; then
		(cd / && runuser -u "$SVC_USER" -- "$@")
	else
		(cd / && "$@")
	fi
}

# settings_data_dir prints the data directory the settings file gives the
# service. It fails unless the last assignment of GOLEM_DEPLOY_DATA_DIR is
# a plain line GOLEM_DEPLOY_DATA_DIR=/path whose path the daemon accepts and
# systemd does not rewrite: no quotes, "$", backslash, control character,
# "," or "=", and no space at the end.
settings_data_dir() {
	local text line trimmed last=''
	text=$(cat -- "$ENV_FILE" 2>/dev/null) || return 1
	while IFS= read -r line; do
		trimmed=${line#"${line%%[![:space:]]*}"}
		case $trimmed in '#'* | ';'*) continue ;; esac
		[[ $trimmed =~ ^GOLEM_DEPLOY_DATA_DIR[[:space:]]*= ]] && last=$line
	done <<<"$text"
	[[ $last =~ ^GOLEM_DEPLOY_DATA_DIR=(/.*)$ ]] || return 1
	last=${BASH_REMATCH[1]}
	case $last in *[\"\'\$\\,=]* | *[[:cntrl:]]* | *[[:space:]]) return 1 ;; esac
	printf '%s' "$last"
}

# read_instance_id DIR sets instance to the id the daemon labels its Docker
# objects with, from the installed binary (admin instance-id), or sets
# instance_why to the reason it cannot.
read_instance_id() {
	local dir=$1 out rc=0
	if [ ! -d "$dir" ]; then
		instance_why="the data directory $dir does not exist"
		return 0
	fi
	if [ ! -f "$BIN_DEST" ] || [ ! -x "$BIN_DEST" ]; then
		instance_why="$BIN_DEST, which reads it from the database, is missing"
		return 0
	fi
	out=$(as_service_user "$BIN_DEST" admin instance-id --data-dir "$dir" </dev/null 2>/dev/null) || rc=$?
	if [ "$rc" -eq 0 ] && [[ $out =~ ^[a-f0-9]{32}$ ]]; then
		instance=$out
	elif [ "$rc" -eq 0 ]; then
		instance_why="golem-deploy admin instance-id printed something other than an id"
	elif [ "$rc" -eq 2 ]; then
		instance_why="the installed golem-deploy is too old to print it"
	else
		instance_why="golem-deploy admin instance-id could not read it from $dir (not initialised, or unreadable)"
	fi
}

# mounts_below DIR... prints every mount point at or below one of the DIRs
# (bind mounts included), from /proc/self/mountinfo. Fails when that cannot
# be read.
mounts_below() {
	local info mp dir _
	info=$(cat /proc/self/mountinfo 2>/dev/null) && [ -n "$info" ] || return 1
	while read -r _ _ _ _ mp _; do
		# mountinfo escapes space, tab, newline and backslash as octal.
		mp=${mp//\\040/ }
		mp=${mp//\\011/$'\t'}
		mp=${mp//\\012/$'\n'}
		mp=${mp//\\134/\\}
		for dir in "$@"; do
			if [ "$mp" = "$dir" ] || [[ $mp == "$dir"/* ]]; then
				printf '%s\n' "$mp"
				break
			fi
		done
	done <<<"$info"
}

# check_mounts DIR... fails, with a message, when something is mounted at or
# below one of the DIRs, or when the mount table cannot be read.
check_mounts() {
	local check=() dir real mounts
	for dir in "$@"; do
		if [ -d "$dir" ] && [ ! -L "$dir" ]; then
			check+=("$dir")
			real=$(readlink -f -- "$dir") || real=''
			[ -z "$real" ] || [ "$real" = "$dir" ] || check+=("$real")
		fi
	done
	[ "${#check[@]}" -gt 0 ] || return 0
	if ! mounts=$(mounts_below "${check[@]}"); then
		printf 'ERROR: /proc/self/mountinfo cannot be read, so nothing shows whether something\n' >&2
		printf 'is mounted in a directory this would delete.\n' >&2
		return 1
	fi
	if [ -n "$mounts" ]; then
		printf 'ERROR: something is mounted in a directory this would delete:\n' >&2
		while IFS= read -r dir; do printf '  %s\n' "$dir" >&2; done <<<"$mounts"
		printf 'Unmount it (and remove it from /etc/fstab).\n' >&2
		return 1
	fi
}

# remove_file PATH removes a file or a link. Anything else found there (a
# directory, for example) is left alone with a warning.
remove_file() {
	if [ -L "$1" ] || [ -f "$1" ]; then
		if rm -f -- "$1"; then note "removed $1"; else
			warn "could not remove $1"
			failed=$((failed + 1))
		fi
	elif [ -e "$1" ]; then
		warn "$1 is not a file; it is left alone"
	fi
}

# remove_dir PATH removes a directory tree without crossing into other file
# systems. A link is removed, not followed.
remove_dir() {
	if [ -L "$1" ]; then
		remove_file "$1"
	elif [ -d "$1" ]; then
		if rm -rf --one-file-system -- "$1"; then note "removed $1"; else
			warn "could not remove all of $1"
			failed=$((failed + 1))
		fi
	elif [ -e "$1" ]; then
		warn "$1 is not a directory; it is left alone"
	fi
}

# lines VAR CMD... runs CMD and stores its non-empty output lines in the
# array VAR. It fails when CMD fails, so a failed listing never looks like
# an empty one.
lines() {
	local -n lines_out=$1
	local text line
	shift
	text=$("$@" </dev/null) || return 1
	lines_out=()
	while IFS= read -r line; do
		[ -z "$line" ] || lines_out+=("$line")
	done <<<"$text"
}

# unique VAR removes empty and repeated elements from the array VAR,
# keeping the first of each.
unique() {
	local -n unique_in=$1
	local -A unique_seen=()
	local unique_out=() x
	for x in "${unique_in[@]}"; do
		[ -n "$x" ] && [ -z "${unique_seen[$x]:-}" ] || continue
		unique_seen[$x]=1
		unique_out+=("$x")
	done
	unique_in=("${unique_out[@]}")
}

# daemon_containers VAR ARGS... stores in the array VAR the containers
# (docker ps ARGS) with the managed and instance labels and one of the keys
# only the daemon's containers have, once each.
daemon_containers() {
	local -n dc_out=$1
	local key part=()
	shift
	dc_out=()
	for key in $CONTAINER_KEYS; do
		lines part docker ps "$@" --filter "label=$LABEL_MANAGED=true" --filter "label=$LABEL_INSTANCE=$instance" \
			--filter "label=$key" || return 1
		dc_out+=("${part[@]}")
	done
	unique dc_out
}

# list_docker_objects fills containers, running, volumes, images and
# networks with this installation's objects. Fails when a listing fails.
list_docker_objects() {
	daemon_containers containers -aq --no-trunc || return 1
	daemon_containers running --format '{{.Names}}' || return 1
	[ -n "$purge" ] || return 0
	local filters=(--filter "label=$LABEL_MANAGED=true" --filter "label=$LABEL_INSTANCE=$instance")
	lines volumes docker volume ls -q "${filters[@]}" || return 1
	# --all: an untagged image that another image is built from is listed too.
	lines images docker image ls -aq --no-trunc "${filters[@]}" || return 1
	unique images
	lines networks docker network ls -q --no-trunc "${filters[@]}" || return 1
}

# managed_by_instance prints one line "ID CONTAINERS VOLUMES IMAGES NETWORKS"
# for every instance id found on objects with the managed label ("(none)"
# for objects without one). Prints nothing when there are no such objects.
# Fails when a listing fails.
managed_by_instance() {
	local filter=(--filter "label=$LABEL_MANAGED=true") fmt="x{{.Label \"$LABEL_INSTANCE\"}}" all=() part=() id ids=()
	# Every object prints one line ("x" and its label), even without the label.
	lines part docker ps -a "${filter[@]}" --format "$fmt" || return 1
	for id in "${part[@]}"; do all+=("c ${id#x}"); done
	lines part docker volume ls "${filter[@]}" --format "$fmt" || return 1
	for id in "${part[@]}"; do all+=("v ${id#x}"); done
	lines part docker network ls "${filter[@]}" --format "$fmt" || return 1
	for id in "${part[@]}"; do all+=("n ${id#x}"); done
	lines ids docker image ls -aq --no-trunc "${filter[@]}" || return 1
	unique ids
	for id in "${ids[@]}"; do
		lines part docker image inspect --format "x{{index .Config.Labels \"$LABEL_INSTANCE\"}}" -- "$id" || return 1
		all+=("i ${part[0]#x}")
	done
	[ "${#all[@]}" -gt 0 ] || return 0
	printf '%s\n' "${all[@]}" | awk '
		{
			kind = substr($0, 1, 1); id = substr($0, 3)
			gsub(/[^A-Za-z0-9._-]/, "?", id)
			if (id == "") id = "(none)"
			n[id, kind]++
			if (!(id in seen)) { seen[id] = 1; order[++k] = id }
		}
		END {
			for (j = 1; j <= k; j++) {
				id = order[j]
				printf "%s %d %d %d %d\n", id, n[id, "c"], n[id, "v"], n[id, "i"], n[id, "n"]
			}
		}'
}

# image_tags ID stores the tags of image ID in the array tags. Fails when
# the image cannot be inspected; image_gone then tells whether it is gone.
image_tags() {
	lines tags docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}' -- "$1" 2>/dev/null
}

# image_gone ID succeeds when a listing of all images succeeds and does not
# contain ID.
image_gone() {
	local all=() x
	lines all docker image ls -aq --no-trunc || return 1
	for x in "${all[@]}"; do
		[ "$x" != "$1" ] || return 1
	done
}

# containers_using ID prints the names of the containers that use image ID,
# from the "image /name" lines in container_images.
containers_using() {
	printf '%s\n' "$container_images" | awk -v id="$1" '$1 == id { printf "%s%s", sep, substr($2, 2); sep = " " }'
}

# remove_image ID removes this installation's tags (golem-deploy/...) from
# image ID with --no-prune. Right before each removal it checks that the tag
# still names ID. Images are never removed by id: removing an image's last
# tag removes the image, and an image left untagged or with other tags (an
# operator's tag, or an image built FROM one of ours, which inherits our
# labels) is reported, as is one a container uses. Fails when a step fails.
remove_image() {
	local id=$1 short=${1#sha256:} tags=() tag cur users
	short=${short:0:12}
	if ! image_tags "$id"; then
		image_gone "$id" && return 0
		warn "could not inspect image $short"
		return 1
	fi
	users=$(containers_using "$id")
	if [ -n "$users" ]; then
		note "left image $short: used by the container(s) $users"
		left_images=$((left_images + 1))
		return 0
	fi
	for tag in "${tags[@]}"; do
		case $tag in
		"$IMAGE_NAMESPACE"* | "docker.io/$IMAGE_NAMESPACE"*) ;;
		*) continue ;;
		esac
		if ! cur=$(docker image inspect --format '{{.Id}}' -- "$tag" </dev/null); then
			warn "could not inspect the tag $tag"
			return 1
		fi
		if [ "$cur" != "$id" ]; then
			note "left the tag $tag: it now names another image"
			continue
		fi
		docker image rm --no-prune -- "$tag" >/dev/null </dev/null || {
			warn "could not remove the tag $tag of image $short"
			return 1
		}
	done
	if ! image_tags "$id"; then
		image_gone "$id" && return 0
		warn "could not inspect image $short"
		return 1
	fi
	if [ "${#tags[@]}" -gt 0 ]; then
		note "left image $short: it has the tag(s) ${tags[*]}"
	else
		note "left image $short: it has no tags (docker image rm $short removes it)"
	fi
	left_images=$((left_images + 1))
}

# delete_docker_objects deletes what list_docker_objects found. Prints a
# warning for each failure and fails when anything could not be deleted.
delete_docker_objects() {
	local id bad=0 all=()
	if [ "${#containers[@]}" -gt 0 ]; then
		say "Removing ${#containers[@]} Docker container(s)"
		for id in "${containers[@]}"; do
			docker rm -f -- "$id" >/dev/null </dev/null || {
				warn "could not remove container $id"
				bad=$((bad + 1))
			}
		done
	fi
	if [ "${#volumes[@]}" -gt 0 ]; then
		say "Removing ${#volumes[@]} Docker volume(s)"
		for id in "${volumes[@]}"; do
			docker volume rm -- "$id" >/dev/null </dev/null || {
				warn "could not remove volume $id"
				bad=$((bad + 1))
			}
		done
	fi
	if [ "${#images[@]}" -gt 0 ]; then
		say "Removing the $IMAGE_NAMESPACE tags of ${#images[@]} Docker image(s)"
		# The image of every remaining container, as "image /name" lines.
		container_images=''
		if ! lines all docker ps -aq --no-trunc ||
			{ [ "${#all[@]}" -gt 0 ] && ! container_images=$(docker container inspect --format '{{.Image}} {{.Name}}' -- "${all[@]}" </dev/null); }; then
			warn "could not list the containers' images; no image tag is removed"
			bad=$((bad + 1))
		else
			for id in "${images[@]}"; do
				remove_image "$id" || bad=$((bad + 1))
			done
		fi
	fi
	if [ "${#networks[@]}" -gt 0 ]; then
		say "Removing ${#networks[@]} Docker network(s)"
		for id in "${networks[@]}"; do
			docker network rm -- "$id" >/dev/null </dev/null || {
				warn "could not remove network $id (are other containers attached to it?)"
				bad=$((bad + 1))
			}
		done
	fi
	[ "$bad" -eq 0 ]
}

# data_dir_in_use DIR succeeds when a golem-deploy serve process holds the
# lock on DIR/.lock, which it takes for as long as it runs
# (internal/datadir). Returns 1 when nothing holds it, 2 when that cannot
# be tested.
data_dir_in_use() {
	local lock=$1/.lock rc=0
	[ -e "$lock" ] || [ -L "$lock" ] || return 1
	# Opening a FIFO or a device could block or have effects.
	[ -f "$lock" ] || return 2
	command -v flock >/dev/null 2>&1 || return 2
	# A shared lock conflicts with serve's exclusive one. The file is opened
	# for reading, so nothing is created.
	flock --nonblock --shared --conflict-exit-code 75 9 2>/dev/null 9<"$lock" || rc=$?
	case $rc in
	0) return 1 ;;
	75) return 0 ;;
	*) return 2 ;;
	esac
}

# stop_service DIR... stops the service and fails unless systemd says it
# stopped (or does not know it) and no golem-deploy serve still holds the
# lock of one of the data directories DIR.
stop_service() {
	local out state key val load='' active='' dir rc
	if have_systemd; then
		say "Stopping $SERVICE"
		out=$(systemctl stop "$SERVICE.service" 2>&1 </dev/null) || true
		if ! state=$(systemctl show -p LoadState -p ActiveState "$SERVICE.service" 2>&1 </dev/null); then
			printf 'ERROR: could not query the state of %s: %s\n' "$SERVICE" "$state" >&2
			return 1
		fi
		while IFS='=' read -r key val; do
			case $key in
			LoadState) load=$val ;;
			ActiveState) active=$val ;;
			esac
		done <<<"$state"
		if [ -z "$load" ] || { [ "$active" != inactive ] && [ "$active" != failed ]; }; then
			printf 'ERROR: %s did not stop (state: %s/%s); see: systemctl status %s\n' \
				"$SERVICE" "${load:-unknown}" "${active:-unknown}" "$SERVICE" >&2
			[ -z "$out" ] || printf '%s\n' "$out" >&2
			return 1
		fi
	else
		warn "systemd is not running; the service is not stopped"
	fi
	for dir in "$@"; do
		rc=0
		data_dir_in_use "$dir" || rc=$?
		case $rc in
		0)
			printf 'ERROR: a golem-deploy serve process still uses %s (it holds %s/.lock).\n' "$dir" "$dir" >&2
			printf 'Stop it first (ps -ef | grep "golem-deploy serve" finds it).\n' >&2
			return 1
			;;
		2)
			printf 'ERROR: cannot tell whether a golem-deploy serve process still uses %s:\n' "$dir" >&2
			printf 'flock is missing or %s/.lock cannot be read.\n' "$dir" >&2
			return 1
			;;
		esac
	done
}

# print_retry ARGS prints how to run this uninstaller again with the
# argument string ARGS.
print_retry() {
	local src=${BASH_SOURCE[0]:-} self
	# Piped into bash, BASH_SOURCE names no copy of this script.
	if [ -n "$src" ] && [ -f "$src" ] &&
		[ "$(sed -n 2p -- "$src" 2>/dev/null)" = '# Golem Deploy uninstaller, installed by install.sh as' ]; then
		self=$(readlink -f -- "$src") || self=$src
		if [ "$self" = "$UNINSTALL_DEST" ]; then
			printf '  sudo %s%s\n' "$UNINSTALL_DEST" "${1:+ $1}"
		else
			printf '  sudo bash %s%s\n' "$(shell_quote "$self")" "${1:+ $1}"
		fi
	else
		printf '  (this copy was piped into bash; save uninstall.sh from the release archive)\n'
		printf '  sudo bash uninstall.sh%s\n' "${1:+ $1}"
	fi
}

# docker_failed MESSAGE stops after a failed Docker step. Apart from the
# Docker objects already deleted nothing has changed: the program, this
# uninstaller, the data, the settings and the user are kept, so the purge
# can run again.
docker_failed() {
	{
		printf 'ERROR: %s.\n' "$1"
		printf 'Nothing else was removed: %s, the settings, the user %s,\n' "$DATA_DIR" "$SVC_USER"
		printf '%s and this uninstaller are kept. The service is stopped\n' "$BIN_DEST"
		printf '(systemctl start %s starts it again). Fix the cause and run:\n' "$SERVICE"
		print_retry "$retry_args"
	} >&2
	exit 1
}

main() {
	local purge='' yes='' instance=''
	while [ "$#" -gt 0 ]; do
		case $1 in
		--purge) purge=yes ;;
		--yes | -y) yes=yes ;;
		--instance)
			[ "$#" -ge 2 ] || die "--instance needs a value (see --help)"
			instance=$2
			shift
			;;
		--instance=*) instance=${1#*=} ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument $1 (see --help)" ;;
		esac
		shift
	done
	if [ -n "$instance" ]; then
		[ -n "$purge" ] || die "--instance only applies to --purge"
		[[ $instance =~ ^[a-f0-9]{32}$ ]] || die "--instance must be 32 lower-case hexadecimal characters (got $instance)"
	fi

	[ "$(id -u)" -eq 0 ] || die "run this script as root (for example with sudo)"

	# --- survey (nothing changes yet) ----------------------------------------

	# The data directory the service uses. Without a settings file (a
	# half-removed installation) that is the default one. When the file
	# sets it to something this script cannot use as a plain path, the
	# instance id is not read from anywhere.
	local configured_dir=$DATA_DIR other_data_dir='' instance_why=''
	if [ -e "$ENV_FILE" ] || [ -L "$ENV_FILE" ]; then
		if ! configured_dir=$(settings_data_dir); then
			configured_dir=''
			instance_why="$ENV_FILE does not set GOLEM_DEPLOY_DATA_DIR to a plain absolute path"
			instance_why+=" (one line GOLEM_DEPLOY_DATA_DIR=/path without quotes), so it is unclear which database the service uses"
		fi
		case $configured_dir in
		'' | "$DATA_DIR") ;;
		*) other_data_dir=$configured_dir ;;
		esac
	fi

	# Nothing may be mounted in a directory this run deletes.
	local dirs=("$CACHE_DIR")
	[ -z "$purge" ] || dirs+=("$DATA_DIR" "$CONF_DIR" "$UNIT_DROPINS")
	check_mounts "${dirs[@]}" || die "nothing was changed; fix this and run the uninstaller again"

	local docker_ok=''
	# type -P: the docker command, not the docker function above.
	if type -P docker >/dev/null 2>&1; then
		if docker version --format '{{.Server.Version}}' >/dev/null 2>&1 </dev/null; then
			docker_ok=yes
		elif [ -n "$purge" ]; then
			die "Docker is installed but not reachable at $DOCKER_SOCKET, so --purge cannot remove the game servers'" \
				"containers, volumes and images. Start Docker (systemctl start docker) and run this again. Nothing was changed."
		fi
	fi

	local instance_source=--instance
	if [ -z "$instance" ]; then
		instance_source="the installation"
		[ -z "$configured_dir" ] || read_instance_id "$configured_dir"
	fi

	local retry_args=''
	if [ -n "$purge" ]; then
		retry_args="--purge${instance:+ --instance $instance}"
	fi
	retry_args+=${yes:+ --yes}
	retry_args=${retry_args# }

	# Docker objects are only deleted with a known instance id, so a purge
	# never reaches another installation's objects (or an older database's).
	local docker_step='' summary id c v i n
	if [ -n "$purge" ] && [ -n "$docker_ok" ]; then
		if [ -n "$instance" ]; then
			docker_step=yes
		else
			summary=$(managed_by_instance) || die "could not list Docker's objects; nothing was changed"
			if [ -n "$summary" ]; then
				{
					printf 'ERROR: this installation'"'"'s instance id is unknown: %s.\n' "$instance_why"
					printf 'Docker holds objects labelled %s=true, and without the id\n' "$LABEL_MANAGED"
					printf 'the uninstaller cannot tell which of them are this installation'"'"'s, so nothing\n'
					printf 'was changed. Instance ids found:\n\n'
					printf '  %-32s  %10s  %7s  %6s  %8s\n' 'instance id' containers volumes images networks
					while read -r id c v i n; do
						printf '  %-32s  %10d  %7d  %6d  %8d\n' "$id" "$c" "$v" "$i" "$n"
					done <<<"$summary"
					printf '\nRun this again with the id of this installation:\n'
					print_retry "--purge --instance ID${yes:+ --yes}"
				} >&2
				exit 1
			fi
		fi
	fi

	local containers=() running=() volumes=() images=() networks=()
	if [ -n "$docker_ok" ] && [ -n "$instance" ]; then
		list_docker_objects || die "could not list Docker's objects; nothing was changed"
	fi

	local data_size=''
	if [ -d "$DATA_DIR" ]; then
		data_size=$(du -sh -- "$DATA_DIR" 2>/dev/null | cut -f1 || true)
	fi

	if [ -n "$purge" ]; then
		printf '\n--purge deletes, besides the service and the program:\n\n'
		if [ -n "$docker_step" ]; then
			printf '  Docker containers: %d (%d running)\n' "${#containers[@]}" "${#running[@]}"
			printf '  Docker volumes:    %d (the game servers'"'"' saved worlds)\n' "${#volumes[@]}"
			printf '  Docker images:     %d (their %s tags)\n' "${#images[@]}" "$IMAGE_NAMESPACE"
			printf '  Docker networks:   %d\n' "${#networks[@]}"
			printf '  Only objects labelled %s=%s\n' "$LABEL_INSTANCE" "$instance"
			printf '  (the instance id from %s).\n' "$instance_source"
		elif [ -n "$docker_ok" ]; then
			printf '  No Docker objects (Docker holds none labelled %s=true).\n' "$LABEL_MANAGED"
		else
			printf '  No Docker objects (Docker is not installed).\n'
		fi
		[ ! -e "$DATA_DIR" ] || printf '  %s%s: database, keys, checkouts, backups\n' "$DATA_DIR" "${data_size:+ ($data_size)}"
		[ ! -e "$CONF_DIR" ] || printf '  %s: settings and TLS copies\n' "$CONF_DIR"
		[ -z "$other_data_dir" ] || printf '  (not %s, the data directory the settings name: delete it yourself)\n' "$other_data_dir"
		if id -u "$SVC_USER" >/dev/null 2>&1 || getent group "$SVC_USER" >/dev/null; then
			printf '  the user and group %s\n' "$SVC_USER"
		fi
		printf '\nKept: Docker, its apt repository and packages, other installations'"'"' objects,\n'
		printf 'images that are left without tags, have other tags or that another container\n'
		printf 'uses (they are listed), and the Inspector hub image (it carries no Golem\n'
		printf 'Deploy label; remove it with docker image rm if unused).\n\n'
		if [ -z "$yes" ]; then
			[ -t 0 ] || die "--purge asks for confirmation on a terminal; pass --yes to run it without one. Nothing was changed."
			local answer=''
			read -r -p 'Type "purge" to delete all of this: ' answer || true
			[ "$answer" = purge ] || die "not confirmed; nothing was changed"
		fi
	fi

	# --- service -------------------------------------------------------------

	local lock_dirs=("$DATA_DIR")
	[ -z "$other_data_dir" ] || lock_dirs+=("$other_data_dir")
	if ! stop_service "${lock_dirs[@]}"; then
		{
			printf 'Nothing was deleted. Fix this and run:\n'
			print_retry "$retry_args"
		} >&2
		exit 1
	fi

	# --- Docker objects ------------------------------------------------------

	local failed=0 left_images=0 container_images=''
	if [ -n "$docker_step" ]; then
		# List again: the stopped daemon creates nothing new.
		list_docker_objects || docker_failed "could not list Docker's objects"
		delete_docker_objects || docker_failed "some Docker objects could not be deleted (see the warnings above)"
		[ "$left_images" -eq 0 ] || note "$left_images image(s) were left; see above"
	fi

	# --- directories, service, program and user ------------------------------

	# Something could have been mounted since the survey.
	if ! check_mounts "${dirs[@]}"; then
		{
			[ -z "$docker_step" ] || printf 'The Docker objects listed above were deleted. '
			printf 'Nothing else was removed. Fix this and run:\n'
			print_retry "$retry_args"
		} >&2
		exit 1
	fi
	remove_dir "$CACHE_DIR"
	if [ -n "$purge" ]; then
		say "Removing the data and the settings"
		remove_dir "$DATA_DIR"
		remove_dir "$CONF_DIR"
		remove_dir "$UNIT_DROPINS"
		if [ -n "$other_data_dir" ] && [ -e "$other_data_dir" ]; then
			warn "the settings named $other_data_dir as the data directory; it is not deleted"
		fi
	fi

	if have_systemd && [ -e "$UNIT_DEST" ]; then
		say "Disabling $SERVICE"
		systemctl disable "$SERVICE.service" >/dev/null 2>&1 || warn "systemctl disable $SERVICE failed"
	fi
	# A half-removed installation can leave the enablement link behind.
	remove_file "$UNIT_WANTS"
	remove_file "$UNIT_DEST"
	if have_systemd; then
		systemctl daemon-reload || true
		systemctl reset-failed "$SERVICE.service" >/dev/null 2>&1 || true
	fi
	remove_file "$BIN_DEST"
	remove_file "$BIN_DEST.new"
	if [ -e "$SYSCTL_FILE" ] || [ -L "$SYSCTL_FILE" ]; then
		remove_file "$SYSCTL_FILE"
		note "the UDP buffer limits it set stay in effect until the next reboot"
	fi

	if [ -n "$purge" ]; then
		if id -u "$SVC_USER" >/dev/null 2>&1; then
			if userdel "$SVC_USER"; then note "removed the user $SVC_USER"; else
				warn "could not remove the user $SVC_USER"
				failed=$((failed + 1))
			fi
		fi
		if getent group "$SVC_USER" >/dev/null; then
			if groupdel "$SVC_USER"; then note "removed the group $SVC_USER"; else
				warn "could not remove the group $SVC_USER"
				failed=$((failed + 1))
			fi
		fi
	fi

	remove_file "$UNINSTALL_DEST.new"

	# --- summary -------------------------------------------------------------

	if [ "$failed" -gt 0 ]; then
		# Keep this uninstaller so the rest can be removed later.
		local left=() path
		for path in "$UNIT_DEST" "$BIN_DEST" "$CACHE_DIR" "$SYSCTL_FILE"; do
			[ ! -e "$path" ] || left+=("$path")
		done
		if [ -n "$purge" ]; then
			for path in "$DATA_DIR" "$CONF_DIR" "$UNIT_DROPINS"; do
				[ ! -e "$path" ] || left+=("$path")
			done
			! id -u "$SVC_USER" >/dev/null 2>&1 || left+=("the user $SVC_USER")
			! getent group "$SVC_USER" >/dev/null || left+=("the group $SVC_USER")
		fi
		{
			printf '\nERROR: Golem Deploy is only partly uninstalled: %d item(s) could not be removed\n' "$failed"
			printf '(see the warnings above).'
			if [ "${#left[@]}" -gt 0 ]; then
				printf ' Left:\n'
				printf '  %s\n' "${left[@]}"
			else
				printf '\n'
			fi
			printf 'Fix the cause and run:\n'
			print_retry "$retry_args"
		} >&2
		exit 1
	fi
	remove_file "$UNINSTALL_DEST"

	printf '\nGolem Deploy is uninstalled.\n'
	if [ -n "$purge" ]; then
		printf 'Docker, its apt repository and packages are still installed.\n'
		exit 0
	fi
	local kept=()
	[ ! -e "$DATA_DIR" ] || kept+=("$DATA_DIR (data, keys, backups)")
	[ ! -e "$CONF_DIR" ] || kept+=("$CONF_DIR (settings)")
	! id -u "$SVC_USER" >/dev/null 2>&1 || kept+=("the user $SVC_USER")
	[ -z "$docker_ok" ] || kept+=("every Docker object")
	if [ "${#kept[@]}" -gt 0 ]; then
		printf 'Kept, and picked up again by a new installation:\n'
		printf '  %s\n' "${kept[@]}"
		printf 'To delete them later, run the uninstaller from a release archive:\n'
		printf '  sudo bash uninstall.sh --purge%s\n' "${instance:+ --instance $instance}"
		[ -n "$instance" ] || [ -z "$instance_why" ] || note "(the instance id is unknown: $instance_why)"
	fi
	if [ -n "$docker_ok" ]; then
		if [ -z "$instance" ]; then
			printf 'Managed containers may keep running without the panel; list them with:\n'
			printf '  docker ps --filter label=%s=true\n' "$LABEL_MANAGED"
		elif [ "${#running[@]}" -eq 0 ]; then
			printf 'No managed containers are running.\n'
		else
			printf '\nThese managed containers (game servers, the Inspector hub) keep running\n'
			printf 'without the panel:\n'
			printf '  %s\n' "${running[@]}"
			printf '\nTo stop and remove them (their volumes and images stay):\n'
			printf '  docker stop %s\n' "${running[*]}"
			printf '  docker rm %s\n' "${running[*]}"
			printf 'docker ps -a --filter label=%s=%s also lists stopped ones.\n' "$LABEL_INSTANCE" "$instance"
		fi
	fi
}

{ main "$@"; }
