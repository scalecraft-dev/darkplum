# darkplum

Private AI in your own cloud account. One OpenAI-compatible endpoint, a chat
and admin UI, and a library of skills, tools and context, all running inside
your AWS account on infrastructure you already own and audit. Your prompts
never reach darkplum, and the project never receives your data.

**Status: early.** Installs on AWS with Terraform and `make deploy`. GCP is
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
(`make configure-sso`).

## Quick start

**Locally, with no cloud account** (Docker, kind, kubectl):

```bash
cd darkplum
make dev-up            # local kind cluster, mocked inference, http://localhost:8080
```

Create the first admin at the printed address, enable a model under Admin >
Models, and chat. Replies are mocked; LM Studio on port 1234 is picked up
automatically on Docker Desktop (on Linux kind, see
`k8s/overlays/dev/patches/gateway-deployment.yaml`).

**On AWS** (AWS CLI, Terraform 1.9+, kubectl, jq):

```bash
cd darkplum
make cli && ./bin/darkplum doctor check --cloud aws --profile <your-profile>
cp terraform/aws/terraform.tfvars.example terraform/aws/terraform.tfvars   # edit: region, profile, gpu_desired_size
make deploy
```

The default adds one GPU node for a self-hosted model and takes 35 to 50
minutes. Set `gpu_desired_size = 0` in `terraform.tfvars` for a Bedrock-only
install, about twenty minutes and the cheapest path to a first chat.

The full install, configuration, cost, exposure, model and troubleshooting
guide is [darkplum/README.md](darkplum/README.md).

## Repository layout

| Path | What it is |
|------|------------|
| `darkplum/services/gateway` | Go. The OpenAI-compatible inference endpoint: routing, policy enforcement, provider translation (vLLM, Bedrock, Anthropic, OpenAI), usage recording. |
| `darkplum/services/api` | Go. Authentication, users and keys, model catalogue, routing policy, conversations, the library and tool loop, audit, spend. Owns the Postgres schema. |
| `darkplum/services/web` | React + TypeScript. Chat, Library and admin UI. |
| `darkplum/services/cli` | Go. The `darkplum` operator CLI: `admin`, `sso`, `status`, `doctor`. |
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
