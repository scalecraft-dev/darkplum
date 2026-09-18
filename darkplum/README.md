# Darkplum — Private AI Cloud

Deploy a private LLM inference stack in your own AWS account. One command, your infrastructure, your data.

## What This Deploys

```
┌─────────────────────────────────────────────────┐
│  Your AWS Account                               │
│                                                 │
│  ┌──────────────────────────────────────────┐   │
│  │  EKS Cluster                             │   │
│  │                                          │   │
│  │  CPU Nodes (m5.large x2)                 │   │
│  │  ├── CoreDNS                             │   │
│  │  └── kube-proxy                          │   │
│  │                                          │   │
│  │  GPU Node (g5.xlarge x1)                 │   │
│  │  ├── NVIDIA Device Plugin                │   │
│  │  └── vLLM (Mistral 7B Instruct)         │   │
│  │      └── OpenAI-compatible API           │   │
│  │          /v1/chat/completions            │   │
│  └──────────────────────────────────────────┘   │
│                                                 │
│  VPC (3 AZs, private + public subnets, NAT)     │
│  IRSA roles (per-service IAM isolation)          │
└─────────────────────────────────────────────────┘
```

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

| Resource | Hourly | Monthly (24/7) |
|----------|--------|----------------|
| g5.xlarge (1x A10G GPU) | $1.01 | $730 |
| m5.large x2 (CPU nodes) | $0.19 | $137 |
| NAT Gateway | $0.05 | $33 |
| **Total** | **~$1.25** | **~$900** |

The GPU node group supports scaling to zero (`gpu_desired_size = 0`). With zero GPUs, `make deploy` skips self-hosted vLLM entirely and installs a Bedrock-only platform with no GPU cost (the $1.01/hr line above) — the cheapest way to get to first chat; raise `gpu_desired_size` and re-run `make deploy` to add self-hosted models later. **Remember to run `make destroy` when done testing.**

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
| `bedrock_region` | us-east-1 | Region the gateway calls Bedrock in |
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

The gateway itself is never exposed: its `/health` endpoint reports routing and
policy state and is meant for in-cluster probes and operators only.

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
