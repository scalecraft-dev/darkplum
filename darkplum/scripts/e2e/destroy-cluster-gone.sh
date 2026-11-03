#!/usr/bin/env bash
# Row 7c: cluster already gone (deleted in the console or by terraform): destroy
# skips the Kubernetes phase loudly and still finishes.
#
#   E2E_CONFIRM_DESTROY=<cluster> destroy-cluster-gone.sh --install NAME [-- extra destroy flags]
#
# Precondition, checked read-only: `aws eks describe-cluster` says the cluster does
# not exist. The script will not delete it for you. Then it runs the one action,
# `darkplum destroy --confirm <cluster>`, and requires exit 0, the loud skip
# (the "gone:" row and "SKIPPING the Kubernetes phase"), a destroyed
# infrastructure, and the closing line; the cluster must still not exist after.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

install="" extra=()
while [ $# -gt 0 ]; do
  case "$1" in
    --install) install="${2:-}"; shift 2 ;;
    --) shift; extra=("$@"); break ;;
    *) e2e_die "unknown argument '$1'" ;;
  esac
done
[ -n "$install" ] || e2e_die "usage: destroy-cluster-gone.sh --install NAME [-- destroy flags]"
e2e_need darkplum jq aws
e2e_tmpdir
e2e_load_install "$install"
e2e_require_destroy_confirmation

cluster_exists() {
  local out
  if out="$(e2e_aws eks describe-cluster --name "$E2E_CLUSTER" 2>&1)"; then return 0; fi
  case "$out" in
    *ResourceNotFoundException*) return 1 ;;
    *) e2e_die "cannot tell whether the cluster exists (not a 'not found' answer): $out" ;;
  esac
}

if cluster_exists; then
  e2e_die "cluster '$E2E_CLUSTER' still exists in $E2E_REGION: delete it first (console or terraform), as the row requires"
fi
e2e_ok "the cluster does not exist in $E2E_REGION"

rc=0
e2e_dp destroy --confirm "$E2E_CLUSTER" ${extra[@]+"${extra[@]}"} >"$E2E_TMP/destroy.out" 2>&1 || rc=$?
e2e_strip <"$E2E_TMP/destroy.out" >"$E2E_TMP/destroy.txt"
[ "$rc" = 0 ] || { cat "$E2E_TMP/destroy.txt" >&2; e2e_die "destroy failed with exit $rc"; }
e2e_expect_in "$E2E_TMP/destroy.txt" "gone: $E2E_CLUSTER does not exist in $E2E_REGION" "the missing cluster must be reported"
e2e_expect_in "$E2E_TMP/destroy.txt" "SKIPPING the Kubernetes phase" "the skip must be loud"
e2e_expect_in "$E2E_TMP/destroy.txt" "destroyed" "the infrastructure must be reported destroyed"
e2e_expect_in "$E2E_TMP/destroy.txt" "Done." "destroy must reach its closing line"
e2e_ok "destroy skipped the Kubernetes phase loudly and finished"
if cluster_exists; then e2e_die "the cluster exists again after destroy"; fi
e2e_ok "the cluster is still gone"
