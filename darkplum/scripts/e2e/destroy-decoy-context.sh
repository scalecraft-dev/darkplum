#!/usr/bin/env bash
# Row 7a: destroy never touches the cluster the ambient kubectl context points at.
#
#   E2E_CONFIRM_DESTROY=<cluster> destroy-decoy-context.sh --install NAME \
#       --decoy-context CTX [--decoy-kubeconfig FILE] [-- extra destroy flags]
#
# The decoy is a second cluster you can afford to have looked at (a second kind
# cluster, or a scratch EKS context; never one that matters). The script
#   1. fingerprints the decoy (names and uids of namespaces, workloads, services,
#      claims, with get only);
#   2. writes a temporary copy of the decoy's kubeconfig whose current-context IS
#      the decoy and runs `darkplum destroy --confirm <cluster>` with KUBECONFIG
#      pointing at it, so the ambient context is the decoy for the whole run. Your
#      real ~/.kube/config is never read or modified;
#   3. fingerprints the decoy again and fails on any difference;
#   4. fails if the destroy log names the decoy's context or API server at all.
# Extra flags after -- are passed to destroy (for example --confirm-no-snapshot).
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

install="" decoy_ctx="" decoy_kc="${KUBECONFIG:-$HOME/.kube/config}"
decoy_kc="${decoy_kc%%:*}"
extra=()
while [ $# -gt 0 ]; do
  case "$1" in
    --install) install="${2:-}"; shift 2 ;;
    --decoy-context) decoy_ctx="${2:-}"; shift 2 ;;
    --decoy-kubeconfig) decoy_kc="${2:-}"; shift 2 ;;
    --) shift; extra=("$@"); break ;;
    *) e2e_die "unknown argument '$1'" ;;
  esac
done
[ -n "$install" ] && [ -n "$decoy_ctx" ] || e2e_die "usage: destroy-decoy-context.sh --install NAME --decoy-context CTX [--decoy-kubeconfig FILE] [-- destroy flags]"
e2e_need darkplum kubectl jq
e2e_tmpdir
e2e_load_install "$install"
e2e_require_destroy_confirmation
e2e_connect

[ -f "$decoy_kc" ] || e2e_die "decoy kubeconfig $decoy_kc does not exist"
E2E_DECOY_KUBECONFIG="$decoy_kc"
E2E_DECOY_CONTEXT="$decoy_ctx"
e2e_decoy_kubectl config get-contexts -o name | grep -qx -- "$decoy_ctx" || e2e_die "context '$decoy_ctx' is not in $decoy_kc"
[ "$decoy_ctx" != "$E2E_CONTEXT" ] || e2e_die "the decoy context has the install's own name; pick another cluster"

server_of() { kubectl --kubeconfig "$1" --context "$2" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}'; }
decoy_server="$(server_of "$E2E_DECOY_KUBECONFIG" "$E2E_DECOY_CONTEXT")"
install_server="$(server_of "$E2E_KUBECONFIG" "$E2E_CONTEXT")"
[ "$decoy_server" != "$install_server" ] || e2e_die "the decoy and the install are the same API server ($decoy_server)"

fingerprint() {
  e2e_decoy_kubectl get namespaces,deployments,statefulsets,daemonsets,services,persistentvolumeclaims,configmaps \
    --all-namespaces -o json |
    jq -r '.items | map("\(.kind) \(.metadata.namespace // "-")/\(.metadata.name) uid=\(.metadata.uid) deleting=\(.metadata.deletionTimestamp // "no")") | sort[]'
}

fingerprint >"$E2E_TMP/decoy.before"
[ -s "$E2E_TMP/decoy.before" ] || e2e_die "the decoy fingerprint is empty: is '$decoy_ctx' reachable?"
e2e_ok "decoy '$decoy_ctx' fingerprinted ($(wc -l <"$E2E_TMP/decoy.before" | tr -d ' ') objects)"

# The ambient kubeconfig for the run: the decoy's, with the decoy as current context.
ambient="$E2E_TMP/ambient-kubeconfig"
(umask 077 && kubectl --kubeconfig "$E2E_DECOY_KUBECONFIG" config view --raw --flatten >"$ambient")
kubectl --kubeconfig "$ambient" config use-context "$decoy_ctx" >/dev/null
[ "$(kubectl --kubeconfig "$ambient" config current-context)" = "$decoy_ctx" ] || e2e_die "could not make the decoy the ambient context"

logdir="$(e2e_home)/installs/$E2E_INSTALL/logs"
before_log="$(e2e_newest_log "$logdir" destroy-)"
e2e_note "running: darkplum destroy --confirm $E2E_CLUSTER with the decoy as the ambient context (the one action in this row)"
rc=0
KUBECONFIG="$ambient" e2e_dp destroy --confirm "$E2E_CLUSTER" ${extra[@]+"${extra[@]}"} >"$E2E_TMP/destroy.out" 2>&1 || rc=$?
e2e_strip <"$E2E_TMP/destroy.out" >"$E2E_TMP/destroy.txt"

fingerprint >"$E2E_TMP/decoy.after"
if ! diff -u "$E2E_TMP/decoy.before" "$E2E_TMP/decoy.after" >"$E2E_TMP/decoy.diff"; then
  cat "$E2E_TMP/decoy.diff" >&2
  e2e_die "THE DECOY CLUSTER CHANGED during destroy"
fi
e2e_ok "the decoy cluster is byte-for-byte what it was (names and uids)"

# Neither the screen nor the destroy log may name the decoy: it was never contacted.
newest="$(e2e_newest_log "$logdir" destroy-)"
[ -n "$newest" ] && [ "$newest" != "$before_log" ] || e2e_die "destroy wrote no new log in $logdir"
for needle in "$decoy_ctx" "$decoy_server"; do
  e2e_expect_not_in "$logdir/$newest" "$needle" "the destroy log names the decoy"
  e2e_expect_not_in "$E2E_TMP/destroy.txt" "$needle" "the destroy output names the decoy"
done
e2e_ok "neither the output nor $newest mentions the decoy's context or server"

[ "$rc" = 0 ] || { cat "$E2E_TMP/destroy.txt" >&2; e2e_die "destroy itself failed with exit $rc (the decoy was untouched, but the row needs a finished destroy)"; }
e2e_expect_in "$E2E_TMP/destroy.txt" "Done." "destroy must reach its closing line"
e2e_ok "destroy finished (exit 0)"
