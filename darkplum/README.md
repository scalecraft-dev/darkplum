# Darkplum — Private AI Cloud

Deploy a private LLM platform in your own AWS account. Your prompts never reach
Darkplum: self-hosted open-source models never leave your cluster, and Bedrock
models run under your own account, your own keys and your own audit trail.

## What This Deploys

```
┌──────────────────────────────────────────────────────────────────────┐
│  Your AWS Account                                                    │
│                                                                      │
│  ┌────────────────────────────────────────────────────────────┐      │
│  │  EKS Cluster (private subnets)                             │      │
│  │                                                            │      │
│  │  darkplum-system (CPU nodes, m5.large x2)                  │      │
│  │  ├── web      — chat + admin UI          ─┐ make expose-web│      │
│  │  ├── api      — auth, catalog, aliases,   │ NLB (+ your ACM│      │
│  │  │             routing policy, audit      │ certificate)   │      │
│  │  └── gateway  — one OpenAI-compatible    ─┘                │      │
│  │                 endpoint; enforces model,                  │      │
│  │                 enabled flag, residency  ──► Bedrock via   │      │
│  │                 on every request             PrivateLink   │      │
│  │                       │                      (IAM allowlist│      │
│  │                       ▼                       geo-scoped)  │      │
│  │  darkplum-inference (GPU nodes, g5.xlarge, optional)       │      │
│  │  └── vLLM per open-source model (make deploy-model)        │      │
│  └────────────────────────────────────────────────────────────┘      │
│                                                                      │
│  RDS Postgres (private, encrypted)  ·  VPC (3 AZs, NAT)  ·  ECR      │
│  IAM: one Pod Identity role for the gateway, limited to the          │
│  configured Bedrock model families in one geography                  │
└──────────────────────────────────────────────────────────────────────┘
```

Set `gpu_desired_size = 0` for a Bedrock-only install (no GPU node, no vLLM);
raise it later to add self-hosted open-source models.

## Prerequisites

| Tool | Version | Install |
|------|---------|---------|
| AWS CLI | >= 2.x | https://aws.amazon.com/cli/ |
| Terraform | >= 1.9 | https://developer.hashicorp.com/terraform/install |
| kubectl | >= 1.28 | https://kubernetes.io/docs/tasks/tools/ |
| jq | any | https://jqlang.github.io/jq/download/ |

You must have AWS credentials configured (`aws configure` or environment variables).

## Quick Start

```bash
# Validate your environment first (tools, cloud creds, GPU quota).
# Pass --profile/--region to match aws_profile/aws_region in your tfvars, so the
# checks run against the account you actually install into.
make cli && ./bin/darkplum doctor check --cloud aws --profile <your-profile>

# Deploy everything (takes 35-50 minutes). CLOUD defaults to aws.
make deploy             # equivalent to: make deploy CLOUD=aws

# Prove the gateway's IAM role can only reach the Bedrock models you configured
make verify-bedrock-iam

# Publish the web UI (internal load balancer; add CERT_ARN=<ACM cert> for HTTPS)
make expose-web
# -> open the printed address, create the first admin, enable a model under Admin > Models

# Check status while waiting for a self-hosted model to load (if gpu_desired_size > 0)
make status

# Test inference
make test

# Tear down when done
make destroy
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
scales to zero (`gpu_desired_size = 0`): `make deploy` then skips self-hosted
vLLM and installs a Bedrock-only platform, the cheapest path to first chat;
raise `gpu_desired_size` and run `make deploy` again to add self-hosted models.
**Run `make destroy` when you are done testing** (`make unexpose-web` first if
you published the UI, so the load balancer is removed cleanly).

## Cloud selection

Each cloud is a standalone Terraform root module under `terraform/<cloud>/`. You
deploy into exactly one. `make deploy` selects it via `CLOUD` (defaults to `aws`);
see `terraform/README.md` for the structure and the normalized output contract
the Kubernetes/platform steps rely on.

- **AWS** — `terraform/aws/` (EKS + IRSA/Pod Identity + RDS + Bedrock). Available.
- **GCP** — `terraform/gcp/` (GKE + Workload Identity + Cloud SQL + Vertex). Planned.

## Configuration

Configuration is via Terraform variables in `terraform/aws/`. All have working
defaults; override only what you need:

```bash
cd terraform/aws
cp terraform.tfvars.example terraform.tfvars   # edit, then `terraform apply` or `make deploy`
```

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

See `terraform/aws/variables.tf` for the full list.

## Timing Breakdown

| Phase | Duration |
|-------|----------|
| Terraform (VPC + EKS + nodes) | 15-20 min |
| GPU node registration + driver | 5-10 min |
| Model download from HF Hub | 10-20 min |
| Model load into GPU memory | 2-5 min |
| **Total** | **35-50 min** |

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make deploy` | Full deploy: infrastructure + workloads |
| `make deploy-infra` | Provision AWS infrastructure only |
| `make deploy-k8s` | Deploy K8s workloads only (requires infra) |
| `make deploy-model NAME= MODEL=` | Add another self-hosted open-source model (one GPU each) |
| `make remove-model NAME=` | Remove a model added with `deploy-model` |
| `make list-models` | List self-hosted model deployments and gateway upstreams |
| `make verify-bedrock-iam` | Prove the gateway's IAM role allows only the configured Bedrock models |
| `make set-hf-token` | Store a Hugging Face token for gated models (from `$HF_TOKEN`) |
| `make expose-web [SCHEME=internal] [CERT_ARN=]` | Publish the web UI on an AWS load balancer |
| `make unexpose-web` | Back to cluster-internal (port-forward) |
| `make test` | Send a test chat completion request |
| `make status` | Show cluster, node, pod, and GPU status |
| `make destroy` | Tear down all resources |
| `make check-prereqs` | Validate required tools are installed |
| `make dev-up [DEV_PORT=8080] [DEV_AWS=1]` | Local kind stack with mocked inference (no cloud account needed) |
| `make dev-test` | Smoke-test the local stack |
| `make dev-down` | Delete the local kind cluster |

## Reaching the web UI

`make deploy` leaves the web UI cluster-internal. `make expose-web` publishes it
on an AWS Network Load Balancer:

- `make expose-web` — internal NLB, plain HTTP. Reachable from inside the VPC /
  your VPN only. Session cookies lose the Secure flag; use this to evaluate, not
  to run.
- `make expose-web CERT_ARN=arn:aws:acm:...` — HTTPS terminated at the NLB with
  your ACM certificate. Add `SCHEME=internet-facing` to publish publicly. Point a
  DNS CNAME at the printed hostname; the certificate must match that name.
- `make unexpose-web` — back to cluster-internal.

Plain HTTP on an internet-facing load balancer is refused. Run `make configure-sso`
after exposing; it uses the load balancer address for the OIDC redirect URL.

The NLB is created by the AWS cloud controller that runs inside the EKS control
plane (the subnets carry the `kubernetes.io/role/elb` tags it needs); no extra
controller is installed. If the load balancer does not appear within five
minutes, `expose-web` reverts to cluster-internal on its own and prints where to
look. The AWS Load Balancer Controller (ALB, WAF, per-path routing) is the
upgrade path when you need more than a TLS-terminating NLB.

The gateway itself is never exposed: its `/health` endpoint reports routing and
policy state and is meant for in-cluster probes and operators only.

## Conversation history

Chat history is stored in the platform's Postgres database, in your account.
Conversation content is readable only by its owner: there is no administrator
endpoint that reads another user's conversations, and the usage log holds
metadata only (who, when, which model, tokens) — never text. Each stored reply
records the model that actually produced it, taken from the usage log rather
than from the browser. Users can download their own history as JSON and delete
conversations permanently (hard delete, no undo); deleting a user deletes all of
their conversations. Existing browser-local history is moved to the server the
first time a user signs in after this upgrade.

Not yet built: an organisation retention policy and supervised eDiscovery
access (dual control, audited). Both are planned as explicit features rather
than by weakening the owner-only rule.

## Models and Routing

Every model sits behind one OpenAI-compatible endpoint. The gateway resolves each
request to a backend and refuses anything it cannot place — an unknown, disabled
or policy-blocked model gets an error, never a silent redirect.

**Two kinds of backend**

- **Self-hosted** — open-source models served by vLLM on your own GPU nodes.
  Prompts and completions never leave the cluster.
- **Cloud-managed** — Bedrock models (Claude, plus open-weight Llama and Mistral)
  called under your own AWS account through the gateway's IAM role.

If the same model ID is served by both, the self-hosted backend always wins.

**Changing the default self-hosted model.** Any vLLM-supported, ungated Hugging
Face model that fits in 24GB VRAM (A10G) works. Set in `terraform/aws/terraform.tfvars`:

```hcl
model_name        = "Qwen/Qwen2.5-7B-Instruct"
served_model_name = "qwen-2.5-7b"
```

**Running more than one self-hosted model.** Each model is its own vLLM
deployment and takes one GPU, so grow the GPU node group first
(`gpu_desired_size = 2`, then `make deploy-infra`), then:

```bash
make deploy-model NAME=qwen MODEL=Qwen/Qwen2.5-7B-Instruct SERVED=qwen-2.5-7b REVISION=<commit-sha>
make list-models
make remove-model NAME=qwen
```

The gateway re-discovers models every minute; a new model shows up under
**Admin > Models**, disabled until an administrator enables it. A model whose
backend goes away (cold start, crash) keeps its settings and is shown as
unreachable until it returns.

Model backends run third-party weights, so they are locked down: safetensors only
(pickled weights are refused), no Kubernetes API token, ingress from the gateway
only, and egress limited to public HTTPS for the weight download — never the
instance metadata service or anything inside the cluster. Pin `REVISION` to a
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
`global.` profiles. `make verify-bedrock-iam` simulates the deployed role and fails
if anything outside the allowlist would be permitted.

**Routing policy and aliases** (Admin > Models):

- *Residency* — `any`, or `self-hosted only`, which refuses every request that
  would go to a cloud-managed model. This is one org-wide setting today, not
  per-workspace policy.
- *Aliases* — stable names such as `default-chat` that point at a model, so
  swapping models is an admin change rather than a code change. The usage log
  records both the alias requested and the model that served it.

**Gated models** (Llama, Gemma and other licence-gated weights on Hugging Face):
accept the licence on huggingface.co with your account, then store a read token
once — it is read from the environment, never passed on the command line:

```bash
read -s HF_TOKEN && export HF_TOKEN
make set-hf-token
make deploy-model NAME=llama MODEL=meta-llama/Llama-3.1-8B-Instruct REVISION=<commit-sha>
```

## Troubleshooting

**GPU node not ready**: Check node events — `kubectl describe node <gpu-node>`. Common cause: g5 capacity unavailable in the selected AZ. Try a different region.

**vLLM CrashLoopBackOff**: Likely still loading. The readiness probe waits 5 minutes before checking. Monitor: `kubectl logs -f -n darkplum-inference deploy/vllm-inference -c vllm`

**Model download stalled**: HF Hub can be slow from NAT gateway IPs. Check init container logs: `kubectl logs -n darkplum-inference deploy/vllm-inference -c model-download`

**Out of GPU memory**: The model doesn't fit in 24GB VRAM. Use a smaller model or a quantized variant (AWQ/GPTQ).

**Bedrock models missing from the catalog**: check `kubectl logs -n darkplum-system deploy/darkplum-gateway | grep -i bedrock` and the gateway's `/health` (port-forward `svc/darkplum-gateway 8082:8080`): a `last_error_kind` of `auth` means the gateway role has no credentials or no access to the model families in `bedrock_model_patterns` — run `make verify-bedrock-iam`; `unreachable` means the VPC endpoints or NAT path are broken. Models also need access granted per model in the Bedrock console.

**Cannot reach vLLM from another pod**: A NetworkPolicy restricts vLLM ingress to pods labeled `app.kubernetes.io/name: darkplum-gateway` only. `make test` uses `kubectl port-forward`, which bypasses NetworkPolicy. To call vLLM from a custom pod, add the gateway label to your pod or temporarily delete the NetworkPolicy: `kubectl delete -n darkplum-inference networkpolicy vllm-inference-ingress`

**`make destroy` hangs on namespace deletion**: GPU pods with device plugin state can block namespace teardown. The Makefile retries with `--force` after 120s. If it still hangs: `kubectl delete namespace darkplum-inference --force --grace-period=0`

**`terraform destroy` fails on VPC/subnet deletion**: Orphaned ENIs from node groups can block VPC teardown. Check EC2 > Network Interfaces in the AWS console, delete any in "available" state attached to the VPC, then re-run `make destroy`.
