# darkplum

**The private AI cloud for your AWS account.**

A chat UI for your whole company and an OpenAI-compatible API for your
engineers, running on infrastructure you already own and audit. Not just a UI:
darkplum ships the cloud infrastructure, the IAM boundary and the residency
policies. Prompts, chat history, skills, tools, and shared context all stay in your account
unless you turn on a vendor-key model.

Early release, AWS only. Read [Status](#status) before you install.

## Contents

- [What it does](#what-it-does)
- [Status](#status)
- [Quick start](#quick-start)
- [Documentation](#documentation)
- [Contributing](#contributing)
- [License](#license)

## What it does

- **Models where you want them.** Open-source models on vLLM in your cluster,
  Bedrock models under your AWS account, and, with your own contract, Anthropic
  and OpenAI models through your own key. Each is a different data-residency
  class, and policy decides which a request may use. Self-hosted and Bedrock
  traffic stays inside your AWS account's agreements, keys and audit trail.
  Vendor-key models leave it, are off by default, and every request records
  which class answered.
- **One endpoint, policy on every request.** The gateway enforces the
  organisation's residency ladder and each user's or API key's allowlist, and
  records every request, including refusals. Optional intent routing runs
  in-cluster, never through a third party.
- **Chat and admin UI.** Streaming chat with server-side history only its
  owner can read. Admin screens for users, keys, models, policy, usage and
  spend. Sign-in with local accounts or your identity provider over OIDC.
- **A library that is yours.** Skills (instructions), context (reference text)
  and tools (MCP servers) in your own Postgres, private or shared, attached to
  any chat on any model, and exportable as plain files. Tools work on every
  model, against MCP servers an admin has allowed (see [Status](#status)).

## Status

**v0.1.0, early.** 

| Area | State | Notes |
|------|-------|-------|
| AWS install: Terraform for EKS, Bedrock, the IAM allowlist, PrivateLink | Validated, not live-run | Written and validated; not exercised on a live account in this build |
| Gateway, API, chat, library, tool calling | Verified in CI | End to end on a local cluster, against a mock model and a test MCP server |
| Live vendor tool turns, real MCP servers | Not exercised | |
| GCP | Planned | Not built |

## Quick start

**Prerequisites:** the AWS CLI signed in to the account you will install into,
Terraform 1.10 or newer, and kubectl. From a release, nothing else: no Docker,
Go, make, jq or git.

```bash
brew install scalecraft-dev/tap/darkplum
darkplum init
darkplum deploy
darkplum configure
```

| Step | What it does |
|------|--------------|
| `darkplum init` | Asks for account, region, model hosting and a domain you control, checks them against your account, and records the answers. Re-run to change a choice, then `darkplum deploy` to apply it. |
| `darkplum deploy` | Builds the platform. Safe to re-run, and also the upgrade path: install the new binary, run it again. |
| `darkplum configure` | Creates the first admin, enables models, sends a test chat and opens the web UI on a private local address. Safe to re-run. |

About twenty minutes to a first chat with Bedrock only, which is also the
cheapest path. About thirty with a self-hosted GPU model, longer for a larger
one. Both times are from an earlier build. Your team then reaches darkplum at
a private `https://darkplum.<domain>` from your VPC and the networks you
allow.

Homebrew is the supported path on macOS. On Linux, or to download by hand,
follow [Install the CLI](darkplum/README.md#install-the-cli) in the operations
guide and verify the archive before you run it.

<details>
<summary><strong>All commands</strong></summary>

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

</details>

The full install, configuration, cost, exposure, model and troubleshooting
guide is the [operations guide](darkplum/README.md).

## Documentation

| Guide | Covers |
|-------|--------|
| [Operations guide](darkplum/README.md) | Install, configure, expose, add models, the library, troubleshooting |
| [API reference](darkplum/docs/API.md) | Every route, its auth, the policy bodies and every refusal shape |
| [Terraform](darkplum/terraform/README.md) | Cloud selection, the output contract, database durability |
| [Vendor APIs runbook](darkplum/docs/RUNBOOK-vendor-apis.md) | Turning on, rotating, removing and rolling back Anthropic and OpenAI keys |

## Contributing

Issues and pull requests are welcome. [CONTRIBUTING.md](CONTRIBUTING.md)
covers local setup (Docker, kind, kubectl, Go), the checks CI runs (Go vet and
tests against Postgres, web typecheck, API-to-UI route parity, Terraform
validation) and how reviews work. `darkplum/scripts/dev.sh up` starts a mocked
stack at `http://localhost:8080`.

Report security problems through [SECURITY.md](SECURITY.md), not the issue
tracker. This project follows the
[Contributor Covenant](CODE_OF_CONDUCT.md).

<details>
<summary><strong>Repository layout</strong></summary>

| Path | What it is |
|------|------------|
| `darkplum/services/gateway` | Go. OpenAI-compatible inference endpoint: routing, policy, provider translation (vLLM, Bedrock, Anthropic, OpenAI), usage recording |
| `darkplum/services/api` | Go. Auth, users and keys, model catalogue, routing policy, conversations, library and tool loop, audit, spend. Owns the Postgres schema |
| `darkplum/services/web` | React + TypeScript. Chat, Library and admin UI |
| `darkplum/services/cli` | Go. The `darkplum` operator CLI and the embedded Terraform and Kubernetes manifests it applies |
| `darkplum/terraform/aws` | EKS, node groups, RDS, VPC, Bedrock allowlist and PrivateLink endpoints |
| `darkplum/k8s` | Kustomize base and overlays (local kind, local with AWS credentials, production) |
| `darkplum/docs` | [API reference](darkplum/docs/API.md) generated from the route inventory; vendor API runbook |

</details>

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
