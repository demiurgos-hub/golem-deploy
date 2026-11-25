#!/usr/bin/env bash
# Build the files of a Golem Deploy release.
#
#   scripts/release.sh [--skip-web] VERSION [OUTDIR]
#
# VERSION is the tag, for example v0.1.0 or v0.2.0-rc.1. OUTDIR (default
# dist/release) receives golem-deploy_linux_amd64.tar.gz,
# golem-deploy_linux_arm64.tar.gz, install.sh (with VERSION stamped in) and
# SHA256SUMS. --skip-web uses an already built frontend/dist instead of
# running npm ci and npm run build. The archives are reproducible: sorted
# names, owner 0, a fixed modification time (SOURCE_DATE_EPOCH, or the time
# of the last commit) and gzip -n. Runs on Linux and in Git Bash on Windows.
# See docs/RELEASING.md.
set -euo pipefail

readonly VERSION_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'
readonly ARCHES='amd64 arm64'

die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

usage() {
	printf 'Usage: %s [--skip-web] VERSION [OUTDIR]\n' "$0" >&2
	exit 2
}

skip_web=''
args=()
while [ "$#" -gt 0 ]; do
	case $1 in
	--skip-web) skip_web=yes ;;
	-h | --help) usage ;;
	-*) die "unknown option $1" ;;
	*) args+=("$1") ;;
	esac
	shift
done
if [ "${#args[@]}" -lt 1 ] || [ "${#args[@]}" -gt 2 ]; then usage; fi
version=${args[0]}
[[ $version =~ $VERSION_RE ]] || die "VERSION must be v followed by a semantic version, for example v0.1.0 or v0.2.0-rc.1 (got $version)"

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
outdir=${args[1]:-$root/dist/release}

tar --version 2>/dev/null | head -n 1 | grep 'GNU tar' >/dev/null || die "GNU tar is needed"
for tool in go gzip sha256sum git; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is needed"
done

epoch=${SOURCE_DATE_EPOCH:-$(git -C "$root" log -1 --format=%ct)}
[[ $epoch =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be a number of seconds (got $epoch)"

grep -qx "readonly RELEASE_VERSION=''" "$root/scripts/install.sh" ||
	die "scripts/install.sh has no empty RELEASE_VERSION line to stamp"

if [ -z "$skip_web" ]; then
	echo "Building the web client"
	(cd "$root/frontend" && npm ci && npm run build)
fi
[ -f "$root/frontend/dist/index.html" ] || die "frontend/dist/index.html is missing; build the web client or drop --skip-web"

mkdir -p "$outdir"
outdir=$(CDPATH='' cd -- "$outdir" && pwd)
for f in golem-deploy_linux_amd64.tar.gz golem-deploy_linux_arm64.tar.gz install.sh SHA256SUMS; do
	rm -f -- "${outdir:?}/$f"
done

stage=$(mktemp -d)
trap 'rm -rf -- "$stage"' EXIT

# Pack the four files in name order with fixed metadata. Modes are set here
# rather than taken from the file system, which does not keep them on
# Windows (the installer sets its own modes anyway).
pack() {
	local dir=$1 out=$2 tarball=$stage/out.tar
	local common=(--force-local --format=ustar --owner=0 --group=0 --numeric-owner --mtime="@$epoch" -C "$dir")
	rm -f -- "$tarball"
	tar -cf "$tarball" "${common[@]}" --mode=0644 LICENSE
	tar -rf "$tarball" "${common[@]}" --mode=0755 golem-deploy
	tar -rf "$tarball" "${common[@]}" --mode=0644 golem-deploy.service
	tar -rf "$tarball" "${common[@]}" --mode=0755 uninstall.sh
	gzip -n -9 -c "$tarball" >"$out"
}

for arch in $ARCHES; do
	echo "Building golem-deploy $version for linux/$arch"
	dir=$stage/$arch
	mkdir -p "$dir"
	(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$version" -o "$dir/golem-deploy" ./cmd/golem-deploy)
	cp -- "$root/deploy/golem-deploy.service" "$dir/golem-deploy.service"
	cp -- "$root/scripts/uninstall.sh" "$dir/uninstall.sh"
	cp -- "$root/LICENSE" "$dir/LICENSE"
	pack "$dir" "$outdir/golem-deploy_linux_$arch.tar.gz"
done

sed "s/^readonly RELEASE_VERSION=''\$/readonly RELEASE_VERSION='$version'/" "$root/scripts/install.sh" >"$outdir/install.sh"
grep -qx "readonly RELEASE_VERSION='$version'" "$outdir/install.sh" || die "could not stamp the version into install.sh"

# sha256sum marks binary mode with "*" on Windows; write the plain form.
(cd "$outdir" && sha256sum golem-deploy_linux_amd64.tar.gz golem-deploy_linux_arm64.tar.gz install.sh |
	sed 's/^\([0-9a-f]\{64\}\) [ *]/\1  /' >SHA256SUMS)

echo "Release $version in $outdir:"
cat "$outdir/SHA256SUMS"
