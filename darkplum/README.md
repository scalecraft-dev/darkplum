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

# Check status while waiting for model to load
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

The GPU node group supports scaling to zero (`gpu_desired_size = 0`) when not in use. **Remember to run `make destroy` when done testing.**

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
| `model_name` | mistralai/Mistral-7B-Instruct-v0.3 | Hugging Face model ID |
| `served_model_name` | mistral-7b | API model name |

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
| `make test` | Send a test chat completion request |
| `make status` | Show cluster, node, pod, and GPU status |
| `make destroy` | Tear down all resources |
| `make check-prereqs` | Validate required tools are installed |

## Using a Different Model

Any vLLM-supported, ungated Hugging Face model that fits in 24GB VRAM (A10G) works. Set the variables in `terraform/aws/terraform.tfvars`:

```hcl
model_name        = "Qwen/Qwen2.5-7B-Instruct"
served_model_name = "qwen-2.5-7b"
```

Gated models (Llama, etc.) require HF token wiring that is not yet implemented. Support is planned for a future release.

## Troubleshooting

**GPU node not ready**: Check node events — `kubectl describe node <gpu-node>`. Common cause: g5 capacity unavailable in the selected AZ. Try a different region.

**vLLM CrashLoopBackOff**: Likely still loading. The readiness probe waits 5 minutes before checking. Monitor: `kubectl logs -f -n darkplum-inference deploy/vllm-inference -c vllm`

**Model download stalled**: HF Hub can be slow from NAT gateway IPs. Check init container logs: `kubectl logs -n darkplum-inference deploy/vllm-inference -c model-download`

**Out of GPU memory**: The model doesn't fit in 24GB VRAM. Use a smaller model or a quantized variant (AWQ/GPTQ).

**Cannot reach vLLM from another pod**: A NetworkPolicy restricts vLLM ingress to pods labeled `app.kubernetes.io/name: darkplum-gateway` only. `make test` uses `kubectl port-forward`, which bypasses NetworkPolicy. To call vLLM from a custom pod, add the gateway label to your pod or temporarily delete the NetworkPolicy: `kubectl delete -n darkplum-inference networkpolicy vllm-inference-ingress`

**`make destroy` hangs on namespace deletion**: GPU pods with device plugin state can block namespace teardown. The Makefile retries with `--force` after 120s. If it still hangs: `kubectl delete namespace darkplum-inference --force --grace-period=0`

**`terraform destroy` fails on VPC/subnet deletion**: Orphaned ENIs from node groups can block VPC teardown. Check EC2 > Network Interfaces in the AWS console, delete any in "available" state attached to the VPC, then re-run `make destroy`.
