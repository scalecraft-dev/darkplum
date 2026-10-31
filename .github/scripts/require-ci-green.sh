#!/usr/bin/env bash
# Fail unless every check run on the given commit finished successfully and the
# required CI jobs exist. release.yml calls this before anything is built or signed:
# a tag on a red or half-run commit never becomes a release.
#
#   require-ci-green.sh <owner/repo> <sha>      (GH_TOKEN in the environment)
set -euo pipefail

repo="${1:?usage: require-ci-green.sh <owner/repo> <sha>}"
sha="${2:?usage: require-ci-green.sh <owner/repo> <sha>}"

# The job names in .github/workflows/ci.yml that must have run and passed.
required=("Go (gateway)" "Go (api)" "Go (cli)" "Web" "Terraform" "Kubernetes manifests and docs" "Release plumbing")

runs="$(gh api --paginate "repos/${repo}/commits/${sha}/check-runs?per_page=100" \
  --jq '.check_runs[] | [.name, .status, (.conclusion // "none")] | @tsv')"

status=0
for name in "${required[@]}"; do
  line="$(printf '%s\n' "$runs" | awk -F'\t' -v n="$name" '$1 == n' | tail -n1)"
  if [ -z "$line" ]; then
    echo "required check not found on ${sha}: ${name}" >&2
    status=1
    continue
  fi
  conclusion="$(printf '%s' "$line" | awk -F'\t' '{print $3}')"
  if [ "$conclusion" != "success" ]; then
    echo "required check did not succeed on ${sha}: ${name} (${conclusion})" >&2
    status=1
  fi
done
exit "$status"
