#!/usr/bin/env bash
# Local development stack (kind) for contributors. Operators never need this:
# they use the `darkplum` CLI (init, deploy, configure).
#
#   scripts/dev.sh up                 create the kind cluster and deploy everything
#   scripts/dev.sh down               delete the kind cluster
#   scripts/dev.sh build gateway|api|web
#                                     rebuild one service and roll it into the cluster
#   scripts/dev.sh status | logs | test
#   scripts/dev.sh cli <args>         run the CLI from this checkout against the kind cluster
#   scripts/dev.sh eks-push --install NAME
#                                     build amd64 images, push them to that install's ECR
#                                     and print the `darkplum deploy --unsigned-images` command
#
# Every kubectl call names --context kind-darkplum-dev explicitly. Nothing here
# ever acts on kubectl's current context, which may be a production cluster.
#
# Environment:
#   DEV_PORT=8080            host port the stack is published on
#   DEV_AWS=1                mount your AWS credential files (read-only) into the dev gateway and
#                            enable Bedrock; off by default so no credentials leave the host
#   DEV_AWS_PROFILE=default  the AWS profile the dev gateway uses when DEV_AWS is set
#   DEV_EMAIL, DEV_PASSWORD  let `test` also run the authenticated chat checks
#   MODEL=<id>               the model `test` chats with (default: the first enabled one)
set -euo pipefail

DEV_CLUSTER=darkplum-dev
DEV_CONTEXT="kind-${DEV_CLUSTER}"
NS=darkplum-system
DEV_PORT="${DEV_PORT:-8080}"
DEV_AWS="${DEV_AWS:-}"
DEV_AWS_PROFILE="${DEV_AWS_PROFILE:-default}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

die() { echo "ERROR: $*" >&2; exit 1; }

usage() {
  sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -e '$d' -e 's/^# \{0,1\}//'
}

# Always the kind cluster, never the ambient context.
kc() { kubectl --context "$DEV_CONTEXT" "$@"; }

need() {
  case "$1" in
    kind) command -v kind >/dev/null 2>&1 || die "kind not found. Install: https://kind.sigs.k8s.io/docs/user/quick-start/#installation" ;;
    *) command -v "$1" >/dev/null 2>&1 || die "$1 not found." ;;
  esac
}

# DEV_PORT and DEV_AWS_PROFILE are substituted into YAML, so they are checked, not trusted.
validate_env() {
  if ! [[ "$DEV_PORT" =~ ^[0-9]{1,5}$ ]] || (( 10#$DEV_PORT < 1 || 10#$DEV_PORT > 65535 )); then
    die "DEV_PORT must be a number from 1 to 65535, got '$DEV_PORT'."
  fi
  DEV_PORT=$((10#$DEV_PORT)) # "08" would reach the kind config as a YAML octal
  [[ "$DEV_AWS_PROFILE" =~ ^[A-Za-z0-9_.@-]+$ ]] \
    || die "DEV_AWS_PROFILE may contain only letters, digits and _ . @ -, got '$DEV_AWS_PROFILE'."
}

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$DEV_CLUSTER"; }

cmd_up() {
  validate_env
  need kind; need kubectl; need docker
  if cluster_exists; then
    echo "Cluster $DEV_CLUSTER already exists. Run 'scripts/dev.sh down' first to recreate." >&2
    exit 1
  fi
  if command -v lsof >/dev/null 2>&1 && lsof -nP -iTCP:"$DEV_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    die "port $DEV_PORT is already in use on this machine. Pick another: DEV_PORT=5174 scripts/dev.sh up"
  fi

  echo "Creating kind cluster (http://localhost:$DEV_PORT)..."
  local cfg overlay=dev rc=0
  cfg="$(mktemp)"
  sed "s|hostPort: 8080|hostPort: $DEV_PORT|" k8s/overlays/dev/kind-config.yaml > "$cfg"
  if [ -n "$DEV_AWS" ]; then
    overlay=dev-aws
    echo "DEV_AWS: mounting AWS credential files (read-only) for profile '$DEV_AWS_PROFILE'"
    echo "    extraMounts:" >> "$cfg"
    local found=0 f
    for f in config credentials sso; do
      if [ -e "$HOME/.aws/$f" ]; then
        found=1
        printf '      - hostPath: %s\n        containerPath: /host-users/.aws/%s\n        readOnly: true\n' "$HOME/.aws/$f" "$f" >> "$cfg"
      fi
    done
    if [ "$found" -ne 1 ]; then
      rm -f "$cfg"
      die "DEV_AWS is set but there is no ~/.aws/config, credentials or sso."
    fi
  fi
  kind create cluster --name "$DEV_CLUSTER" --config "$cfg" || rc=$?
  rm -f "$cfg"
  [ "$rc" -eq 0 ] || exit "$rc"

  local svc
  for svc in gateway api web; do
    echo "Building $svc image..."
    docker build -t "darkplum-$svc:latest" "services/$svc/"
  done
  echo "Loading images into kind..."
  kind load docker-image --name "$DEV_CLUSTER" darkplum-gateway:latest darkplum-api:latest darkplum-web:latest

  echo "Deploying all services ($overlay overlay)..."
  kc kustomize "k8s/overlays/$overlay" \
    | sed -e "s|http://localhost:8080|http://localhost:$DEV_PORT|g" \
          -e "s|PLACEHOLDER_AWS_PROFILE|$DEV_AWS_PROFILE|g" \
    | kc apply -f -

  echo "Waiting for services to be ready..."
  sleep 5
  kc wait --namespace="$NS" --for=condition=ready pod -l app.kubernetes.io/name=postgres --timeout=60s
  kc wait --namespace="$NS" --for=condition=ready pod -l app.kubernetes.io/name=darkplum-gateway --timeout=90s
  kc wait --namespace="$NS" --for=condition=ready pod -l app.kubernetes.io/name=darkplum-api --timeout=90s
  kc wait --namespace="$NS" --for=condition=ready pod -l app.kubernetes.io/name=darkplum-web --timeout=60s

  cat <<MSG

=========================================
  Local dev cluster ready.
  http://localhost:$DEV_PORT
=========================================

First run: create an admin account at http://localhost:$DEV_PORT,
then enable a model under Admin > Models (new models start disabled).
LLM responses are mocked. LM Studio on :1234 is picked up automatically if running.

Useful commands:
  scripts/dev.sh status   Show pod status
  scripts/dev.sh logs     Tail service logs
  scripts/dev.sh test     Smoke-test the stack (DEV_EMAIL/DEV_PASSWORD for the full chat test)
MSG
}

cmd_down() {
  need kind
  kind delete cluster --name "$DEV_CLUSTER" 2>/dev/null || true
  echo "Local dev cluster deleted."
}

cmd_build() {
  local svc="${1:-}"
  case "$svc" in
    gateway|api|web) ;;
    *) die "usage: scripts/dev.sh build gateway|api|web" ;;
  esac
  [ "$#" -eq 1 ] || die "usage: scripts/dev.sh build gateway|api|web"
  need kind; need kubectl; need docker
  docker build -t "darkplum-$svc:latest" "services/$svc/"
  kind load docker-image --name "$DEV_CLUSTER" "darkplum-$svc:latest"
  kc rollout restart "deployment/darkplum-$svc" -n "$NS"
  kc rollout status "deployment/darkplum-$svc" -n "$NS" --timeout=60s
}

cmd_status() {
  need kubectl
  echo "=== Pods ==="
  kc get pods -n "$NS" -o wide 2>/dev/null || echo "Cluster not running. Run: scripts/dev.sh up"
  echo ""
  echo "=== Services ==="
  kc get svc -n "$NS" 2>/dev/null || true
}

cmd_logs() {
  need kubectl
  local pair title svc
  for pair in "Gateway:gateway" "API:api" "Web:web"; do
    title="${pair%%:*}"; svc="${pair##*:}"
    echo "=== $title (last 20) ==="
    kc logs -n "$NS" "deploy/darkplum-$svc" --tail=20 2>/dev/null || true
    echo ""
  done
}

cmd_test() {
  validate_env
  need curl; need jq
  local base="http://localhost:$DEV_PORT" code setup m resp body errcode jar
  local h1='Content-Type: application/json' h2='X-Requested-With: XMLHttpRequest'
  fail() { echo "  FAIL: $1"; exit 1; }
  echo "Testing $base ..."
  code=$(curl -s -o /dev/null -w "%{http_code}" "$base/health" || true)
  [ "$code" = "200" ] || fail "/health returned HTTP $code (is the cluster running on DEV_PORT=$DEV_PORT?)"
  echo "  /health OK"
  code=$(curl -s -o /dev/null -w "%{http_code}" "$base/v1/chat/completions" -H "$h1" -H "$h2" \
    -d '{"model":"mock-model","messages":[{"role":"user","content":"Hello"}]}')
  [ "$code" = "401" ] || fail "unauthenticated chat returned HTTP $code, expected 401"
  echo "  unauthenticated chat is refused (401) OK"
  setup=$(curl -s "$base/auth/config" | jq -r '.setup_required')
  if [ "$setup" = "true" ]; then
    echo "  First-run setup not done yet: create your admin at $base, then re-run with DEV_EMAIL/DEV_PASSWORD."
    return 0
  fi
  if [ -z "${DEV_EMAIL:-}" ] || [ -z "${DEV_PASSWORD:-}" ]; then
    echo "  Skipping authenticated chat test. Run: DEV_EMAIL=you@example.com DEV_PASSWORD=... scripts/dev.sh test"
    return 0
  fi
  jar="$(mktemp)"
  trap 'rm -f "$jar"' RETURN
  # The password goes to curl on stdin, never argv.
  code=$(jq -n --arg e "$DEV_EMAIL" --arg p "$DEV_PASSWORD" '{email:$e,password:$p}' \
    | curl -s -o /dev/null -w "%{http_code}" -c "$jar" "$base/auth/login" -H "$h1" -H "$h2" -d @-)
  [ "$code" = "200" ] || fail "login returned HTTP $code"
  echo "  login OK"
  m="${MODEL:-}"
  [ -n "$m" ] || m=$(curl -s -b "$jar" "$base/v1/models" -H "$h2" | jq -r '.data[0].id // empty')
  [ -n "$m" ] || fail "no models are enabled. Enable one under Admin > Models."
  resp=$(jq -n --arg m "$m" '{model:$m,messages:[{role:"user",content:"Say hello in one sentence."}],max_tokens:50}' \
    | curl -s -w "\n%{http_code}" -b "$jar" "$base/v1/chat/completions" -H "$h1" -H "$h2" -d @-)
  code=$(echo "$resp" | tail -1); body=$(echo "$resp" | sed '$d')
  [ "$code" = "200" ] || fail "chat with '$m' returned HTTP $code: $body"
  echo "  chat with '$m' OK: $(echo "$body" | jq -r '.choices[0].message.content // empty' | head -c 120)"
  resp=$(curl -s -w "\n%{http_code}" -b "$jar" "$base/v1/chat/completions" -H "$h1" -H "$h2" \
    -d '{"model":"darkplum-no-such-model","messages":[{"role":"user","content":"x"}]}')
  code=$(echo "$resp" | tail -1); errcode=$(echo "$resp" | sed '$d' | jq -r '.error.code // empty')
  { [ "$code" = "404" ] && [ "$errcode" = "model_not_found" ]; } \
    || fail "unknown model returned HTTP $code code='$errcode', expected 404 model_not_found"
  echo "  unknown model is refused (404 model_not_found) OK"
  echo "All checks passed."
}

# The CLI from this checkout, pointed at the kind cluster. For keys and models
# against the local stack: scripts/dev.sh cli keys set anthropic
cmd_cli() {
  [ "$#" -gt 0 ] || die "usage: scripts/dev.sh cli <darkplum command and flags>"
  need go
  (cd services/cli && go run ./cmd "$@" --context "$DEV_CONTEXT")
}

cmd_eks_push() {
  local name=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --install) [ "$#" -ge 2 ] || die "--install needs a name"; name="$2"; shift 2 ;;
      --install=*) name="${1#--install=}"; shift ;;
      *) die "unknown argument '$1'. usage: scripts/dev.sh eks-push --install NAME" ;;
    esac
  done
  [ -n "$name" ] || die "usage: scripts/dev.sh eks-push --install NAME"
  [[ "$name" =~ ^[a-z][a-z0-9-]{0,38}[a-z0-9]$ ]] || die "'$name' is not a valid install name."
  need docker; need aws; need jq

  local home="${DARKPLUM_HOME:-$HOME/.darkplum}" rec cluster region profile
  rec="$home/installs/$name/install.json"
  [ -f "$rec" ] || die "no install '$name' (looked for $rec). Run 'darkplum init' first."
  cluster=$(jq -r '.cluster_name // empty' "$rec")
  region=$(jq -r '.region // empty' "$rec")
  profile=$(jq -r '.profile // empty' "$rec")
  [ -n "$cluster" ] && [ -n "$region" ] || die "$rec has no cluster_name/region."

  local -a awsargs=(--region "$region")
  [ -z "$profile" ] || awsargs+=(--profile "$profile")

  local svc uri host sha tag digest
  sha=$(git rev-parse --short=12 HEAD 2>/dev/null || echo nogit)
  tag="dev-$sha-$(date +%s)"
  local -a refs=()
  local login
  login="$(mktemp -d)"
  trap 'rm -rf "$login"' EXIT
  for svc in gateway api web; do
    uri=$(aws ecr describe-repositories "${awsargs[@]}" --repository-names "$cluster/darkplum-$svc" \
      --query 'repositories[0].repositoryUri' --output text) \
      || die "cannot read ECR repository $cluster/darkplum-$svc. Has 'darkplum deploy' created the infrastructure?"
    host="${uri%%/*}"
    echo "Building $svc (linux/amd64)..."
    docker build --platform linux/amd64 -t "$uri:$tag" "services/$svc/"
    # A throwaway docker config: the ECR login token is not left in ~/.docker.
    aws ecr get-login-password "${awsargs[@]}" \
      | DOCKER_CONFIG="$login" docker login --username AWS --password-stdin "$host" >/dev/null
    echo "Pushing $uri:$tag ..."
    DOCKER_CONFIG="$login" docker push "$uri:$tag" >/dev/null
    digest=$(aws ecr describe-images "${awsargs[@]}" --repository-name "$cluster/darkplum-$svc" \
      --image-ids "imageTag=$tag" --query 'imageDetails[0].imageDigest' --output text)
    [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || die "unexpected digest '$digest' for $svc."
    refs+=("$svc=$uri@$digest")
  done
  local IFS=,
  cat <<MSG

Pushed. Deploy these UNSIGNED images with a development build of the CLI
(go build -o darkplum ./services/cli/cmd from darkplum/):

  darkplum deploy --install $name --unsigned-images ${refs[*]}

status and doctor will show the install as image-trust: unsigned until a signed release is deployed.
MSG
}

main() {
  local cmd="${1:-help}"
  [ "$#" -eq 0 ] || shift
  case "$cmd" in
    up) [ "$#" -eq 0 ] || die "up takes no arguments (use DEV_PORT / DEV_AWS)"; cmd_up ;;
    down) [ "$#" -eq 0 ] || die "down takes no arguments"; cmd_down ;;
    build) cmd_build "$@" ;;
    status) cmd_status ;;
    logs) cmd_logs ;;
    test) cmd_test ;;
    cli) cmd_cli "$@" ;;
    eks-push) cmd_eks_push "$@" ;;
    help|-h|--help) usage ;;
    *) usage >&2; echo >&2; die "unknown command '$cmd'" ;;
  esac
}

main "$@"
