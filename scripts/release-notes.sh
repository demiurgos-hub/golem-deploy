#!/usr/bin/env bash
# Print the release notes of a version: its section of CHANGELOG.md.
#
#   scripts/release-notes.sh VERSION
#
# VERSION is the tag (v0.1.0) or the bare version (0.1.0). The section is
# the text below "## [0.1.0]" (optionally followed by " - DATE") up to the
# next "## " heading or link definition. It fails when the section is
# missing or empty.
set -euo pipefail

[ "$#" -eq 1 ] || {
	printf 'Usage: %s VERSION\n' "$0" >&2
	exit 2
}
version=${1#v}
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || {
	printf 'ERROR: %s is not a version such as v0.1.0\n' "$1" >&2
	exit 1
}

root=$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
notes=$(awk -v v="$version" '
	/^## / {
		if (found) exit
		if (index($0, "## [" v "]") == 1) {
			rest = substr($0, length("## [" v "]") + 1)
			if (rest == "" || rest ~ /^ - /) { found = 1; next }
		}
	}
	# Link definitions at the end of the file belong to no section.
	found && /^\[[^]]+\]: / { exit }
	found { print }
' "$root/CHANGELOG.md")

# Drop leading blank lines; the command substitution drops trailing ones.
notes=$(printf '%s\n' "$notes" | sed '/./,$!d')
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	printf 'ERROR: CHANGELOG.md has no notes for %s (a "## [%s] - YYYY-MM-DD" section)\n' "$version" "$version" >&2
	exit 1
fi
printf '%s\n' "$notes"
