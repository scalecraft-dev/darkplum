#!/usr/bin/env bash
# Proves the e2e assertion scripts fail when they should, against stand-ins for
# darkplum, kubectl and aws (selftest/). It tests the scripts, not Darkplum.
# Run by CI; needs bash, python3 and jq.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fail=0
# check NAME zero -- command...          must exit 0
# check NAME nonzero REASON -- command...  must exit non-zero AND say REASON (so a
#                                          script failing for a different reason is caught)
check() {
  local name="$1" want="$2" reason="" rc=0
  if [ "$want" = nonzero ]; then reason="$3"; shift; fi
  shift 3
  "$@" >"$tmp/out" 2>&1 || rc=$?
  local bad=0
  if [ "$want" = zero ] && [ "$rc" != 0 ]; then bad=1; fi
  if [ "$want" = nonzero ] && [ "$rc" = 0 ]; then bad=1; fi
  if [ "$want" = nonzero ] && ! grep -qF -- "$reason" "$tmp/out"; then bad=1; reason="(output lacks: $reason)"; fi
  if [ "$bad" = 1 ]; then
    echo "SELFTEST FAIL: $name (exit $rc, wanted $want)"; sed 's/^/    /' "$tmp/out"; fail=1
  else
    echo "selftest ok: $name"
  fi
}

tmp="$(mktemp -d "${TMPDIR:-/tmp}/e2e-selftest.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
export FAKE_STATE="$tmp/state" DARKPLUM_HOME="$tmp/home" HOME="$tmp/userhome"
export PATH="$tmp/bin:$PATH"
mkdir -p "$tmp/bin" "$HOME/.kube"
cp "$here/selftest/darkplum" "$here/selftest/aws" "$tmp/bin/"
printf '#!/bin/sh\nexec python3 "%s/selftest/kubectl.py" "$@"\n' "$here" >"$tmp/bin/kubectl"
chmod +x "$tmp/bin/"*

reset() {
  rm -rf "$FAKE_STATE" "$DARKPLUM_HOME"
  mkdir -p "$FAKE_STATE/install" "$FAKE_STATE/decoy" "$DARKPLUM_HOME/installs/t"
  printf 'current-context: darkplum\nserver: https://install.example:443\n' >"$DARKPLUM_HOME/installs/t/kubeconfig"
  printf 'current-context: prod-ctx\ncontexts: prod-ctx decoy-ctx\nserver: https://decoy.example:6443\n' >"$HOME/.kube/decoy-config"
  printf '{"schema":1,"name":"t","cluster_name":"acme","region":"us-east-2","account_ids":["123456789012"]}\n' >"$DARKPLUM_HOME/installs/t/install.json"
  echo '{"data":{"SESSION_SIGNING_KEY":"a2V5","OIDC_ISSUER":"dW5jb25maWd1cmVk"}}' >"$FAKE_STATE/install/secret-darkplum-auth.json"
  echo '{"data":{"user-hmac-key":"aA==","user-hmac-key-id":"MQ=="}}' >"$FAKE_STATE/install/secret-darkplum-user-hmac.json"
  echo '{"items":[{"metadata":{"name":"api-1","uid":"u1"},"status":{"containerStatuses":[{"restartCount":0,"imageID":"sha256:aa"}]}}]}' >"$FAKE_STATE/install/pods.json"
  echo '{"items":[{"metadata":{"name":"darkplum-api","generation":3}}]}' >"$FAKE_STATE/install/deploys.json"
  echo '{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"VLLM_UPSTREAMS","value":"qwen=http://vllm:8000"}]}]}}}}' >"$FAKE_STATE/install/deploy-darkplum-gateway.json"
  echo '{"items":[{"kind":"Namespace","metadata":{"name":"prod","uid":"d1"}},{"kind":"Deployment","metadata":{"namespace":"prod","name":"dagctl","uid":"d2"}}]}' >"$FAKE_STATE/install/objects.json"
  cp "$FAKE_STATE/install/objects.json" "$FAKE_STATE/decoy/objects.json"
}

echo "-- noop-rerun.sh"
reset; check "no-op rerun passes" zero -- "$here/noop-rerun.sh" --install t --expect-vllm
reset; FAKE_DEPLOY_MUTATE=1 check "a changed secret fails" nonzero "changed something" -- "$here/noop-rerun.sh" --install t
reset; FAKE_DEPLOY_NOTEXT=1 check "missing 'no infrastructure changes' fails" nonzero "must find nothing to change" -- "$here/noop-rerun.sh" --install t
reset; FAKE_DEPLOY_RC=1 check "a failed deploy fails" nonzero "failed with exit 1" -- "$here/noop-rerun.sh" --install t
reset; echo '{"spec":{"template":{"spec":{"containers":[{"env":[]}]}}}}' >"$FAKE_STATE/install/deploy-darkplum-gateway.json"
check "--expect-vllm without VLLM_UPSTREAMS fails" nonzero "no VLLM_UPSTREAMS" -- "$here/noop-rerun.sh" --install t --expect-vllm
reset; check "--expect-oidc with unconfigured OIDC fails" nonzero "OIDC is not configured" -- "$here/noop-rerun.sh" --install t --expect-oidc
reset; check "a path as install name fails" nonzero "not a valid workspace name" -- "$here/noop-rerun.sh" --install ../t

echo "-- destroy scripts refuse without E2E_CONFIRM_DESTROY"
reset; check "decoy script" nonzero "E2E_CONFIRM_DESTROY=acme" -- "$here/destroy-decoy-context.sh" --install t --decoy-context decoy-ctx --decoy-kubeconfig "$HOME/.kube/decoy-config"
reset; check "cluster-gone script" nonzero "E2E_CONFIRM_DESTROY=acme" -- "$here/destroy-cluster-gone.sh" --install t
reset; check "account-mismatch script" nonzero "E2E_CONFIRM_DESTROY=acme" -- "$here/destroy-account-mismatch.sh" --install t
reset; check "expired-session script" nonzero "E2E_CONFIRM_DESTROY=acme" -- "$here/destroy-expired-session.sh" --install t --already-expired

echo "-- destroy-decoy-context.sh"
export E2E_CONFIRM_DESTROY=acme
args=(--install t --decoy-context decoy-ctx --decoy-kubeconfig "$HOME/.kube/decoy-config")
reset; check "an untouched decoy passes" zero -- "$here/destroy-decoy-context.sh" "${args[@]}"
reset; FAKE_TOUCH_DECOY=1 check "a decoy that changed fails" nonzero "DECOY CLUSTER CHANGED" -- "$here/destroy-decoy-context.sh" "${args[@]}"
reset; FAKE_LOG_DECOY=1 check "a log that names the decoy fails" nonzero "names the decoy" -- "$here/destroy-decoy-context.sh" "${args[@]}"
reset; check "the install's own context as decoy fails" nonzero "is not in" -- "$here/destroy-decoy-context.sh" --install t --decoy-context darkplum --decoy-kubeconfig "$DARKPLUM_HOME/installs/t/kubeconfig"
reset; check "the current-context of the user's kubeconfig is never changed" zero -- bash -c "
  before=\$(cat '$HOME/.kube/decoy-config'); '$here/destroy-decoy-context.sh' ${args[*]} >/dev/null; [ \"\$before\" = \"\$(cat '$HOME/.kube/decoy-config')\" ]"

echo "-- destroy-account-mismatch.sh"
reset; check "a refused destroy passes" zero -- "$here/destroy-account-mismatch.sh" --install t
reset; FAKE_MISMATCH_IGNORED=1 check "a destroy that proceeds on the wrong account fails" nonzero "destroy exited 0 against the wrong account" -- "$here/destroy-account-mismatch.sh" --install t
reset; check "the real record is not edited" zero -- bash -c "
  '$here/destroy-account-mismatch.sh' --install t >/dev/null; jq -e '.account_ids == [\"123456789012\"]' '$DARKPLUM_HOME/installs/t/install.json' >/dev/null"
unset E2E_CONFIRM_DESTROY

echo "-- every kubectl call is pinned"
# Any kubectl word in a script other than the pinned helpers' own definitions is a bug.
bare_kubectl() {
  ! grep -nE '(^|[^_a-z-])kubectl ' "$here"/*.sh |
    grep -vE 'e2e_need|^[^:]*:[0-9]+:[[:space:]]*#|kubectl --kubeconfig|kubectl is required|refusing to run kubectl|"kubectl"' |
    grep -v '/selftest\.sh:' | grep .
}
check "no bare kubectl outside the pinned helpers" zero -- bare_kubectl

[ "$fail" = 0 ] || { echo "selftest FAILED"; exit 1; }
echo "selftest passed"
