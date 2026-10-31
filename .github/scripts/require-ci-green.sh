#!/usr/bin/env bash
# Wait for, then require, a green CI on the commit a release tag points at.
# release.yml runs this in its gate job before anything is built or signed, so a
# tag on a red, cancelled or never-run commit never becomes a release.
#
#   require-ci-green.sh <owner/repo> <sha>      (GH_TOKEN in the environment)
#
# The authority is the latest run of the CI workflow (.github/workflows/ci.yml)
# for this exact commit, from a push to main: its conclusion must be "success".
# While that run is queued or in progress the script polls, so tagging a minute
# before CI finishes does not burn the version: the gate waits, and if it times
# out or CI fails, only the gate has run (nothing was built or signed) and
# "Re-run failed jobs" retries it.
#
# As a second check, every job the release relies on must exist in the commit's
# check runs with a "success" conclusion, so a CI workflow edited to skip a job
# cannot turn the run green by omission.
#
#   CI_WAIT_SECONDS  how long to wait for CI to finish        (default 3600)
#   CI_POLL_SECONDS  seconds between looks                    (default 30)
set -euo pipefail

repo="${1:?usage: require-ci-green.sh <owner/repo> <sha>}"
sha="${2:?usage: require-ci-green.sh <owner/repo> <sha>}"
wait_for="${CI_WAIT_SECONDS:-3600}"
poll="${CI_POLL_SECONDS:-30}"

# The job names in .github/workflows/ci.yml that must have run and passed.
required=("Go (gateway)" "Go (api)" "Go (cli)" "Web" "Bundle" "Terraform" "Kubernetes manifests and docs" "Release plumbing")

# latest_run prints "<status> <conclusion> <url>" for the newest push-to-main run
# of ci.yml on this commit, or nothing when there is none yet.
latest_run() {
  gh api "repos/${repo}/actions/workflows/ci.yml/runs?head_sha=${sha}&event=push&branch=main&per_page=30" |
    python3 -I -c '
import json, sys
runs = json.load(sys.stdin).get("workflow_runs", [])
runs = [r for r in runs if r.get("head_sha") == sys.argv[1]]
if runs:
    r = max(runs, key=lambda r: (r.get("created_at", ""), r.get("run_attempt", 0)))
    print(r.get("status") or "unknown", r.get("conclusion") or "none", r.get("html_url") or "")
' "$sha"
}

deadline=$(( $(date +%s) + wait_for ))
while :; do
  line="$(latest_run)"
  status="${line%% *}"
  if [ -z "$line" ]; then
    echo "no CI run for ${sha} on main yet"
  elif [ "$status" = "completed" ]; then
    rest="${line#* }"
    conclusion="${rest%% *}"
    if [ "$conclusion" != "success" ]; then
      echo "CI did not succeed on ${sha}: ${conclusion} (${rest#* })" >&2
      exit 1
    fi
    echo "CI succeeded on ${sha}: ${rest#* }"
    break
  else
    echo "CI is ${status} on ${sha}; waiting"
  fi
  if [ "$(date +%s)" -ge "$deadline" ]; then
    echo "gave up after ${wait_for}s waiting for CI on ${sha} (re-run this job once CI is green)" >&2
    exit 1
  fi
  sleep "$poll"
done

runs="$(gh api "repos/${repo}/commits/${sha}/check-runs?per_page=100" |
  python3 -I -c '
import json, sys
for r in json.load(sys.stdin).get("check_runs", []):
    print("\t".join([r.get("name", ""), r.get("status", ""), r.get("conclusion") or "none"]))
')"

status=0
for name in "${required[@]}"; do
  line="$(printf '%s\n' "$runs" | awk -F'\t' -v n="$name" '$1 == n' | tail -n1)"
  if [ -z "$line" ]; then
    echo "required job not found on ${sha}: ${name}" >&2
    status=1
    continue
  fi
  conclusion="$(printf '%s' "$line" | awk -F'\t' '{print $3}')"
  if [ "$conclusion" != "success" ]; then
    echo "required job did not succeed on ${sha}: ${name} (${conclusion})" >&2
    status=1
  fi
done
exit "$status"
