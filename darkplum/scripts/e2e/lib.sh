# shellcheck shell=bash
# Shared helpers for the release-candidate e2e assertions (docs/E2E-RELEASE-CHECK.md).
# Sourced, never run. Every script that sources this exits non-zero on the first
# failed assertion and says which one.
#
# Rules these scripts keep, on purpose:
#   - Every kubectl call goes through e2e_kubectl, which pins the install's own
#     private kubeconfig (from `darkplum kubeconfig`) and its context. Nothing
#     here ever reads or writes ~/.kube/config or acts on the current context.
#     The one place another cluster is read is a decoy the caller names
#     explicitly (e2e_decoy_kubectl), and only with get.
#   - Everything is read-only except the single `darkplum` action a row requires,
#     and the destructive ones refuse to run without E2E_CONFIRM_DESTROY=<cluster>.
#   - Secret values are hashed, never printed.

set -euo pipefail
export NO_COLOR=1

e2e_die() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
e2e_ok() { printf 'ok:   %s\n' "$*"; }
e2e_note() { printf '      %s\n' "$*"; }

e2e_need() {
  local t
  for t in "$@"; do
    command -v "$t" >/dev/null 2>&1 || e2e_die "$t is required on PATH"
  done
}

# e2e_args_install NAME: the --install value is the one thing every script needs.
e2e_valid_name() { [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,62}$ ]] || e2e_die "install name '$1' is not a valid workspace name"; }

e2e_home() { printf '%s' "${DARKPLUM_HOME:-$HOME/.darkplum}"; }
e2e_record() { printf '%s/installs/%s/install.json' "$(e2e_home)" "$1"; }

# shellcheck disable=SC2034 # the globals below are read by the scripts that source this
# e2e_load_install NAME sets E2E_INSTALL, E2E_CLUSTER, E2E_REGION, E2E_PROFILE and
# E2E_ACCOUNTS from the install's record.
e2e_load_install() {
  E2E_INSTALL="$1"
  e2e_valid_name "$E2E_INSTALL"
  local rec
  rec="$(e2e_record "$E2E_INSTALL")"
  [ -f "$rec" ] || e2e_die "no install record at $rec"
  E2E_CLUSTER="$(jq -r '.cluster_name' "$rec")"
  E2E_REGION="$(jq -r '.region' "$rec")"
  E2E_PROFILE="$(jq -r '.profile // ""' "$rec")"
  E2E_ACCOUNTS="$(jq -r '.account_ids | join(",")' "$rec")"
  [ -n "$E2E_CLUSTER" ] && [ "$E2E_CLUSTER" != null ] || e2e_die "$rec has no cluster_name"
  [ -n "$E2E_REGION" ] && [ "$E2E_REGION" != null ] || e2e_die "$rec has no region"
}

# e2e_dp runs the darkplum under test against this install.
e2e_dp() { darkplum --install "$E2E_INSTALL" "$@"; }

# e2e_aws runs aws as the install's profile and region (read-only callers only).
e2e_aws() {
  if [ -n "$E2E_PROFILE" ]; then
    aws --profile "$E2E_PROFILE" --region "$E2E_REGION" "$@"
  else
    aws --region "$E2E_REGION" "$@"
  fi
}

# e2e_connect sets E2E_KUBECONFIG and E2E_CONTEXT from `darkplum kubeconfig`. The
# export line it prints must point inside this install's workspace, or we stop.
e2e_connect() {
  local out path ws
  out="$(e2e_dp kubeconfig)" || e2e_die "darkplum kubeconfig could not establish the install's cluster"
  case "$out" in
    "export KUBECONFIG='"*"'") ;;
    *) e2e_die "unexpected output from darkplum kubeconfig: $out" ;;
  esac
  path="${out#export KUBECONFIG=\'}"
  path="${path%\'}"
  ws="$(e2e_home)/installs/$E2E_INSTALL/"
  case "$path" in
    "$ws"*) ;;
    *) e2e_die "darkplum kubeconfig points outside the install's workspace ($path)" ;;
  esac
  [ -f "$path" ] || e2e_die "kubeconfig $path does not exist"
  E2E_KUBECONFIG="$path"
  E2E_CONTEXT="$(kubectl --kubeconfig "$E2E_KUBECONFIG" config current-context)"
  [ -n "$E2E_CONTEXT" ] || e2e_die "the install's kubeconfig has no current context"
}

e2e_kubectl() {
  [ -n "${E2E_KUBECONFIG:-}" ] || e2e_die "e2e_connect was not called: refusing to run kubectl without the install's kubeconfig"
  kubectl --kubeconfig "$E2E_KUBECONFIG" --context "$E2E_CONTEXT" "$@"
}

# e2e_decoy_kubectl is for a cluster the caller names with --decoy-context. Reads only.
e2e_decoy_kubectl() {
  [ -n "${E2E_DECOY_KUBECONFIG:-}" ] && [ -n "${E2E_DECOY_CONTEXT:-}" ] || e2e_die "no decoy context named"
  case "${1:-}" in
    get | config) ;;
    *) e2e_die "the decoy cluster is read-only here (got: ${1:-nothing})" ;;
  esac
  kubectl --kubeconfig "$E2E_DECOY_KUBECONFIG" --context "$E2E_DECOY_CONTEXT" "$@"
}

e2e_sha() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi
}

# e2e_require_destroy_confirmation: destructive rows run only when the operator
# restates the cluster name in the environment.
e2e_require_destroy_confirmation() {
  [ "${E2E_CONFIRM_DESTROY:-}" = "$E2E_CLUSTER" ] ||
    e2e_die "this script runs 'darkplum destroy' on cluster '$E2E_CLUSTER'. Set E2E_CONFIRM_DESTROY=$E2E_CLUSTER to allow it"
}

# e2e_strip removes terminal escapes so greps match what a person reads.
e2e_strip() { sed -e $'s/\x1b\\[[0-9;]*[A-Za-z]//g'; }

# e2e_expect_in FILE PATTERN WHY: the output must contain the fixed string.
e2e_expect_in() {
  grep -qF -- "$2" "$1" || { cat "$1" >&2; e2e_die "$3 (expected the output to say: $2)"; }
}
e2e_expect_not_in() {
  if grep -qF -- "$2" "$1"; then cat "$1" >&2; e2e_die "$3 (the output must not say: $2)"; fi
}

e2e_tmpdir() {
  E2E_TMP="$(mktemp -d "${TMPDIR:-/tmp}/darkplum-e2e.XXXXXX")"
  chmod 700 "$E2E_TMP"
  trap 'rm -rf "$E2E_TMP"' EXIT
}

# e2e_newest_log DIR PREFIX prints the name of the newest DIR/PREFIX* file, or nothing.
e2e_newest_log() {
  local f newest=""
  for f in "$1"/"$2"*; do
    [ -e "$f" ] || continue
    if [ -z "$newest" ] || [[ "$f" > "$newest" ]]; then newest="$f"; fi
  done
  [ -z "$newest" ] || basename "$newest"
}
