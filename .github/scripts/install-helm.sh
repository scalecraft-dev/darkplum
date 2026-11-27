#!/usr/bin/env bash
# Installs the one helm that renders the semantic router chart in a workflow, at a
# pinned version and checksum, and puts it first on PATH for the job's later steps.
#
# Every workflow that runs render-semantic-router.sh runs this first (a test in
# test_plumbing.py holds them to it). The runner image's own helm is not used:
# ubuntu-latest moves to a new Ubuntu release without notice and its helm with it,
# and the committed rendering is compared byte for byte.
#
# To move to another helm: change both values below, run
# render-semantic-router.sh with that helm, and commit the rendering it writes.
set -euo pipefail

HELM_VERSION=v3.22.1
HELM_SHA256=896cf63684a42a723d61c980369d6e588330927deb13676ca0d85006dd6da0ea # helm-v3.22.1-linux-amd64.tar.gz

: "${RUNNER_TEMP:?install-helm.sh runs inside a GitHub Actions job}"
: "${GITHUB_PATH:?install-helm.sh runs inside a GitHub Actions job}"
[ "$(uname -s)-$(uname -m)" = "Linux-x86_64" ] || { echo "install-helm.sh pins the linux/amd64 helm; this runner is $(uname -s)/$(uname -m)"; exit 1; }

tarball="helm-${HELM_VERSION}-linux-amd64.tar.gz"
curl -fsSL --retry 3 --retry-all-errors -o "$RUNNER_TEMP/$tarball" "https://get.helm.sh/$tarball"
echo "${HELM_SHA256}  $RUNNER_TEMP/$tarball" | sha256sum -c -
mkdir -p "$RUNNER_TEMP/helm"
tar -xzf "$RUNNER_TEMP/$tarball" -C "$RUNNER_TEMP/helm" --strip-components=1 linux-amd64/helm
found="$("$RUNNER_TEMP/helm/helm" version --template '{{.Version}}')"
[ "$found" = "$HELM_VERSION" ] || { echo "installed helm reports $found, expected $HELM_VERSION"; exit 1; }
echo "$RUNNER_TEMP/helm" >> "$GITHUB_PATH"
echo "helm $HELM_VERSION installed"
