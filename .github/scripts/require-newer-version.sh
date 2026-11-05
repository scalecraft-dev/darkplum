#!/usr/bin/env bash
# A release version must be newer than every vX.Y.Z tag that already exists.
# release.yml runs this in its gate job: the release is published and the
# Homebrew tap bumped with nobody in between, so an older version tagged after
# a newer one would become Latest and downgrade every brew user.
#
#   require-newer-version.sh <X.Y.Z>     (run inside the repository, tags fetched)
#
# Only exact vX.Y.Z tags count. Anything else (v1.2.3-rc1, v1.2.3x) is ignored,
# so a stray tag can never block a version. The tag ruleset forbids deleting
# tags, so a version whose gate was rejected still counts as taken: tag the
# next one.
set -euo pipefail

version="${1:?usage: require-newer-version.sh <X.Y.Z>}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "not an X.Y.Z version: $version"; exit 1; }

# grep exits 1 on no match, which under pipefail would end the script with no
# message; the `|| true` keeps an empty list an empty list.
others="$(git tag -l | { grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' || true; } | { grep -vx "v$version" || true; })"
highest="$(printf '%s\n' "$others" | sed '/^$/d' | sort -V | tail -n 1)"

if [ -z "$highest" ]; then
  echo "v$version is the first version tag"
  exit 0
fi
newest="$(printf '%s\n%s\n' "$highest" "v$version" | sort -V | tail -n 1)"
if [ "$newest" != "v$version" ]; then
  echo "v$version is not newer than $highest, the highest existing version tag: tag a higher version"
  exit 1
fi
echo "v$version is newer than $highest"
