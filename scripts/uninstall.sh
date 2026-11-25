#!/usr/bin/env bash
# Golem Deploy uninstaller, installed by install.sh as
# /usr/local/sbin/golem-deploy-uninstall.
#
#   sudo golem-deploy-uninstall [--purge [--instance ID]] [--yes]
#
# Without --purge it removes the service, the program, the cache directory
# and the UDP buffer settings, and keeps the data, the settings, the service
# user and every Docker object: game servers keep running. With --purge it
# also deletes this installation's Docker containers, volumes, images and
# network, the data and settings directories and the service user. Docker
# objects are only deleted when this installation's instance id is known;
# when a Docker deletion fails, everything else is kept. Docker itself, its
# apt repository and other packages are never removed, and no prune command
# is ever run. See docs/INSTALL.md.
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
                 (the game servers' saved worlds), images and network,
                 $DATA_DIR, $CONF_DIR and the user and
                 group $SVC_USER. It asks for confirmation and refuses
                 without a terminal unless --yes is given.
  --instance ID  the installation's instance id (32 hexadecimal characters,
                 the Docker label $LABEL_INSTANCE). Only
                 needed when it cannot be read from the installation.
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

# as_service_user CMD... runs CMD as the service user when it exists (so no
# root-owned files appear in the data directory), otherwise as root.
as_service_user() {
	if id -u "$SVC_USER" >/dev/null 2>&1 && command -v runuser >/dev/null 2>&1; then
		(cd / && runuser -u "$SVC_USER" -- "$@")
	else
		(cd / && "$@")
	fi
}

# read_instance_id DATA_DIR prints the instance id the daemon labels its
# Docker objects with: from the installed binary (admin instance-id), or
# from the database with sqlite3. Prints nothing when it cannot be read.
read_instance_id() {
	local dir=$1 id='' db=$1/golem-deploy.db
	[ -d "$dir" ] || return 0
	if [ -x "$BIN_DEST" ] && [ -f "$BIN_DEST" ]; then
		id=$(as_service_user "$BIN_DEST" admin instance-id --data-dir "$dir" </dev/null 2>/dev/null || true)
		if [[ $id =~ ^[a-f0-9]{32}$ ]]; then
			printf '%s' "$id"
			return 0
		fi
	fi
	if [ -f "$db" ] && command -v sqlite3 >/dev/null 2>&1; then
		id=$(as_service_user sqlite3 -readonly "$db" "SELECT value FROM settings WHERE key = 'instance_id';" </dev/null 2>/dev/null || true)
		if [[ $id =~ ^[a-f0-9]{32}$ ]]; then
			printf '%s' "$id"
		fi
	fi
	return 0
}

# mounts_below DIR... prints every mount point at or below one of the DIRs
# (bind mounts included), from /proc/self/mountinfo.
mounts_below() {
	local mp dir _
	[ -r /proc/self/mountinfo ] || return 0
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
	done </proc/self/mountinfo
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
	local text
	shift
	text=$("$@" </dev/null) || return 1
	lines_out=()
	# shellcheck disable=SC2034 # a reference to the caller's array
	[ -z "$text" ] || mapfile -t lines_out < <(printf '%s\n' "$text" | sed '/^$/d')
}

# daemon_containers ARGS... prints the containers (docker ps ARGS) with the
# managed and instance labels and one of the keys only the daemon's
# containers have, once each.
daemon_containers() {
	local key out all=''
	for key in $CONTAINER_KEYS; do
		out=$(docker ps "$@" --filter "label=$LABEL_MANAGED=true" --filter "label=$LABEL_INSTANCE=$instance" \
			--filter "label=$key" </dev/null) || return 1
		all+=$out$'\n'
	done
	printf '%s' "$all" | sed '/^$/d' | LC_ALL=C sort -u
}

# list_docker_objects fills containers, running, volumes, images and
# networks with this installation's objects. Fails when a listing fails.
list_docker_objects() {
	local text
	text=$(daemon_containers -aq --no-trunc) || return 1
	lines containers printf '%s' "$text" || return 1
	text=$(daemon_containers --format '{{.Names}}') || return 1
	lines running printf '%s' "$text" || return 1
	[ -n "$purge" ] || return 0
	local filters=(--filter "label=$LABEL_MANAGED=true" --filter "label=$LABEL_INSTANCE=$instance")
	lines volumes docker volume ls -q "${filters[@]}" || return 1
	text=$(docker image ls -q --no-trunc "${filters[@]}" </dev/null) || return 1
	lines images printf '%s' "$(printf '%s\n' "$text" | LC_ALL=C sort -u)" || return 1
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
	lines ids docker image ls -q --no-trunc "${filter[@]}" || return 1
	if [ "${#ids[@]}" -gt 0 ]; then
		mapfile -t ids < <(printf '%s\n' "${ids[@]}" | LC_ALL=C sort -u)
		for id in "${ids[@]}"; do
			lines part docker image inspect --format "x{{index .Config.Labels \"$LABEL_INSTANCE\"}}" -- "$id" || return 1
			all+=("i ${part[0]#x}")
		done
	fi
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

# containers_using ID prints the names of the containers that use image ID,
# from the "image /name" lines in container_images.
containers_using() {
	printf '%s\n' "$container_images" | awk -v id="$1" '$1 == id { printf "%s%s", sep, substr($2, 2); sep = " " }'
}

# remove_image ID removes this installation's tags (golem-deploy/...) from
# image ID with --no-prune, and the image by id once it has no tags left.
# An image a container uses, or that keeps tags of its own (an operator's
# tag, or an image built FROM one of ours), is left and reported. Fails
# when a deletion fails.
remove_image() {
	local id=$1 short=${1#sha256:} text tag ours=() others=() users err
	short=${short:0:12}
	if ! text=$(docker image inspect --format '{{range .RepoTags}}{{println .}}{{end}}' -- "$id" </dev/null 2>/dev/null); then
		# Removing an earlier image's last tag can remove this one too.
		docker image inspect -- "$id" >/dev/null 2>&1 </dev/null || return 0
		warn "could not inspect image $short"
		return 1
	fi
	while IFS= read -r tag; do
		case $tag in
		'') ;;
		"$IMAGE_NAMESPACE"* | "docker.io/$IMAGE_NAMESPACE"*) ours+=("$tag") ;;
		*) others+=("$tag") ;;
		esac
	done <<<"$text"
	users=$(containers_using "$id")
	if [ -n "$users" ]; then
		note "left image $short: used by the container(s) $users"
		left_images=$((left_images + 1))
		return 0
	fi
	for tag in "${ours[@]}"; do
		docker image rm --no-prune -- "$tag" >/dev/null </dev/null || {
			warn "could not remove the tag $tag of image $short"
			return 1
		}
	done
	if [ "${#others[@]}" -gt 0 ]; then
		note "left image $short: it also has the tag(s) ${others[*]}"
		left_images=$((left_images + 1))
		return 0
	fi
	# Removing its last tag usually removed the image already.
	docker image inspect -- "$id" >/dev/null 2>&1 </dev/null || return 0
	if err=$(docker image rm --no-prune -- "$id" 2>&1 >/dev/null </dev/null); then
		return 0
	fi
	case $err in
	*'dependent child images'*)
		note "left image $short: other images are built from it"
		left_images=$((left_images + 1))
		return 0
		;;
	esac
	warn "could not remove image $short"
	return 1
}

# delete_docker_objects deletes what list_docker_objects found. Prints a
# warning for each failure and fails when anything could not be deleted.
delete_docker_objects() {
	local id bad=0 text
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
		say "Removing ${#images[@]} Docker image(s)"
		# The image of every remaining container, as "image /name" lines.
		container_images=''
		if ! text=$(docker ps -aq --no-trunc </dev/null) ||
			{ [ -n "$text" ] && ! container_images=$(printf '%s\n' "$text" |
				xargs -r docker container inspect --format '{{.Image}} {{.Name}}'); }; then
			warn "could not list the containers' images; no image is removed"
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

# stop_service stops the service and fails unless it really stopped.
stop_service() {
	local state pids
	if have_systemd; then
		say "Stopping $SERVICE"
		systemctl stop "$SERVICE.service" >/dev/null 2>&1 || true
		state=$(systemctl is-active "$SERVICE.service" 2>/dev/null || true)
		case $state in
		active | activating | deactivating | reloading | refreshing)
			printf 'ERROR: %s did not stop (state: %s); see: systemctl status %s\n' "$SERVICE" "$state" "$SERVICE" >&2
			return 1
			;;
		esac
	elif [ -n "$purge" ]; then
		warn "systemd is not running; the service is not stopped"
	fi
	# A daemon started some other way would still use the data directory.
	if [ -n "$purge" ] && command -v pgrep >/dev/null 2>&1; then
		pids=$(pgrep -f "^$BIN_DEST serve" || true)
		if [ -n "$pids" ]; then
			printf 'ERROR: %s serve is still running (pid %s); stop it first\n' "$BIN_DEST" "$(printf '%s' "$pids" | tr '\n' ' ')" >&2
			return 1
		fi
	fi
}

# self_command prints how to run this uninstaller again.
self_command() {
	local self
	self=$(readlink -f -- "$0" 2>/dev/null || printf '%s' "$0")
	if [ "$self" = "$UNINSTALL_DEST" ]; then
		printf 'sudo %s' "$UNINSTALL_DEST"
	else
		printf 'sudo bash %s' "$self"
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
		printf '  %s --purge --instance %s\n' "$(self_command)" "$instance"
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

	local configured_dir=$DATA_DIR other_data_dir='' configured
	if [ -r "$ENV_FILE" ]; then
		configured=$(sed -n 's/^GOLEM_DEPLOY_DATA_DIR=//p' "$ENV_FILE" | tail -n 1)
		case $configured in /*) configured_dir=$configured ;; esac
		[ "$configured_dir" = "$DATA_DIR" ] || other_data_dir=$configured_dir
	fi

	# Nothing may be mounted in a directory this run deletes.
	local dirs=("$CACHE_DIR") check=() dir real mounts
	[ -z "$purge" ] || dirs+=("$DATA_DIR" "$CONF_DIR" "$UNIT_DROPINS")
	for dir in "${dirs[@]}"; do
		if [ -d "$dir" ] && [ ! -L "$dir" ]; then
			check+=("$dir")
			real=$(readlink -f -- "$dir" || true)
			[ -z "$real" ] || [ "$real" = "$dir" ] || check+=("$real")
		fi
	done
	if [ "${#check[@]}" -gt 0 ]; then
		mounts=$(mounts_below "${check[@]}")
		if [ -n "$mounts" ]; then
			{
				printf 'ERROR: something is mounted in a directory this would delete:\n'
				printf '  %s\n' "$mounts"
				printf 'Unmount it (and remove it from /etc/fstab) and run this again. Nothing was changed.\n'
			} >&2
			exit 1
		fi
	fi

	local docker_ok=''
	if command -v docker >/dev/null 2>&1; then
		if docker version --format '{{.Server.Version}}' >/dev/null 2>&1 </dev/null; then
			docker_ok=yes
		elif [ -n "$purge" ]; then
			die "Docker is installed but not reachable, so --purge cannot remove the game servers' containers," \
				"volumes and images. Start Docker (systemctl start docker) and run this again. Nothing was changed."
		fi
	fi

	local instance_source=--instance
	if [ -z "$instance" ]; then
		instance=$(read_instance_id "$configured_dir")
		instance_source="the installation"
	fi

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
					printf 'ERROR: this installation'"'"'s instance id could not be read (from %s\n' "$BIN_DEST"
					printf 'or the database in %s), and Docker holds objects labelled\n' "$configured_dir"
					printf '%s=true. Without the id the uninstaller cannot tell which of them\n' "$LABEL_MANAGED"
					printf 'are this installation'"'"'s, so nothing was changed. Instance ids found:\n\n'
					printf '  %-32s  %10s  %7s  %6s  %8s\n' 'instance id' containers volumes images networks
					while read -r id c v i n; do
						printf '  %-32s  %10d  %7d  %6d  %8d\n' "$id" "$c" "$v" "$i" "$n"
					done <<<"$summary"
					printf '\nRun this again with the id of this installation:\n'
					printf '  %s --purge --instance ID\n' "$(self_command)"
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
		printf 'images that have other tags or that another container uses, and the\n'
		printf 'Inspector hub image (it carries no Golem Deploy label; remove it with\n'
		printf 'docker image rm if unused).\n\n'
		if [ -z "$yes" ]; then
			[ -t 0 ] || die "--purge asks for confirmation on a terminal; pass --yes to run it without one. Nothing was changed."
			local answer=''
			read -r -p 'Type "purge" to delete all of this: ' answer || true
			[ "$answer" = purge ] || die "not confirmed; nothing was changed"
		fi
	fi

	# --- service -------------------------------------------------------------

	stop_service || die "nothing else was changed"

	# --- Docker objects ------------------------------------------------------

	local failed=0 left_images=0 container_images=''
	if [ -n "$docker_step" ]; then
		# List again: the stopped daemon creates nothing new.
		list_docker_objects || docker_failed "could not list Docker's objects"
		delete_docker_objects || docker_failed "some Docker objects could not be deleted (see the warnings above)"
		[ "$left_images" -eq 0 ] || note "$left_images image(s) were left; see above"
	fi

	# --- service, program and data -------------------------------------------

	if have_systemd && [ -e "$UNIT_DEST" ]; then
		say "Disabling $SERVICE"
		systemctl disable "$SERVICE.service" >/dev/null 2>&1 || warn "systemctl disable $SERVICE failed"
	fi
	# A half-removed installation can leave the enablement link behind.
	remove_file "$UNIT_WANTS"
	remove_file "$UNIT_DEST"
	[ -z "$purge" ] || remove_dir "$UNIT_DROPINS"
	if have_systemd; then
		systemctl daemon-reload || true
		systemctl reset-failed "$SERVICE.service" >/dev/null 2>&1 || true
	fi
	remove_file "$BIN_DEST"
	remove_file "$BIN_DEST.new"
	remove_dir "$CACHE_DIR"
	if [ -e "$SYSCTL_FILE" ] || [ -L "$SYSCTL_FILE" ]; then
		remove_file "$SYSCTL_FILE"
		note "the UDP buffer limits it set stay in effect until the next reboot"
	fi

	if [ -n "$purge" ]; then
		say "Removing the data and the settings"
		remove_dir "$DATA_DIR"
		remove_dir "$CONF_DIR"
		if [ -n "$other_data_dir" ] && [ -e "$other_data_dir" ]; then
			warn "the settings named $other_data_dir as the data directory; it is not deleted"
		fi
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
	remove_file "$UNINSTALL_DEST"

	# --- summary -------------------------------------------------------------

	printf '\nGolem Deploy is uninstalled.\n'
	if [ -n "$purge" ]; then
		if [ "$failed" -gt 0 ]; then
			warn "$failed item(s) could not be removed; see the warnings above"
		fi
		printf 'Docker, its apt repository and packages are still installed.\n'
		exit "$((failed > 0))"
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
	fi
	if [ "$failed" -gt 0 ]; then
		warn "$failed item(s) could not be removed; see the warnings above"
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
	exit "$((failed > 0))"
}

{ main "$@"; }
