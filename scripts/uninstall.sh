#!/usr/bin/env bash
# Golem Deploy uninstaller, installed by install.sh as
# /usr/local/sbin/golem-deploy-uninstall.
#
#   sudo golem-deploy-uninstall [--purge] [--yes]
#
# Without --purge it removes the service, the program, the cache directory
# and the UDP buffer settings, and keeps the data, the settings, the service
# user and every Docker object: game servers keep running. With --purge it
# also deletes this installation's Docker containers, volumes, images and
# network, the data and settings directories and the service user. Docker
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
# Labels from internal/docker (validate.go). Every container the daemon
# creates has the inspector-hub label (empty except on the Inspector hub),
# and verified build images never carry it, so it tells the daemon's
# containers apart from containers someone else started from its images
# (containers inherit image labels).
readonly LABEL_MANAGED=dev.golemengine.deploy.managed
readonly LABEL_INSTANCE=dev.golemengine.deploy.instance
readonly LABEL_CREATED=dev.golemengine.deploy.inspector-hub

usage() {
	cat <<EOF
Usage: sudo golem-deploy-uninstall [--purge] [--yes]

Removes the golem-deploy service, $BIN_DEST, $CACHE_DIR,
$SYSCTL_FILE and this uninstaller. The data directory, the settings,
the user $SVC_USER and all Docker objects are kept, and game servers keep
running.

  --purge   also delete this installation's Docker containers, volumes (the
            game servers' saved worlds), images and network, $DATA_DIR,
            $CONF_DIR and the user and group $SVC_USER. It asks
            for confirmation and refuses without a terminal unless --yes
            is given.
  --yes     do not ask for confirmation
  -h, --help  show this help

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

# The instance id the daemon labels its Docker objects with, read from the
# database when sqlite3 is installed. Empty when it cannot be read.
read_instance_id() {
	local data_dir=$DATA_DIR db id
	if [ -r "$ENV_FILE" ]; then
		local configured
		configured=$(sed -n 's/^GOLEM_DEPLOY_DATA_DIR=//p' "$ENV_FILE" | tail -n 1)
		case $configured in /*) data_dir=$configured ;; esac
	fi
	db=$data_dir/golem-deploy.db
	[ -f "$db" ] || return 0
	command -v sqlite3 >/dev/null 2>&1 || return 0
	id=$(sqlite3 -readonly "$db" "SELECT value FROM settings WHERE key = 'instance_id';" 2>/dev/null || true)
	[[ $id =~ ^[a-f0-9]{32}$ ]] && printf '%s' "$id"
	return 0
}

remove_path() {
	if [ -e "$1" ] || [ -L "$1" ]; then
		rm -rf -- "$1"
		note "removed $1"
	fi
}

main() {
	local purge='' yes=''
	while [ "$#" -gt 0 ]; do
		case $1 in
		--purge) purge=yes ;;
		--yes | -y) yes=yes ;;
		-h | --help)
			usage
			exit 0
			;;
		*) die "unknown argument $1 (see --help)" ;;
		esac
		shift
	done

	[ "$(id -u)" -eq 0 ] || die "run this script as root (for example with sudo)"

	# --- survey (nothing changes yet) ----------------------------------------

	local docker_ok='' instance='' filters=() scope
	if command -v docker >/dev/null 2>&1; then
		if docker version --format '{{.Server.Version}}' >/dev/null 2>&1; then
			docker_ok=yes
		elif [ -n "$purge" ]; then
			die "Docker is installed but not reachable, so --purge cannot remove the game servers' containers," \
				"volumes and images. Start Docker (systemctl start docker) and run this again. Nothing was changed."
		fi
	fi
	instance=$(read_instance_id)
	filters=(--filter "label=$LABEL_MANAGED=true")
	if [ -n "$instance" ]; then
		filters+=(--filter "label=$LABEL_INSTANCE=$instance")
		scope="this installation's Docker objects (label $LABEL_INSTANCE=$instance)"
	else
		scope="every Docker object labelled $LABEL_MANAGED=true (this installation's instance id could not be read"
		if command -v sqlite3 >/dev/null 2>&1; then scope+=")"; else scope+=": sqlite3 is not installed)"; fi
	fi

	local containers=() running=() volumes=() images=() networks=()
	if [ -n "$docker_ok" ]; then
		mapfile -t containers < <(docker ps -aq --no-trunc "${filters[@]}" --filter "label=$LABEL_CREATED")
		mapfile -t running < <(docker ps --format '{{.Names}}' "${filters[@]}" --filter "label=$LABEL_CREATED")
		if [ -n "$purge" ]; then
			mapfile -t volumes < <(docker volume ls -q "${filters[@]}")
			mapfile -t images < <(docker image ls -q --no-trunc "${filters[@]}" | sort -u)
			mapfile -t networks < <(docker network ls -q --no-trunc "${filters[@]}")
		fi
	fi

	local data_size='' other_data_dir=''
	if [ -r "$ENV_FILE" ]; then
		other_data_dir=$(sed -n 's/^GOLEM_DEPLOY_DATA_DIR=//p' "$ENV_FILE" | tail -n 1)
		[ "$other_data_dir" != "$DATA_DIR" ] || other_data_dir=''
	fi
	if [ -d "$DATA_DIR" ]; then
		data_size=$(du -sh -- "$DATA_DIR" 2>/dev/null | cut -f1 || true)
	fi

	if [ -n "$purge" ]; then
		printf '\n--purge deletes, besides the service and the program:\n\n'
		if [ -n "$docker_ok" ]; then
			printf '  Docker containers: %d (%d running)\n' "${#containers[@]}" "${#running[@]}"
			printf '  Docker volumes:    %d (the game servers'"'"' saved worlds)\n' "${#volumes[@]}"
			printf '  Docker images:     %d\n' "${#images[@]}"
			printf '  Docker networks:   %d\n' "${#networks[@]}"
			printf '  Scope: %s.\n' "$scope"
		else
			printf '  No Docker objects (Docker is not installed).\n'
		fi
		[ ! -e "$DATA_DIR" ] || printf '  %s%s: database, keys, checkouts, backups\n' "$DATA_DIR" "${data_size:+ ($data_size)}"
		[ ! -e "$CONF_DIR" ] || printf '  %s: settings and TLS copies\n' "$CONF_DIR"
		[ -z "$other_data_dir" ] || printf '  (not %s, the data directory the settings name: delete it yourself)\n' "$other_data_dir"
		if id -u "$SVC_USER" >/dev/null 2>&1 || getent group "$SVC_USER" >/dev/null; then
			printf '  the user and group %s\n' "$SVC_USER"
		fi
		printf '\nKept: Docker, its apt repository and packages, and the Inspector hub image\n'
		printf '(it carries no Golem Deploy label; remove it with docker image rm if unused).\n\n'
		if [ -z "$yes" ]; then
			[ -t 0 ] || die "--purge asks for confirmation on a terminal; pass --yes to run it without one. Nothing was changed."
			local answer=''
			read -r -p 'Type "purge" to delete all of this: ' answer || true
			[ "$answer" = purge ] || die "not confirmed; nothing was changed"
		fi
	fi

	# --- service and program -------------------------------------------------

	if have_systemd; then
		say "Stopping and disabling $SERVICE"
		systemctl stop "$SERVICE.service" >/dev/null 2>&1 || true
		if [ -e "$UNIT_DEST" ]; then
			systemctl disable "$SERVICE.service" >/dev/null 2>&1 || warn "systemctl disable $SERVICE failed"
		fi
	else
		warn "systemd is not running; the service is not stopped"
	fi
	# A half-removed installation can leave the enablement link behind.
	if [ -L "$UNIT_WANTS" ]; then
		rm -f -- "$UNIT_WANTS"
		note "removed $UNIT_WANTS"
	fi
	remove_path "$UNIT_DEST"
	[ -z "$purge" ] || remove_path "$UNIT_DROPINS"
	if have_systemd; then
		systemctl daemon-reload || true
		systemctl reset-failed "$SERVICE.service" >/dev/null 2>&1 || true
	fi
	remove_path "$BIN_DEST"
	remove_path "$BIN_DEST.new"
	remove_path "$CACHE_DIR"
	if [ -e "$SYSCTL_FILE" ]; then
		remove_path "$SYSCTL_FILE"
		note "the UDP buffer limits it set stay in effect until the next reboot"
	fi

	# --- purge ---------------------------------------------------------------

	local failed=0 id tags
	if [ -n "$purge" ] && [ -n "$docker_ok" ]; then
		if [ "${#containers[@]}" -gt 0 ]; then
			say "Removing ${#containers[@]} Docker container(s)"
			for id in "${containers[@]}"; do
				docker rm -f -- "$id" >/dev/null || {
					warn "could not remove container $id"
					failed=$((failed + 1))
				}
			done
		fi
		if [ "${#volumes[@]}" -gt 0 ]; then
			say "Removing ${#volumes[@]} Docker volume(s)"
			for id in "${volumes[@]}"; do
				docker volume rm -- "$id" >/dev/null || {
					warn "could not remove volume $id"
					failed=$((failed + 1))
				}
			done
		fi
		if [ "${#images[@]}" -gt 0 ]; then
			say "Removing ${#images[@]} Docker image(s)"
			for id in "${images[@]}"; do
				# Remove by tag so an image with several tags goes without -f.
				tags=$(docker image inspect --format '{{range .RepoTags}}{{.}} {{end}}' -- "$id" 2>/dev/null || true)
				# shellcheck disable=SC2086 # tags are words without spaces
				docker image rm -- ${tags:-$id} >/dev/null || {
					warn "could not remove image $id"
					failed=$((failed + 1))
				}
			done
		fi
		if [ "${#networks[@]}" -gt 0 ]; then
			say "Removing ${#networks[@]} Docker network(s)"
			for id in "${networks[@]}"; do
				docker network rm -- "$id" >/dev/null || {
					warn "could not remove network $id (are other containers attached?)"
					failed=$((failed + 1))
				}
			done
		fi
	fi
	if [ -n "$purge" ]; then
		say "Removing the data and the settings"
		remove_path "$DATA_DIR"
		remove_path "$CONF_DIR"
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

	remove_path "$UNINSTALL_DEST.new"
	remove_path "$UNINSTALL_DEST"

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
		printf '  sudo bash uninstall.sh --purge\n'
	fi
	if [ -z "$docker_ok" ]; then
		return 0
	fi
	if [ "${#running[@]}" -eq 0 ]; then
		printf 'No managed containers are running.\n'
		return 0
	fi
	printf '\nThese managed containers (game servers, the Inspector hub) keep running\n'
	printf 'without the panel:\n'
	printf '  %s\n' "${running[@]}"
	printf '\nTo stop and remove them (their volumes and images stay):\n'
	printf '  docker stop %s\n' "${running[*]}"
	printf '  docker rm %s\n' "${running[*]}"
	printf 'docker ps -a --filter label=%s=true also lists stopped ones.\n' "$LABEL_MANAGED"
}

{ main "$@"; }
