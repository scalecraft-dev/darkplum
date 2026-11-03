# Contributing to darkplum

Thank you for taking the time. This page covers how to run the project
locally, what the checks are, and what a change needs before it is merged.

## Local setup

You need Docker, [kind](https://kind.sigs.k8s.io/), kubectl, Go (the version
in each service's `go.mod`), Node 22 and pnpm 9. Contributors do not use the
operator CLI to run the local stack; `scripts/dev.sh` does, and it only ever
talks to the `kind-darkplum-dev` cluster, never to kubectl's current context.

```bash
cd darkplum
scripts/dev.sh up                 # kind cluster with mocked inference at http://localhost:8080
scripts/dev.sh test               # smoke test
scripts/dev.sh build api          # rebuild one service into the cluster (also gateway, web)
scripts/dev.sh logs               # tail the services
scripts/dev.sh down               # remove the cluster
scripts/dev.sh cli keys list      # the CLI from this checkout, pointed at the kind cluster
```

Port 8080 taken? `DEV_PORT=5174 scripts/dev.sh up`. To exercise Bedrock locally
with your own AWS credentials, `DEV_AWS=1 scripts/dev.sh up` (optionally
`DEV_AWS_PROFILE=<profile>`). For the authenticated part of `test`, set
`DEV_EMAIL` and `DEV_PASSWORD` after creating the first admin. LM Studio on
port 1234 is picked up automatically on Docker Desktop; on Linux kind,
`host.docker.internal` needs the extra host mapping described in
`k8s/overlays/dev/patches/gateway-deployment.yaml`.

The operator CLI builds with the Go toolchain alone. `go build -o darkplum ./cmd`
in `darkplum/services/cli` gives a development build (channel `dev`): it embeds
the checkout's Terraform and manifests, prints a development banner, and
refuses to deploy images unless you give it `--release-images` or
`--unsigned-images` (hidden flags that release binaries reject).
`scripts/dev.sh eks-push --install <name>` builds amd64 images, pushes them to
that install's registry and prints the `--unsigned-images` command; the install
is then marked unsigned until a signed release is deployed over it.

Each service also runs on its own:

```bash
cd darkplum/services/api && go test ./...          # integration tests need TEST_DATABASE_URL (see below)
cd darkplum/services/gateway && go test ./...
cd darkplum/services/web && pnpm install && pnpm run typecheck && pnpm run check:parity && pnpm run check:policy && pnpm run build
```

The api and gateway integration tests run against a real Postgres and skip
silently without it. Point them at any disposable database:

```bash
docker run -d --name darkplum-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=darkplum_test -p 55432:5432 postgres:16-alpine
export TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/darkplum_test?sslmode=disable'
```

Each test creates its own schema and drops it afterwards.

## What CI checks

`.github/workflows/ci.yml` runs on every push and pull request:

- `gofmt`, `go build`, `go vet` and `go test -race` for the gateway, api and
  cli, with the integration tests against a Postgres service container.
- Web: `pnpm install --frozen-lockfile`, `tsc --noEmit`, `pnpm run build`,
  the API-to-UI parity check and the policy truth-table check
  (`pnpm run check:policy`).
- Kubernetes manifests and docs: every kustomize overlay builds, the vLLM
  model template renders, `scripts/dev.sh` passes shellcheck and argument
  validation, the repository holds no instructions to run make, and `go run ./cmd/apidoc`
  must leave `docs/API.md` unchanged so the reference never drifts from the
  route inventory.
- Terraform: `terraform fmt -check`, `terraform validate`, and the offline
  `terraform test` of the Bedrock IAM allowlist module.

The parity check is worth understanding before you touch a route.
`darkplum/docs/api-routes.json` is the one list of every HTTP route the
services serve. Each entry is marked `ui` (a screen calls it) or `machine`
(with a reason). Go tests in both services fail if a route is registered that
the file does not list, or listed but not registered; the web check fails if
the UI calls a route the file does not mark `ui`, or a `ui` route no screen
reaches. When you add or change a route: register it, add it to the JSON,
add it to `services/web/src/lib/routes.ts`, then regenerate the reference
with `go run ./cmd/apidoc` from `darkplum/services/api`.

Database changes are numbered SQL files in
`darkplum/services/api/internal/db/migrations`, applied at api start. Keep
them additive (`IF NOT EXISTS`), because old and new pods overlap during a
rollout.

## Making a change

1. Open an issue first for anything beyond a small fix, so the approach can
   be discussed before the work.
2. Keep a pull request to one change. Say what it does and why in the
   description; the commit message should stand on its own.
3. Add or update tests with the change. A bug fix carries the test that
   would have caught it.
4. Run the checks above locally. CI must be green.
5. Expect review comments. Reviews here are direct and assume most changes
   need adjustment; that is how the code stays trustworthy, not a judgement
   of the contributor.

Some rules the codebase holds that a reviewer will hold you to:

- **Nothing a user types is ever logged or recorded by an administrator
  path.** Conversation content and library bodies are readable only by their
  owner (or, for shared library items, the organisation). Audit rows carry
  metadata only.
- **Refuse by name, never drop silently.** A request field the gateway cannot
  honour is refused with a 400 naming it.
- **Policy decides where a prompt goes, never the client.** No request header
  or body field may widen what a user or key is allowed.
- **Credentials never land in Postgres, logs or responses.** Vendor keys and
  tool credentials live in cluster Secrets.
- **Every refusal has a closed error code**, documented in `docs/API.md`.

## License of contributions

By submitting a contribution you agree that it is licensed under the Apache
License 2.0, the same license as the project (see `LICENSE`), as described
in section 5 of that license. You keep the copyright to your work.
