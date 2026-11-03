#!/usr/bin/env bash
# Row 7b: with an expired AWS session, destroy refuses and deletes nothing.
#
#   E2E_CONFIRM_DESTROY=<cluster> E2E_CONFIRM_LOGOUT=1 destroy-expired-session.sh --install NAME
#   destroy-expired-session.sh --install NAME --already-expired   (you expired the session yourself)
#
# While the session is still valid it records what exists: the EKS cluster's
# status, every database whose identifier names the cluster with its deletion
# protection (the flip the original bug skipped), and the workloads, services and
# claims in the install's cluster. Then the session is ended
# (`aws sso logout --profile <the install's profile>`; needs E2E_CONFIRM_LOGOUT=1
# and an SSO profile), `darkplum destroy --confirm <cluster>` must exit non-zero
# before it changes anything, and after you log back in (E2E_RELOGIN_CMD, default
# `aws sso login --profile <profile>`) the same facts must be identical.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib.sh
. "$here/lib.sh"

install="" already=0
while [ $# -gt 0 ]; do
  case "$1" in
    --install) install="${2:-}"; shift 2 ;;
    --already-expired) already=1; shift ;;
    *) e2e_die "unknown argument '$1'" ;;
  esac
done
[ -n "$install" ] || e2e_die "usage: destroy-expired-session.sh --install NAME [--already-expired]"
e2e_need darkplum kubectl jq aws
e2e_tmpdir
e2e_load_install "$install"
e2e_require_destroy_confirmation

facts() {
  e2e_aws eks describe-cluster --name "$E2E_CLUSTER" --query 'cluster.[name,status,arn]' --output text
  e2e_aws rds describe-db-instances --output json |
    jq -r --arg c "$E2E_CLUSTER" '.DBInstances[] | select(.DBInstanceIdentifier | contains($c)) | "rds \(.DBInstanceIdentifier) deletion_protection=\(.DeletionProtection) status=\(.DBInstanceStatus)"' | sort
  e2e_kubectl get namespaces,deployments,statefulsets,services,persistentvolumeclaims --all-namespaces -o json |
    jq -r '.items | map("\(.kind) \(.metadata.namespace // "-")/\(.metadata.name) uid=\(.metadata.uid)") | sort[]'
}

if [ "$already" = 0 ]; then
  [ "${E2E_CONFIRM_LOGOUT:-}" = 1 ] || e2e_die "this ends your AWS SSO session for profile '$E2E_PROFILE'. Set E2E_CONFIRM_LOGOUT=1, or expire it yourself and pass --already-expired"
  [ -n "$E2E_PROFILE" ] || e2e_die "the install has no profile to log out of; expire the session yourself and pass --already-expired"
  e2e_connect
  facts >"$E2E_TMP/before" || e2e_die "cannot read the install's facts with a valid session"
  [ -s "$E2E_TMP/before" ] || e2e_die "no facts recorded"
  e2e_ok "recorded the install's facts ($(wc -l <"$E2E_TMP/before" | tr -d ' ') lines)"
  aws sso logout --profile "$E2E_PROFILE"
else
  e2e_note "--already-expired: nothing could be recorded with a valid session, so only the refusal is asserted"
fi

if e2e_aws sts get-caller-identity >/dev/null 2>&1; then
  e2e_die "the session is still valid: the row needs it expired"
fi
e2e_ok "the session is expired"

rc=0
e2e_dp destroy --confirm "$E2E_CLUSTER" >"$E2E_TMP/destroy.out" 2>&1 || rc=$?
e2e_strip <"$E2E_TMP/destroy.out" >"$E2E_TMP/destroy.txt"
[ "$rc" != 0 ] || { cat "$E2E_TMP/destroy.txt" >&2; e2e_die "destroy exited 0 with an expired session"; }
e2e_expect_in "$E2E_TMP/destroy.txt" "Nothing was deleted" "the refusal must say nothing was deleted"
e2e_expect_not_in "$E2E_TMP/destroy.txt" "terraform destroy (" "terraform destroy must not start"
e2e_expect_not_in "$E2E_TMP/destroy.txt" "turning off the database's deletion protection" "the database must not be touched"
e2e_expect_not_in "$E2E_TMP/destroy.txt" "Done." "no closing line after a refusal"
e2e_ok "destroy refused (exit $rc), said nothing was deleted, and never started terraform destroy"

relogin="${E2E_RELOGIN_CMD:-aws sso login --profile $E2E_PROFILE}"
[ -n "$E2E_PROFILE" ] || [ -n "${E2E_RELOGIN_CMD:-}" ] || e2e_die "set E2E_RELOGIN_CMD to a command that restores the session"
e2e_note "logging back in: $relogin"
# shellcheck disable=SC2086 # the command is a word list on purpose
$relogin
if [ "$already" = 0 ]; then
  e2e_connect
  facts >"$E2E_TMP/after"
  if ! diff -u "$E2E_TMP/before" "$E2E_TMP/after" >"$E2E_TMP/diff"; then
    cat "$E2E_TMP/diff" >&2
    e2e_die "something changed while destroy was refusing"
  fi
  e2e_ok "the cluster, the databases' deletion protection and the workloads are identical after the refusal"
fi
