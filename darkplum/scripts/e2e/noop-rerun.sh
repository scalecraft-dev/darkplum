#!/usr/bin/env bash
# Row 3: a second `darkplum deploy` right after a good one changes nothing.
#
#   noop-rerun.sh --install NAME [--expect-vllm] [--expect-oidc]
#
# Snapshots, read-only, before and after: the hash of every key in darkplum-auth
# (SESSION_SIGNING_KEY, SERVICE_KEY_SECRET and the OIDC_* settings) and
# darkplum-user-hmac; every pod in darkplum-system with its uid, restart count and
# image digest; the generation of each Deployment; and the gateway's
# VLLM_UPSTREAMS. The only action is the one `darkplum deploy` the row asks for.
# Fails when anything differs, when deploy fails, or when it does not report "no
# infrastructure changes".
#
#   --expect-vllm   VLLM_UPSTREAMS must be non-empty (after `darkplum models add`)
#   --expect-oidc   OIDC settings must be configured (after `darkplum sso configure`)
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

install="" expect_vllm=0 expect_oidc=0
while [ $# -gt 0 ]; do
  case "$1" in
    --install) install="${2:-}"; shift 2 ;;
    --expect-vllm) expect_vllm=1; shift ;;
    --expect-oidc) expect_oidc=1; shift ;;
    *) e2e_die "unknown argument '$1'" ;;
  esac
done
[ -n "$install" ] || e2e_die "usage: noop-rerun.sh --install NAME [--expect-vllm] [--expect-oidc]"
e2e_need darkplum kubectl jq
e2e_tmpdir
e2e_load_install "$install"
e2e_connect

ns=darkplum-system

# secret_hashes prints "<secret>/<key> <sha256 of the stored value>" per key.
secret_hashes() {
  local spec secret key json
  for spec in \
    darkplum-auth:SESSION_SIGNING_KEY darkplum-auth:SERVICE_KEY_SECRET \
    darkplum-auth:OIDC_ISSUER darkplum-auth:OIDC_CLIENT_ID darkplum-auth:OIDC_CLIENT_SECRET darkplum-auth:OIDC_REDIRECT_URL \
    darkplum-user-hmac:user-hmac-key darkplum-user-hmac:user-hmac-key-id; do
    secret="${spec%%:*}"
    key="${spec##*:}"
    json="$(e2e_kubectl -n "$ns" get secret "$secret" -o json 2>/dev/null)" || json=""
    if [ -z "$json" ]; then
      printf '%s/%s <no such secret>\n' "$secret" "$key"
    else
      printf '%s/%s %s\n' "$secret" "$key" "$(printf '%s' "$json" | jq -r --arg k "$key" '.data[$k] // "<absent>"' | e2e_sha)"
    fi
  done
}

pods() {
  e2e_kubectl -n "$ns" get pods -o json | jq -r '
    .items | sort_by(.metadata.name)[] |
    "\(.metadata.name) uid=\(.metadata.uid) restarts=\([.status.containerStatuses[]?.restartCount] | add // 0) images=\([.status.containerStatuses[]?.imageID] | join(","))"'
}

generations() {
  e2e_kubectl -n "$ns" get deploy -o json | jq -r '.items | sort_by(.metadata.name)[] | "\(.metadata.name) generation=\(.metadata.generation)"'
}

vllm_upstreams() {
  e2e_kubectl -n "$ns" get deploy darkplum-gateway -o json |
    jq -r '[.spec.template.spec.containers[0].env[]? | select(.name == "VLLM_UPSTREAMS") | .value] | first // ""'
}

snapshot() {
  {
    echo "## secrets (sha256 of stored value)"; secret_hashes
    echo "## pods"; pods
    echo "## deployments"; generations
    echo "## VLLM_UPSTREAMS"; vllm_upstreams
  } >"$1"
}

snapshot "$E2E_TMP/before"
if [ "$expect_vllm" = 1 ] && [ -z "$(vllm_upstreams)" ]; then
  e2e_die "--expect-vllm: the gateway has no VLLM_UPSTREAMS before the re-run (run 'darkplum models add' first)"
fi
if [ "$expect_oidc" = 1 ]; then
  grep -q '^darkplum-auth/OIDC_ISSUER ' "$E2E_TMP/before" || e2e_die "--expect-oidc: no OIDC_ISSUER line"
  oidc="$(e2e_kubectl -n "$ns" get secret darkplum-auth -o json | jq -r '.data.OIDC_ISSUER // "" | @base64d')"
  case "$oidc" in "" | unconfigured) e2e_die "--expect-oidc: OIDC is not configured before the re-run (run 'darkplum sso configure' first)" ;; esac
fi
e2e_ok "snapshot taken ($(wc -l <"$E2E_TMP/before" | tr -d ' ') lines)"

e2e_note "running: darkplum deploy (the one action in this row)"
rc=0
e2e_dp deploy >"$E2E_TMP/deploy.out" 2>&1 || rc=$?
e2e_strip <"$E2E_TMP/deploy.out" >"$E2E_TMP/deploy.txt"
[ "$rc" = 0 ] || { cat "$E2E_TMP/deploy.txt" >&2; e2e_die "the re-run failed with exit $rc"; }
e2e_expect_in "$E2E_TMP/deploy.txt" "no infrastructure changes" "the re-run must find nothing to change in Terraform"
e2e_ok "deploy exited 0 and reported no infrastructure changes"

snapshot "$E2E_TMP/after"
if ! diff -u "$E2E_TMP/before" "$E2E_TMP/after" >"$E2E_TMP/diff"; then
  cat "$E2E_TMP/diff" >&2
  e2e_die "the re-run changed something (secrets, pods, restarts, image digests, generations or VLLM_UPSTREAMS)"
fi
e2e_ok "secrets, pods, restart counts, image digests, generations and VLLM_UPSTREAMS are identical"
[ "$expect_vllm" = 0 ] || e2e_ok "VLLM_UPSTREAMS preserved: $(vllm_upstreams)"
