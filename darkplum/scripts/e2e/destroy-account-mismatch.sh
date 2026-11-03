#!/usr/bin/env bash
# Row 7d: credentials for a different account than the install's: destroy aborts
# and deletes nothing.
#
#   E2E_CONFIRM_DESTROY=<cluster> destroy-account-mismatch.sh --install NAME
#
# Works on a copy: a temporary DARKPLUM_HOME holds the install's record with its
# pinned account replaced by 000000000000 (and its settings and backend), so the
# real workspace is never edited. `darkplum destroy --confirm <cluster>` run
# against it uses the install's real, valid credentials, which are therefore for
# the wrong account as far as the record is concerned. It must exit non-zero with
# the account refusal before terraform, and the real cluster must be unchanged.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

install=""
while [ $# -gt 0 ]; do
  case "$1" in
    --install) install="${2:-}"; shift 2 ;;
    *) e2e_die "unknown argument '$1'" ;;
  esac
done
[ -n "$install" ] || e2e_die "usage: destroy-account-mismatch.sh --install NAME"
e2e_need darkplum kubectl jq aws
e2e_tmpdir
e2e_load_install "$install"
e2e_require_destroy_confirmation
e2e_connect

wrong=000000000000
case ",$E2E_ACCOUNTS," in *",$wrong,"*) e2e_die "the install is pinned to $wrong?" ;; esac

facts() {
  e2e_aws eks describe-cluster --name "$E2E_CLUSTER" --query 'cluster.[name,status,arn]' --output text
  e2e_kubectl get namespaces,deployments,statefulsets,services,persistentvolumeclaims --all-namespaces -o json |
    jq -r '.items | map("\(.kind) \(.metadata.namespace // "-")/\(.metadata.name) uid=\(.metadata.uid)") | sort[]'
}
facts >"$E2E_TMP/before"
e2e_ok "recorded the cluster ($(wc -l <"$E2E_TMP/before" | tr -d ' ') lines)"

copy="$E2E_TMP/home/installs/$E2E_INSTALL"
mkdir -p "$copy"
chmod 700 "$E2E_TMP/home" "$E2E_TMP/home/installs" "$copy"
real="$(e2e_home)/installs/$E2E_INSTALL"
for f in terraform.tfvars backend.hcl; do
  [ ! -f "$real/$f" ] || cp "$real/$f" "$copy/$f"
done
jq --arg w "$wrong" '.account_ids = [$w]' "$(e2e_record "$E2E_INSTALL")" >"$copy/install.json"
chmod 600 "$copy"/*

rc=0
DARKPLUM_HOME="$E2E_TMP/home" e2e_dp destroy --confirm "$E2E_CLUSTER" >"$E2E_TMP/destroy.out" 2>&1 || rc=$?
e2e_strip <"$E2E_TMP/destroy.out" >"$E2E_TMP/destroy.txt"
[ "$rc" != 0 ] || { cat "$E2E_TMP/destroy.txt" >&2; e2e_die "destroy exited 0 against the wrong account"; }
e2e_expect_in "$E2E_TMP/destroy.txt" "the install is in $wrong" "the refusal must name the account mismatch"
e2e_expect_in "$E2E_TMP/destroy.txt" "Nothing was deleted" "the refusal must say nothing was deleted"
e2e_expect_not_in "$E2E_TMP/destroy.txt" "terraform destroy (" "terraform destroy must not start"
e2e_ok "destroy refused (exit $rc) with the account mismatch before terraform"

facts >"$E2E_TMP/after"
if ! diff -u "$E2E_TMP/before" "$E2E_TMP/after" >"$E2E_TMP/diff"; then
  cat "$E2E_TMP/diff" >&2
  e2e_die "the cluster changed while destroy was refusing"
fi
e2e_ok "the cluster and its workloads are unchanged"
