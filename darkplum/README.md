# darkplum operations guide

Deploy a private AI platform in your own AWS account with the `darkplum` CLI. Your prompts never reach
darkplum: self-hosted open-source models never leave your cluster, and Bedrock
models run under your own account, your own keys and your own audit trail.

This is the install, configuration and operations reference. For what the
project is, the repository layout, contributing and the license, start at the
[repository README](../README.md). darkplum is open source under the Apache
License 2.0.

## What this deploys

```
┌──────────────────────────────────────────────────────────────────────┐
│  Your AWS Account                                                    │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────┐      │
│  │  EKS Cluster (private subnets)                             │      │
│  │                                                            │      │
│  │  darkplum-system (CPU nodes, m5.large x2)                  │      │
│  │  ├── web      — chat + admin UI   ◄── internal NLB, HTTPS  │      │
│  │  ├── api      — auth, catalog, aliases,  (ACM certificate, │      │
│  │  │             routing policy, audit      private address) │      │
│  │  └── gateway  — one OpenAI-compatible                      │      │
│  │                 endpoint; enforces model,                  │      │
│  │                 enabled flag, residency  ──► Bedrock via   │      │
│  │                 on every request             PrivateLink   │      │
│  │                       │                      (IAM allowlist│      │
│  │                       ▼                       geo-scoped)  │      │
│  │  darkplum-inference (GPU nodes, g5.xlarge, optional)       │      │
│  │  └── vLLM per open-source model (darkplum models add)      │      │
│  └────────────────────────────────────────────────────────────┘      │
│                                                                      │
│  RDS Postgres (private, encrypted)  ·  VPC (3 AZs, NAT)  ·  ECR      │
│  IAM: one Pod Identity role for the gateway, limited to the          │
│  configured Bedrock model families in one geography                  │
└──────────────────────────────────────────────────────────────────────┘
```

Choose Bedrock only in `darkplum init` (`gpu_desired_size = 0`: no GPU node, no
vLLM); re-run `init` later to add self-hosted open-source models.

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| AWS CLI | >= 2.x | https://aws.amazon.com/cli/ |
| Terraform | >= 1.10 | https://developer.hashicorp.com/terraform/install |
| kubectl | >= 1.28 | https://kubernetes.io/docs/tasks/tools/ |
| gh (optional) | any recent | https://cli.github.com/ , only to verify a manual download's attestation |

That is all: no Docker, Go, make, jq or git. You must have AWS credentials for
the account you install into (`aws configure`, or `aws sso login`).

## Install the CLI

> **Not yet released.** The Homebrew formula and release downloads below do not
> exist until the first signed release is published. Until then, build from
> source. From this directory (`darkplum/` in the repository), with Go as in
> `services/cli/go.mod`: `(cd services/cli && go build -o ../../darkplum ./cmd)`
> builds `./darkplum` (or move it onto your `PATH`). Run `./darkplum init`, then
> deploy with images from `scripts/dev.sh eks-push --install <name>` (Docker; it
> prints the exact `./darkplum deploy --unsigned-images ...` line). A development
> build trusts no signature and marks the install unsigned. See
> [CONTRIBUTING.md](../CONTRIBUTING.md).

```bash
brew install scalecraft-dev/tap/darkplum
```

Without Homebrew, download the archive for your platform and `checksums.txt`
from the [releases page](https://github.com/scalecraft-dev/darkplum/releases)
and verify them before you run anything:

```bash
sha256sum -c checksums.txt --ignore-missing      # shasum -a 256 -c on macOS
gh attestation verify darkplum_<version>_<os>_<arch>.tar.gz --repo scalecraft-dev/darkplum \
  --signer-workflow scalecraft-dev/darkplum/.github/workflows/release.yml
tar -xzf darkplum_<version>_<os>_<arch>.tar.gz darkplum
```

The binary is not notarised by Apple. It is the trust anchor of the install
(it carries the Terraform and manifests it applies and decides which image
signatures to accept), so its authenticity is the check that matters most; see
"What you are trusting" in the [repository README](../README.md#what-you-are-trusting).

## Quick start

```bash
# Optional: check this machine first (tools, cloud credentials, GPU quota).
darkplum doctor check --profile <your-profile>

# Answer a few questions (account, region, Bedrock-only or self-hosted GPU model,
# a domain you control for the team's private address, where Terraform keeps
# state). Nothing is deployed yet.
darkplum init --profile <your-profile>

# Deploy everything (takes 35-50 minutes). Shows the Terraform plan first and
# never approves a delete on its own.
darkplum deploy

# First run (~2 min, longer while a self-hosted model loads): create the first
# admin, enable models, send a test chat, record client addresses for the audit
# log, and print the team's address (https://darkplum.<domain>, reachable from
# the networks you allowed). Safe to re-run; --wait waits for a loading model.
darkplum configure

# Prove the gateway's IAM role can only reach the Bedrock models you configured
darkplum verify

# Check status while waiting for a self-hosted model to load (if you chose one)
darkplum status

# Test a self-hosted model directly
darkplum models test <name>

# Tear down when done (asks you to type the cluster name)
darkplum destroy
```

## Cost

On-demand us-east-2 list prices, 24/7 (730 h/month). Region and usage change these.

| Resource | Hourly | Monthly |
|----------|--------|---------|
| EKS control plane | $0.10 | $73 |
| m5.large x2 (CPU nodes for web, api, gateway) | $0.19 | $140 |
| NAT gateway (plus $0.045/GB processed) | $0.05 | $33 |
| RDS db.t4g.micro + 20 GB storage | $0.02 | $14 |
| Bedrock interface endpoints, 2 x 3 AZs (plus $0.01/GB) | $0.06 | $44 |
| **Bedrock-only floor (`gpu_desired_size = 0`)** | **~$0.42** | **~$305** + Bedrock tokens |
| g5.xlarge (1x A10G GPU) per self-hosted model | $1.01 | $730 |
| **With one self-hosted model** | **~$1.43** | **~$1,035** |

Bedrock tokens are billed by AWS per model on top of this. The GPU node group
scales to zero (`gpu_desired_size = 0`): `darkplum deploy` then skips self-hosted
vLLM and installs a Bedrock-only platform, the cheapest path to first chat;
re-run `darkplum init` (choose Self-hosted GPU) and `darkplum deploy` again to add self-hosted models.
**Run `darkplum destroy` when you are done testing.**

## Cloud selection

Each cloud is a standalone Terraform root module under `terraform/<cloud>/`,
embedded in the CLI. You deploy into exactly one; see `terraform/README.md` for
the structure and the normalized output contract the Kubernetes/platform steps
rely on.

- **AWS** — `terraform/aws/` (EKS + IRSA/Pod Identity + RDS + Bedrock). Available.
- **GCP** — `terraform/gcp/` (GKE + Workload Identity + Cloud SQL + Vertex). Planned.

## Configuration

An install's settings are kept in a private workspace on your machine, written
by `darkplum init`; `darkplum status` shows which install you are acting on.
Re-run `init` to change region, hosting mode, model or GPU type: it keeps a
backup of the previous settings. Then run `darkplum deploy`, which applies the
change. `darkplum deploy` refuses to start without an install and re-checks
your AWS session, account match and GPU quota first. Keep a laptop-independent
record of the choices you made: the settings are not stored in your account yet,
though the Terraform state is.

`init` keeps Terraform state in a versioned, KMS-encrypted, private S3 bucket in
your own account. It also pins the AWS provider to your account
(`allowed_account_ids`), so credentials for another account fail at plan time.
Preview everything with `darkplum init --dry-run`. The Terraform code only
declares `backend "s3" {}`; the bucket, key and region are recorded with the
install and handed to Terraform by the CLI. Hand-driving Terraform (or a GitOps
pipeline) is supported for advanced use; see `terraform/README.md`. Never
delete the `alias/darkplum-tfstate` KMS key: it makes all state in the bucket
unrecoverable. Installs in the same account and region share the state bucket
and KMS key (each under its own `darkplum/aws/<cluster>/` key), so they are one
trust domain; installs that must not trust each other need separate AWS
accounts.

**Upgrading.** The default self-hosted model is unchanged (Mistral-7B-Instruct-v0.3,
served as `mistral-7b`), so an existing install keeps its model and API name on
its next apply. `model_revision` now accepts a 40-hex commit SHA, or `main` for the
default model only; any other `model_name` must set a SHA. `required_version`
is now Terraform >= 1.10, and the AWS provider accepts `allowed_account_ids`
(empty by default, no restriction). Re-running `darkplum init` writes all of these
explicitly and pins the model to a reviewed SHA.

<!-- tfvars-manual:begin -->
Advanced (GitOps): teams that render tfvars from their own pipeline can start
from `terraform/aws/terraform.tfvars.example` and keep `terraform.tfvars` in
their own repository. `darkplum init` remains the supported path.
<!-- tfvars-manual:end -->

The variables are:

| Variable | Default | Description |
|----------|---------|-------------|
| `aws_region` | us-east-2 | AWS region |
| `aws_profile` | (default chain) | AWS CLI profile, if not using env/SSO creds |
| `cluster_name` | darkplum | EKS cluster name |
| `gpu_instance_types` | ["g5.xlarge"] | GPU instance type |
| `gpu_desired_size` | 1 | Number of GPU nodes |
| `model_name` | mistralai/Mistral-7B-Instruct-v0.3 | Hugging Face model ID (default self-hosted model) |
| `served_model_name` | mistral-7b | API model name |
| `bedrock_region` | = `aws_region` | Region the gateway calls Bedrock in; must belong to `bedrock_inference_geo` (validated). Set it explicitly when the cluster is outside that geography |
| `bedrock_inference_geo` | us | Geography inference stays in (`us`, `eu`, `apac`); `global` profiles are never granted |
| `bedrock_model_patterns` | Claude, Llama, Mistral | Bedrock model-ID globs the gateway may invoke (IAM allowlist + catalog filter) |
| `model_revision` | main | Hugging Face revision of the default model — pin a commit SHA |
| `web_domain` | (none) | A domain you control; the team's private address is `https://darkplum.<web_domain>`. Empty: no address (installs from before it existed) |
| `web_hostname` | darkplum.<web_domain> | Name the team opens; must be inside `web_domain` |
| `web_zone_id` | (created) | Existing public Route53 zone to put the name in, or the zone `darkplum init` created when your DNS is elsewhere; never managed |
| `web_certificate_arn` | (issued) | Existing ACM certificate for the name; never managed |
| `web_allowed_cidrs` | the VPC | Networks allowed to reach the address on 443 (VPN, Direct Connect, office); never `0.0.0.0/0` |

See `terraform/aws/variables.tf` for the full list.

## Timing breakdown

| Phase | Duration |
|-------|----------|
| Terraform (VPC + EKS + nodes) | 15-20 min |
| GPU node registration + driver | 5-10 min |
| Model download from HF Hub | 10-20 min |
| Model load into GPU memory | 2-5 min |
| **Total** | **35-50 min** |

## Commands

| Command | Description |
|---------|-------------|
| `darkplum init` | Create an install, or show an existing one and change its settings (`--dry-run` previews) |
| `darkplum deploy` | Create or update the install: infrastructure, verified images, the platform. Safe to re-run; also the upgrade path |
| `darkplum configure` | First run: first admin, models, a test chat, open the web UI |
| `darkplum status` | Show cluster, node, pod and GPU status |
| `darkplum doctor check` | Check this machine can run an install (tools, credentials, GPU quota) |
| `darkplum verify` | Prove the gateway's IAM role allows only the configured Bedrock models |
| `darkplum models add <name> --hf <id>` | Add another self-hosted open-source model (one GPU each) |
| `darkplum models remove <name>` / `list` / `test <name>` | Remove a model added with `models add`; list models and gateway upstreams; send a test chat to one |
| `darkplum keys set huggingface` | Store a Hugging Face token for gated models |
| `darkplum keys set anthropic --models 'claude-*'` | Install or rotate the organisation's Anthropic API key and set its catalog filter |
| `darkplum keys set openai --models 'gpt-*'` | Same for OpenAI |
| `darkplum keys remove anthropic\|openai` / `keys list` | Remove a vendor key and its filter (its models stop answering); show which keys are installed |
| `darkplum init --semantic-router on\|off` | Turn the open-source intent classifier for routed aliases on or off; `darkplum deploy` applies it |
| `darkplum sso configure` / `darkplum admin create` | Configure OIDC sign-in; create a local break-glass admin |
| `darkplum kubeconfig` | Point this shell's kubectl at the install's cluster (`eval "$(darkplum kubeconfig)"`) |
| `darkplum version` | The release, the digests it deploys, the third-party images it pins |
| `darkplum destroy` | Tear down all resources (type the cluster name to confirm) |
| `pnpm run check:parity` (in `services/web`) | Verify every API route has a UI and vice versa against `docs/api-routes.json` |

Working on darkplum itself (local kind stack, rebuilding one service) uses
`scripts/dev.sh`; see [CONTRIBUTING.md](../CONTRIBUTING.md).

## API reference

`docs/API.md` lists every endpoint, generated from `docs/api-routes.json`, with
the routing-policy request/response bodies and the refusal shapes (`400`, `409
locked_out`, `502`, and the chat `denial_code`s). That file is also the parity
contract: each route is marked `ui` (a screen calls it — checked by `pnpm run
check:parity`, which also fails on a wrapper no screen reaches) or `machine`
with a reason (probes, the api↔gateway service calls, the OIDC redirect target,
the plain stored-row policy GETs). Go tests in both services fail if a route is
registered that the file does not list, or listed but not registered.

One gap to know before you script against it: the admin endpoints (`auth:
admin`/`session`) authenticate with a browser session cookie only. An API key
does not work on them, so there is no non-interactive credential for managing
users, keys or policy yet — a reconciler has to drive `POST /auth/login` first.

## Reaching the web UI

Every install gets a private HTTPS address: `https://darkplum.<domain>` for a
domain you name in `darkplum init`. It is an internal Network Load Balancer in
the cluster's private subnets with an ACM certificate, reachable only from the
networks you allow (the VPC by default; add VPN, Direct Connect or office
ranges). The name resolves publicly to private addresses: anyone can look the
name up (and it appears in certificate transparency logs), only your networks
can connect. Nothing is internet-facing.

- **Your domain's DNS is a Route53 zone in the same account:** Terraform
  creates a zone for the name, delegates it from yours, issues the certificate
  and adds the record. All of it is removed by `darkplum destroy`.
- **Your DNS is elsewhere:** `darkplum init` creates the zone for the name
  itself, prints four NS records to add at your DNS provider and waits until
  they resolve (re-running init resumes). Terraform then issues the certificate
  in it. `darkplum destroy` leaves that zone, so your NS records never point at a
  deleted zone; remove both yourself when you retire the install.
- **Your own zone or certificate:** `darkplum init --zone-id` /
  `--certificate-arn`. Records are added to a supplied zone; neither is ever
  modified or deleted otherwise. A supplied certificate must be issued, valid
  for 30+ more days and cover the name (a wildcard covers one label).

The load balancer terminates TLS and passes each browser's address to nginx in
a PROXY protocol header. `darkplum configure` writes the load balancer's
own addresses into nginx's trusted list and a `web-ingress` NetworkPolicy, so
the audit log records the browser's address and no other peer can claim one.
Until it has run, the logged address is the load balancer's.

The operator's fallback is `darkplum configure`, which opens a private
port-forward and prints its `http://localhost:<port>` address.

The gateway itself is never exposed: its `/health` endpoint reports routing and
policy state and is meant for in-cluster probes and operators only.

## Conversation history

Chat history is stored in the platform's Postgres database, in your account.
Conversation content is readable only by its owner through the platform: there
is no administrator endpoint that reads another user's conversations, and the
usage log holds metadata only (who, when, which model, tokens) — never text.
That rule is enforced by the application, not by the database: the services
share one Postgres role and row-level security is not enabled, so whoever holds
the database credential (the operator who ran Terraform) can read every row
directly, exactly as they can read the audit rows. Treat that credential
accordingly. Each stored reply
records the model that actually produced it, taken from the usage log rather
than from the browser. Users can download their own history as JSON and delete
conversations permanently (hard delete, no undo); deleting a user deletes all of
their conversations. Existing browser-local history is moved to the server the
first time a user signs in after this upgrade.

Not yet built: an organisation retention policy and supervised eDiscovery
access (dual control, audited). Both are planned as explicit features rather
than by weakening the owner-only rule.

## Models and routing

Every model sits behind one OpenAI-compatible endpoint. The gateway resolves each
request to a backend and refuses anything it cannot place — an unknown, disabled
or policy-blocked model gets an error, never a silent redirect.

**Three kinds of backend**, in order of how far a prompt travels:

- **Self-hosted** — open-source models served by vLLM on your own GPU nodes.
  Prompts and completions never leave the cluster.
- **Cloud-managed** — Bedrock models (Claude, plus open-weight Llama and Mistral)
  called under your own AWS account through the gateway's IAM role. Prompts leave
  the cluster and stay inside your cloud account.
- **External API** — Anthropic and OpenAI models called directly at the vendor's
  own API with your organisation's key. Prompts leave your cluster **and** your
  cloud account, under your contract with that vendor. Off until you install a
  key, and refused by the default routing policy until an administrator allows
  it (see *Anthropic and OpenAI* below).

If the same model ID is served by more than one, the backend that keeps the
prompt closest wins: self-hosted over cloud-managed over external API. A request
never silently travels further than it has to.

**Changing the default self-hosted model.** Any vLLM-supported, ungated Hugging
Face model that fits in 24GB VRAM (A10G) works. Re-run `darkplum init`,
pick Self-hosted GPU and choose a model from the reviewed list or a custom
Hugging Face id (`--model <id>` skips the prompt). `init` checks the repo is
public and has safetensors weights, pins its current commit SHA and records the
result; then run `darkplum deploy` to apply it.

**Running more than one self-hosted model.** Each model is its own vLLM
deployment and takes one GPU, so grow the GPU node group first
(re-run `darkplum init --mode gpu --gpu-count 2`, then `darkplum deploy`), then:

```bash
darkplum models add qwen --hf Qwen/Qwen2.5-7B-Instruct --served qwen-2.5-7b --revision <commit-sha>
darkplum models list
darkplum models remove qwen
```

The gateway re-discovers models every minute; a new model shows up under
**Admin > Models**, disabled until an administrator enables it. A model whose
backend goes away (cold start, crash) keeps its settings and is shown as
unreachable until it returns.

Model backends run third-party weights, so they are locked down: safetensors only
(pickled weights are refused), no Kubernetes API token, ingress from the gateway
only, and egress limited to public HTTPS for the weight download — never the
instance metadata service or anything inside the cluster. Pin `--revision` to a
commit SHA so a re-download cannot change the weights.

**Choosing which Bedrock models are reachable.** `bedrock_model_patterns` is the
IAM allowlist for the gateway's role and also filters the catalog, so the admin
UI only lists models the role can actually invoke. Model access must still be
granted per model in the Bedrock console.

```hcl
bedrock_inference_geo  = "us"                                     # us | eu | apac
bedrock_model_patterns = ["anthropic.claude-*", "meta.llama*"]   # [] = no Bedrock
```

The role is granted the geo inference profiles (`us.anthropic.claude-*`) and the
underlying models in that geography's regions only — no all-region wildcard and no
`global.` profiles. One more thing the role may do is *read* the account's
Bedrock data-retention mode: some models (Claude Fable 5 and later) are refused
by Bedrock unless that mode is raised to `aws_review`, and darkplum uses the
read to mark such models **Needs AWS opt-in** in Admin > Models before anyone
sends a prompt. Raising the mode is an account-owner decision made with your
own credentials — the role cannot write it — and what it means is in
`docs/RUNBOOK-vendor-apis.md`. `darkplum verify` simulates the deployed role and fails
if anything outside the allowlist would be permitted.

**Anthropic and OpenAI (your own vendor keys).** Install a key from the product
or from a terminal; both end up in the same place and either can be used at any
time.

*In the product:* **Admin > Models > Model vendors > Install key.** Paste the key,
choose **Check key** — darkplum asks the vendor, through the gateway, which
models the key can reach and lists them — tick the models to offer, and choose
**Install key**. The card shows *Installing* until your cluster has picked the
key up (usually about two minutes; you can leave the page), then offers **Review
and enable**. *Replace key*, *Change models* and *Remove key* are on the same
card. Nothing is restarted. Installing from the product needs darkplum to be
served over HTTPS, or reached on `localhost` — see below; without either, the card
shows the terminal path instead.

*From a terminal:* the key is typed at a hidden prompt (or piped with
`--key-stdin`; `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` are also read when set),
never passed as an argument, and checked against the vendor before anything is
changed:

```bash
darkplum keys set anthropic --models 'claude-*'   # --models = catalog filter (globs)
# add --workspace wrkspc_... if the key was created at organisation level (see below)

darkplum keys set openai --models 'gpt-*'
```

**Create the Anthropic key inside a workspace.** Anthropic requires every request
made with a key created at *organisation* level to name a workspace
(`anthropic-workspace-id`); darkplum sends that header when you give it a
workspace ID — the optional field on the install form, or `--workspace wrkspc_…`
on the command — and the key check tells you when a key needs one. A key
created inside a workspace (Anthropic console > Workspaces > API keys) needs
nothing, and that is the better choice anyway: the workspace is where the spend
limit lives.

The list you tick in the product and `--models` on the command line are the same
setting — the catalog filter. Whichever was set last is the one in force; after a
command-line change the card says so and offers to adopt it. `--models` is required
the first time. There is no IAM allowlist beneath the
gateway for a vendor key, so this filter *is* the catalog bound: only matching
models can ever be listed, enabled or allowlisted, and an empty filter means no
vendor models at all. Name a family: a bare `*` is refused (`--allow-all`
overrides), because a glob also admits models the vendor releases later — they
arrive disabled, like any new model. After the filter, the gateway also drops
ids that are plainly not chat models (embeddings, audio, image, moderation);
that is catalog hygiene, not a bound. The command prints how many models the key can reach and
which of them the filter admits.

Then choose **Review and enable** on the vendor's card.
Two things stand between an installed key and an answer, deliberately: the
organisation's residency must be loosened to *Any model, including outside your
account* (a confirmed, audited change — every install, new or existing, sits at
*Your cloud account only*), and each model must be enabled. The same dialog does
both. After that a vendor model is an ordinary model: pick it in chat, put it in
an allowlist, make it a user's default, or make it one route of a routed alias
next to a self-hosted route — one endpoint, one policy, and the caller never
chooses the backend. Per-user and per-key residency can still hold individual
people and keys to your cloud account or to self-hosted only.

The key lives in one place — the `darkplum-provider-keys` Secret in
`darkplum-system`, file-mounted into the gateway only. It is never written to the
database and never returned by any endpoint. Installing it from Admin > Models
sends it from your browser through the web tier and the api, in memory, on its way
into that Secret — so that path must be HTTPS: the api refuses a key over plain
HTTP (local `localhost` development excepted), and inside the cluster the hops
between web, api and gateway are plain HTTP, as they are for everything else
darkplum carries. The command avoids all of that: the key goes from your
terminal to the Kubernetes API over TLS and nowhere else. The api's only
Kubernetes permission is `patch` on that one Secret and on the filter's
ConfigMap — it cannot reach any other Secret — but be clear about what that does
not mean: a Kubernetes `patch` returns the object, so a compromised api could
read the vendor keys, on top of what compromising it already yields. The install form is why the web app now ships a strict
Content-Security-Policy: a script injected into the admin UI could already act as
the signed-in admin, and a pasted key would add disclosure to that. *Replace key*,
or the same command, rotates it: the gateway re-reads the mounted file, so rotation is not a rollout
(allow about two minutes for the kubelet to refresh the mount). Behind an egress
proxy, set `HTTPS_PROXY` on the gateway; vendor calls honour it. A missing or
rejected key, an empty filter or a vendor outage disables that vendor only —
self-hosted and Bedrock models keep answering — and **Admin > Models** shows
which of those it is under *Model vendors*.

Rotation, diagnosing a vendor that is not answering (`/health` on the gateway
carries each vendor's state), turning a vendor off, and the one ordering rule
that matters — drain the vendor catalog **before** rolling the api back past this
release — are in [`docs/RUNBOOK-vendor-apis.md`](docs/RUNBOOK-vendor-apis.md).

Limits to know, because this backend is different in kind from the other two:

- **There is no cloud IAM boundary beneath the gateway.** For Bedrock, the
  gateway's role refuses models outside the allowlist even if the gateway is
  wrong. For a vendor key the only control below the gateway is the scope *you*
  set on the key at the vendor — an Anthropic workspace key with a spend limit,
  an OpenAI project key with a model allowlist and a budget. darkplum cannot see
  or verify that scope. Set it.
- **The key is organisation-wide.** Every permitted user and API key spends
  against the same vendor key; there is no per-principal vendor credential.
  Anyone who can read Secrets in `darkplum-system` can read it — which is whoever
  administers the cluster. Any darkplum administrator can replace or remove it
  from Admin > Models; each install, replacement, removal and model-list change is
  in Admin > Activity with who did it and the last four characters of the key
  they submitted.
- **No spend cap in the product.** Usage is recorded per request (tokens,
  provider and backend class, filterable and exportable in Admin > Activity as
  "Outside your cloud account"), but nothing stops a request for cost. The first
  hard stop is the limit you set at the vendor.
- **No failover across backends, by design.** If a vendor is down or
  rate-limiting, that request fails with the vendor's status (`429` with
  `Retry-After` is passed through); it is not retried on a local model. The
  only places a request can move are routed aliases (see Routed aliases): a
  picked route that is down moves to the alias's default route only as its
  *If the model on a chosen route is down* setting allows (by default never to a looser
  backend class), and a classifier failure always uses the default route. Separately, once a model id is registered
  with a stricter backend class (see Model class registration), a request for it
  that could only be answered from a looser class is refused with `503
  model_class_downgraded` instead of being sent there. Sending a prompt somewhere else because a
  GPU node was unhealthy would be a data-egress decision taken when nobody is
  watching.
- **Bring-your-own key per request is declined by design.** Callers present
  darkplum API keys only; a caller-supplied vendor key would let the caller,
  not the policy, decide where a prompt goes.
- **Tool calling, client-executed.** `tools` and `tool_choice` are accepted
  in the OpenAI shape on every backend (vLLM/LM Studio, Bedrock Converse,
  Anthropic, OpenAI): the model's `tool_calls` come back to the caller, who
  runs the tool and sends the `role: tool` turn. A malformed tool request is
  refused `400 invalid_tools` naming the field; a backend that cannot take
  tools refuses `400 unsupported_parameter` after route resolution. The
  gateway itself never runs a tool. `functions`, `function_call`,
  `response_format: json_schema` and image content are still refused by name.
  `top_p` and `stop` are forwarded.

**Usage and cost.** Admin > Activity > Spend attributes estimated cost — at the
vendors' published list prices, kept in Admin > Models > Prices — to people,
API keys, models, aliases, routes and backends, from darkplum's own records.
Optionally, a separate **billing key** per vendor (an Anthropic *Admin API key*
or OpenAI *admin key*, not the inference key, held by the api only and used by
one daily job) reconciles those estimates against the vendor's own usage and
cost reports; Bedrock reconciles through AWS Cost Explorer. Details and the
honest caveats are in `docs/RUNBOOK-vendor-apis.md`.

What is *not* configurable from the product: deploying self-hosted models
(`darkplum models add`) and the Bedrock model patterns (Terraform) are still set at
deploy time.

**Who may use which models** — policy per organisation, per user and per API
key (Admin > Models for the organisation; Admin > Users & Keys, "Routing
policy" on each row). Each level carries the same three settings:

- *Allowed models* — a list of models (inherit = whatever the level above
  allows). Lists hold concrete models; an "add every target of alias …" helper
  expands an alias when you click it, so adding a route later never widens
  anyone's access.
- *Residency* — inherit, or tighten to *your cloud account only* or *self-hosted
  only*. Below the organisation it can never loosen.
- *Default model* — preselected in the chat picker; used by an API key whose
  request names no model.

They merge most-restrictive-wins: **a key can never reach more than its owner,
and a user never more than the organisation.** The gateway checks the model that
actually serves the request — including the target a routed alias picks — and
refuses with `model_not_allowed`. Every refusal is recorded on its usage-log row
as `denial_code` and shown in Admin > Activity (filter by reason, export with
the same filter), so "did my rule fire, and on whom" has an answer in the
product, not in `kubectl logs`. A save at any level — organisation included —
that would leave a user or key with no allowed model is refused by the API
(`409`, naming who), not just greyed out in the UI, and the editor names which
of the user's keys lose access, and what they lose, before you save. Every user
can open
"why is my access limited?" in chat to see their effective policy and which
level set each part.

Two limits to know: policy changes take effect within a few seconds (the gateway
refreshes its policy snapshot; revoking a key is immediate), and per-user policy
for webchat traffic relies on the api telling the gateway who the user is —
whoever holds the api's service credential can act as any user. That credential
is a Kubernetes Secret in your own cluster and namespace, so holding it already
means cluster-admin-level access to your own account; it is not reachable from
outside, and API-key traffic is unaffected because the key itself is presented
to the gateway. Per-user and per-key allowlists are not yet backstopped by a
separate cloud IAM role; every principal shares the gateway's role until
workspaces ship. Groups and workspaces are the next principal types; the schema
reserves room for them.

**Routing policy** (Admin > Models > Policy) **and aliases** (Admin > Routing):

- *Residency* — a three-step ladder. *Any model, including outside your account*
  permits all three kinds of backend; *Your cloud account only* (the default)
  refuses external-API models; *Self-hosted only* refuses cloud-managed and
  external-API models alike. A model whose backend class the gateway does not
  recognise is refused at every step. Set for the organisation here — loosening
  it asks for confirmation and is recorded in the audit log — and tightened per
  user or per key from Users & Keys.
- *Aliases* — stable names such as `default-chat` that point at a model, so
  swapping models is an admin change rather than a code change. The usage log
  records both the alias requested and the model that served it.
- *Model class registration* — each model sync records, per model id, the
  strictest backend class it has seen serve that id (`model_registered_classes`;
  the sync only ever tightens it). The gateway compares that with the class now
  serving the id. If the backend for an id that normally runs self-hosted
  becomes unreachable and a vendor API that lists the same id (for example an
  OpenAI-compatible vendor serving the same Hugging Face id) would take it over,
  the request is refused with `503 model_class_downgraded` (a plain-language
  message, not sent anywhere) rather than answered by the vendor. This holds
  for direct model ids and static aliases, streaming or not, and the routing
  dry run reports the same; a routed alias's pick is treated as the route being
  down, so the alias's *If the model on a chosen route is down* setting applies
  (and a refusal there is `route_target_unavailable`). The user-key model list and
  the reachable-model counts omit such ids. The guarantee has conditions: both
  the api's migration 028 and a gateway that reads it must be in place; the id
  must have been registered (an id served only between two syncs, or only ever
  by a vendor, is registered at the vendor class or not at all); and an
  in-cluster source that once lists a vendor's id pins that id to self-hosted
  until an administrator accepts the new class.

  Rollout: apply the api's migrations (028) before the gateway that reads the
  table; until 028 is applied there is no class check, and the gateway logs a
  warning. The migration registers each existing model at the strictest of its
  current class and every class the usage log recorded for it. After
  upgrading, compare `SELECT model_id, registered_class FROM
  model_registered_classes` with your self-hosted inventory. A wrong row is
  fixed by accepting the class the model is discovered under now:
  `PATCH /api/v1/models/:id` with `{"accept_class": true}` — which is also how
  you move a model to a looser class on purpose. It is audited as
  `model.class_accepted` in Admin > Activity and is API-only for now (no
  button). A row that is too strict (an in-cluster source once listed a vendor's
  id) is fixed the same way, once the model is discovered under the class you
  want.
- *Routed aliases* — an alias whose administrator writes a short list of routes
  (a named intent, a description, one target model each) and picks a classifier:
  the open-source vLLM Semantic Router running on CPU inside your cluster, or
  explainable rules (keywords, code detection, prompt length). The classifier
  chooses among those routes and no others;
  the chosen model still passes the enabled and residency checks, and a route the
  policy forbids is refused, never swapped. If the chosen route's model is
  disabled or unreachable, the alias's *If the model on a chosen route is down* setting
  (`on_route_unavailable`) decides, per alias: `refuse` never uses another route
  (`503 route_target_unavailable`); `fallback_same_or_stricter` (the default, for
  new and existing aliases) uses the alias's default route only if its model is
  usable and its backend class is the same as or stricter than the chosen
  route's (self-hosted, then cloud-managed, then outside your cloud account) and
  otherwise refuses with the same 503, an unknown class refusing too;
  `fallback_any_permitted` uses the default route whenever it is usable, so
  prompts may be sent to a looser class when a route is down. A fallback is
  recorded as `fallback:route_target_disabled` or `fallback:route_target_unreachable`
  and still passes the residency and allowlist checks. A classifier failure
  (timeout, open circuit breaker, low confidence, unknown category, Semantic
  Router not installed) is a different case and always uses the default route,
  whatever this setting says; Admin > Routing warns when the default route is
  looser than another route. Every request records the route, the
  reason, and every model that saw the prompt — the classifier included — and the
  chat shows which model answered. Regulated intents can stay on in-account
  models while everything else may use the frontier gateway. Test any prompt
  against a routed alias from Admin > Routing without sending it to a model.
  Every message is classified on its own: a thread that starts with small talk
  and turns to code moves to the code route at that turn, and the reply says
  which model answered it. The whole conversation is sent each time, so a model
  taking over mid-thread sees everything said before.

  Two classifiers are available per routed alias:
  - **Semantic Router** (recommended) — the open-source [vLLM Semantic Router](https://vllm-sr.ai)
    (Apache-2.0) running on CPU inside the cluster, turned on with
    `darkplum init --semantic-router on` and applied by `darkplum deploy`. Roughly 100 ms per decision on an unloaded CPU
    node. Its decision names are the shipped defaults (`legal`, `code`, `general`;
    changing them is not supported yet) and must match your route names
    (matching is case-insensitive and treats spaces as dashes, so a decision named
    `computer science` matches a route `computer-science`). The gateway only
    calls its classification API; it never carries traffic. It keeps its
    classifier weights in ephemeral storage and re-downloads them (~1-2 GB) on
    every pod start. It reads the text of the prompts sent to routed aliases, and
    its pod may connect to any public address on port 443 (private ranges stay
    blocked) because it downloads those models from Hugging Face at startup; it
    needs about 2 GiB of memory, so add a CPU node before
    enabling it on the default 2 x m5.large node group. Until it is turned on, a
    routed alias that names it always uses its default route — Admin > Routing
    warns you.
  - **Rules** — admin keywords plus three built-in signals (`builtin:code`,
    `builtin:long`, `builtin:multimodal`). No model, no added latency, no extra
    component, one-line explanations. The right choice for air-gapped or
    minimal installs.

  An earlier option that asked a self-hosted LLM to pick the route was removed:
  it re-implemented an open-source router's prompt protocol, spent GPU capacity
  on routing, and its reference weights (Arch-Router-1.5B) are licensed for
  non-commercial use only.

  Whatever the classifier says, the chosen model still passes the enabled and
  residency checks; a pick the policy forbids is refused with the route named,
  never swapped, and a pick whose model is disabled or unreachable follows the
  alias's setting above. Disabling a model that aliases route to asks for
  confirmation and says what each alias will do. Classification runs in-cluster with a 500 ms budget and a circuit
  breaker; on timeout or breaker-open the alias uses its default route
  (regardless of the setting above) and the reason is recorded. Neither classifier is a model invocation, so the
  audit trail's "which models saw this prompt" is exactly the model that
  answered.

**Gated models** (Llama, Gemma and other licence-gated weights on Hugging Face):
accept the licence on huggingface.co with your account, then store a read token
once — typed at a hidden prompt, never passed on the command line:

```bash
darkplum keys set huggingface
darkplum models add llama --hf meta-llama/Llama-3.1-8B-Instruct --revision <commit-sha>
```

## Library: skills, tools and context

Every signed-in person has a Library (one entry in the nav; three tabs:
Skills, Tools, Context) stored in the organisation's own Postgres, not in any
AI vendor's account. A skill is instructions in Markdown, context is
reference text, a tool is an MCP server (streamable HTTP) the model may call
during a chat. Items are private by default; sharing to the organisation lets
everyone read and attach them, the owner alone edits them, and administrators
can unshare or remove shared items. Administrators never see private items.
Deleting a user removes their private items and passes their shared ones to
the acting administrator. Everything exports as plain files: `.md` with a
`name`/`description`/`kind` front matter, `.json` for a tool (never with its
credential), or one zip of all of it. Import reads the instruction text of a
Markdown file (a Claude `SKILL.md`'s scripts and resources are never
imported or run) and lists what it dropped before anything is written.

**Tools do nothing until an administrator allows their hosts.** The
allowlist (Library > Tools > Allowed tool servers, or `PUT
/api/v1/settings/tool-hosts`) is empty on every install. A tool server's host
must be listed (`mcp.corp.example`, `*.svc.cluster.local`) before it can be
verified or called; link-local and other reserved addresses can never be
listed, and redirects are never followed. A server's bearer token, if it
needs one, is written to the cluster Secret `darkplum-tool-credentials` by
the api (never to Postgres) and read back from its file mount, like the
billing keys. On a chat turn with a tool attached the api runs the loop
itself: the model's call goes to the server, the result comes back as a tool
turn, and the browser is shown each call and its result; at most 8 rounds
and 16 calls per round. A tool call sends conversation content to that
server, so list only servers you would send a prompt to.

Verified so far against the mock model, a fake MCP server and the file-mount
credential path. Live Anthropic, OpenAI and Bedrock tool turns, a real MCP
server and the cluster Secret write path have not yet been exercised.

## Upgrading

Install the new binary (`brew upgrade darkplum`, or download and verify it as
above) and run `darkplum deploy` again. It shows the Terraform plan, never
approves a delete on its own (`--confirm-plan`), refuses to apply an older
release over a newer one (`--allow-downgrade` overrides) and applies in the
right order: infrastructure, network policies, then workloads. Running it
twice with the same binary changes nothing the second time. `darkplum version`
shows what a binary will deploy.

NetworkPolicy enforcement is on. Earlier releases set the vpc-cni add-on option
in a form the add-on ignored, so the policies in `k8s/base/network-policies/`
were stored but never enforced. Now they are, so:

- the gateway may only reach DNS, HTTPS (not 169.254.0.0/16), the Pod Identity
  agent, the vLLM backends (`:8000`), the semantic router (`:8080`) and the
  database (VPC CIDR, `:5432`) - anything else, including other pods and
  custom egress you added, is dropped;
- the api accepts connections only from the web pod; use `kubectl port-forward`
  (node-local, unaffected) for operator access;
- vLLM Services become headless (`clusterIP: None`); `darkplum deploy` and
  `darkplum models add` recreate an old one (a brief gap while it is deleted);
- Pod Identity role trust now also requires this cluster's ARN and account.

The EKS control-plane log group is kept after `darkplum destroy` (Kubernetes
audit logs) and re-adopted by the next `darkplum deploy`.

**An install made with the old make-based flow** is taken over with
`darkplum init --adopt`, run from the `darkplum/` directory it was installed
from. The Terraform state stays where it is. Review the plan `darkplum deploy`
shows before approving it: the new Terraform owns the per-cluster image
repositories and pins different module versions.

## Troubleshooting

The `kubectl` commands below need the install's own kubeconfig. Run
`eval "$(darkplum kubeconfig)"` in the shell first: it checks that the
cluster belongs to the install's AWS account and points only that shell at a
private kubeconfig, without touching `~/.kube/config` or your current context.

**GPU node not ready**: Check node events — `kubectl describe node <gpu-node>`. Common cause: g5 capacity unavailable in the selected AZ. Try a different region.

**vLLM CrashLoopBackOff**: Likely still loading. The readiness probe waits 5 minutes before checking. Monitor: `kubectl logs -f -n darkplum-inference deploy/vllm-inference -c vllm`

**Model download stalled**: HF Hub can be slow from NAT gateway IPs. Check init container logs: `kubectl logs -n darkplum-inference deploy/vllm-inference -c model-download`

**Out of GPU memory**: The model doesn't fit in 24GB VRAM. Use a smaller model or a quantized variant (AWQ/GPTQ).

**Bedrock models missing from the catalog**: check `kubectl logs -n darkplum-system deploy/darkplum-gateway | grep -i bedrock` and the gateway's `/health` (port-forward `svc/darkplum-gateway 8082:8080`): a `last_error_kind` of `auth` means the gateway role has no credentials or no access to the model families in `bedrock_model_patterns` — run `darkplum verify`; `unreachable` means the VPC endpoints or NAT path are broken. Models also need access granted per model in the Bedrock console.

**Cannot reach vLLM from another pod**: A NetworkPolicy restricts vLLM ingress to pods labeled `app.kubernetes.io/name: darkplum-gateway` only. `darkplum models test` uses a port-forward, which bypasses NetworkPolicy. To call vLLM from a custom pod, add the gateway label to your pod or temporarily delete the NetworkPolicy: `kubectl delete -n darkplum-inference networkpolicy vllm-inference-ingress`

**`darkplum destroy` stops at "kubernetes resources did not go away"**: GPU pods with device plugin state can block namespace teardown. `darkplum destroy` forces nothing; it names what is holding them and leaves the infrastructure intact. Inspect with `kubectl get all -n darkplum-inference` after `eval "$(darkplum kubeconfig)"`. If you decide to force it: `kubectl delete namespace darkplum-inference --force --grace-period=0`, then re-run `darkplum destroy`.

**`terraform destroy` fails on VPC/subnet deletion**: Orphaned ENIs from node groups can block VPC teardown. `darkplum destroy` lists tagged leftovers when it finishes. Check EC2 > Network Interfaces in the AWS console, delete any in "available" state attached to the VPC, then re-run `darkplum destroy`.
