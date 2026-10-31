# darkplum

Private AI in your own cloud account. One OpenAI-compatible endpoint, a chat
and admin UI, and a library of skills, tools and context, all running inside
your AWS account on infrastructure you already own and audit. Your prompts
never reach darkplum, and the project never receives your data.

**Status: early.** Installs on AWS with the `darkplum` command-line tool (`darkplum init`, then
`darkplum deploy`). GCP is
planned, not built. The application (gateway, api, chat, library, tool
calling) is verified end to end on a local cluster and in CI. The AWS
Terraform (Bedrock, the IAM allowlist, PrivateLink endpoints) is written and
validated but has not yet been exercised on a live account in this build,
and the install times below come from an earlier one. Live vendor tool turns
and a real MCP server are not yet exercised either. Expect breaking changes
before a 1.0.

## What it does

- **Models where you want them.** Open-source models on vLLM in your own
  cluster, Bedrock models under your own AWS account, and, if you hold your
  own contract, Anthropic and OpenAI models through your own key. Each is a
  different data-residency class, and policy decides which a request may use.
- **Inside your account's existing compliance boundary.** Self-hosted and
  Bedrock traffic stays under your AWS account's agreements, keys and audit
  trail; vendor-key models leave it, are off by default, and every request
  records which class answered. Bedrock access is an IAM allowlist of model
  families in one geography, traffic to Bedrock stays on PrivateLink, and the
  database is encrypted, backed up and deletion-protected by default.
- **One endpoint, policy-enforced on every request.** The gateway resolves
  aliases, refuses unknown and disabled models, enforces the organisation's
  residency ladder and each user's or API key's allowlist, and records every
  request, including refusals, with the backend class that answered. A
  routed alias can send a prompt to one of several models by intent, using an
  in-cluster classifier, never a third party.
- **Chat and admin UI.** Streaming chat with server-side history that only its
  owner can read; admin screens for users, API keys, models and vendor keys,
  routing and residency policy, activity, usage and estimated spend
  reconciled against vendor bills.
- **A library that is yours.** Skills (instructions), context (reference
  text) and tools (MCP servers) stored in your own Postgres, private or
  shared across the organisation, attached to any chat on any model, and
  exported again as plain files. Tool calls run through the gateway's
  OpenAI-shaped tool calling on every backend, against servers an
  administrator has allowed (verified against a mock model and a test MCP
  server; live vendor tool turns are not yet exercised).

Unlike a chat UI you point at an API, darkplum ships the cloud
infrastructure, the IAM boundary and the residency policy along with the UI.
Sign-in is local accounts or your identity provider over OIDC
(`darkplum sso configure`).

## Quick start

> **Not yet released.** `brew install scalecraft-dev/tap/darkplum` and the
> release downloads below do not exist until the first signed release is
> published. Until then, build from source (needs Go, as in
> `darkplum/services/cli/go.mod`, and Docker for the images):
>
> ```bash
> git clone https://github.com/scalecraft-dev/darkplum && cd darkplum/darkplum
> (cd services/cli && go build -o ../../darkplum ./cmd)      # a development build
> ./darkplum init --cluster-name demo   # a few questions; then build and push images:
> scripts/dev.sh eks-push --install demo    # prints the exact `darkplum deploy --unsigned-images` line
> ```
>
> A development build trusts no signature and marks the install `image-trust:
> unsigned` until a signed release is deployed over it. See
> [CONTRIBUTING.md](CONTRIBUTING.md).

**On AWS.** You need the AWS CLI (signed in to the account you will install
into), Terraform 1.10 or newer, and kubectl. From a release, nothing else: no
Docker, Go, make, jq or git.

```bash
brew install scalecraft-dev/tap/darkplum
darkplum init          # a few questions about account, region and models
darkplum deploy        # builds the platform in your account
darkplum configure     # first admin, models, a test chat, then the web UI
```

No Homebrew? Download the archive for your platform from the
[releases page](https://github.com/scalecraft-dev/darkplum/releases), check
it, and put `darkplum` on your `PATH` (the `gh` CLI is needed for the
attestation check below; it is optional, but without it you have only the
checksum, which proves the download matches the release, not who built it):

```bash
sha256sum -c checksums.txt --ignore-missing      # shasum -a 256 -c on macOS
gh attestation verify darkplum_<version>_<os>_<arch>.tar.gz --repo scalecraft-dev/darkplum \
  --signer-workflow scalecraft-dev/darkplum/.github/workflows/release.yml
tar -xzf darkplum_<version>_<os>_<arch>.tar.gz darkplum
```

The binaries are not notarised by Apple. Homebrew is the supported path on
macOS; if you download by hand, verify it as above before you run it.

`darkplum init` asks which account, region and model hosting you want, checks
them against your account and records the answers, so there is no file to
edit. Choosing a self-hosted GPU model takes 35 to 50 minutes; choosing Bedrock
only takes about twenty minutes and is the cheapest path to a first chat.
Re-run `darkplum init` to change a choice, then `darkplum deploy` to apply it.
It also asks for a domain you control: your team reaches Darkplum at a private
`https://darkplum.<domain>`, from your VPC and the networks you allow.
`darkplum deploy` is safe to re-run, and it is also how you upgrade (install
the new binary, run it again). `darkplum configure` creates the first admin,
enables models, sends a test chat and opens the web UI on a private local
address; it is safe to re-run.

**Locally, to work on darkplum** (Docker, kind, kubectl, Go): see
[CONTRIBUTING.md](CONTRIBUTING.md). `scripts/dev.sh up` starts a mocked stack
at `http://localhost:8080`.

The full install, configuration, cost, exposure, model and troubleshooting
guide is [darkplum/README.md](darkplum/README.md).

## Commands

| Command | What it does |
|---------|--------------|
| `darkplum init` | Create an install, or show an existing one and change its settings |
| `darkplum deploy` | Create or update the install: infrastructure, verified images, the platform. Also the upgrade path |
| `darkplum configure` | First run: first admin, models, a test chat, open the web UI |
| `darkplum status` / `darkplum doctor check` | Health of the install / whether this machine can run one |
| `darkplum verify` | Prove the gateway's IAM role allows only the Bedrock models you configured |
| `darkplum keys set\|remove\|list` | Anthropic, OpenAI and Hugging Face keys |
| `darkplum models add\|remove\|list\|test` | Self-hosted models |
| `darkplum sso configure`, `darkplum admin create` | Sign-in with your identity provider; a break-glass admin |
| `darkplum kubeconfig` | Point one shell at this install's cluster, without touching `~/.kube` |
| `darkplum version` | The release, the three image digests it deploys and the third-party images it pins |
| `darkplum destroy` | Delete the install (asks you to type the cluster name) |

## What you are trusting

- **The `darkplum` binary is the trust anchor.** It carries the Terraform and
  Kubernetes manifests it applies and the identity (this repository's release
  workflow) that the images it deploys must be signed by. If you run a
  tampered binary, nothing else can protect you, so verify what you download
  (the Homebrew formula pins each archive's SHA-256, and the release workflow
  writes it only after verifying the published archives against their
  attestation; for a manual download, run the commands above).
- **Image signature checks are defence in depth.** Before it copies the
  gateway, api and web images into your registry, by digest, the CLI verifies
  each one against the identity compiled into the binary. That catches a
  swapped image. It does not help against a compromised binary, which chooses
  what to trust.
- **Third-party images are pinned by digest, not mirrored.** The vLLM server,
  the NVIDIA device plugin and, if you turn it on, the semantic router are
  pinned to exact digests in the binary but pulled from their public registries
  (Docker Hub, nvcr.io, ghcr.io) by your cluster. They are not copied into your
  registry and not signature-checked.
- **The EKS API endpoint is public.** It is authenticated by AWS IAM and is
  also reachable privately; this release does not restrict it to a list of
  networks.
- **The vLLM server container runs as root** (its model-download init
  container does not). The pod has no Kubernetes API token, accepts traffic
  from the gateway only, and may connect out only over public HTTPS.
- **The semantic router reads prompt text.** If you turn it on, the pod that
  classifies prompts may connect to any public address on port 443 (private
  ranges stay blocked) because it downloads its models from Hugging Face at
  startup.
- **There is no air-gapped install.** `darkplum deploy` needs these from the
  machine that runs it: `ghcr.io` and `pkg-containers.githubusercontent.com`
  (images and their signatures), `tuf-repo-cdn.sigstore.dev` (signature trust
  root), the Terraform registry and provider downloads, and your AWS account's
  ECR, S3 and STS endpoints. The cluster needs to reach the public registries
  above for the third-party images and `huggingface.co` for model weights (the
  vLLM download and, if enabled, the semantic router).

## Repository layout

| Path | What it is |
|------|------------|
| `darkplum/services/gateway` | Go. The OpenAI-compatible inference endpoint: routing, policy enforcement, provider translation (vLLM, Bedrock, Anthropic, OpenAI), usage recording. |
| `darkplum/services/api` | Go. Authentication, users and keys, model catalogue, routing policy, conversations, the library and tool loop, audit, spend. Owns the Postgres schema. |
| `darkplum/services/web` | React + TypeScript. Chat, Library and admin UI. |
| `darkplum/services/cli` | Go. The `darkplum` operator CLI: init, deploy, configure, keys, models and the rest (see Commands above). Also holds the embedded Terraform and Kubernetes manifests it applies. |
| `darkplum/terraform/aws` | EKS, node groups, RDS, VPC, Bedrock allowlist and PrivateLink endpoints. |
| `darkplum/k8s` | Kustomize base and overlays (local kind, local with AWS credentials, production). |
| `darkplum/docs` | [API reference](darkplum/docs/API.md) generated from the route inventory the services and UI are checked against; vendor API runbook. |

## Documentation

- [Operations guide](darkplum/README.md): install, configure, expose, add
  models, the library, troubleshooting.
- [API reference](darkplum/docs/API.md): every route, its auth, the policy
  bodies and every refusal shape.
- [Terraform](darkplum/terraform/README.md): cloud selection, the output
  contract, database durability.
- [Vendor APIs runbook](darkplum/docs/RUNBOOK-vendor-apis.md).

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md)
for the local setup, the checks CI runs (Go vet and tests against Postgres,
web typecheck, API-to-UI route parity, Terraform validation) and how reviews
work here. Security problems go through
[SECURITY.md](SECURITY.md), not the issue tracker. This project follows the
[Contributor Covenant](CODE_OF_CONDUCT.md).

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
