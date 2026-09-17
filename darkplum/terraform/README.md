# Darkplum Terraform

Each subdirectory is a **standalone root module for one cloud**. A customer
deploys into exactly one of them — never both. Each root has its own provider,
its own state, and its own child modules. There is intentionally no shared
"cloud abstraction" module: the IAM primitives (AWS Pod Identity vs. GKE
Workload Identity, Bedrock vs. Vertex) are honestly different and stay separate.

```
terraform/
  aws/    # EKS + IRSA/Pod Identity + RDS + Bedrock   (available)
  gcp/    # GKE + Workload Identity + Cloud SQL + Vertex  (planned)
```

## Selecting a cloud

```bash
# AWS
cd terraform/aws
cp terraform.tfvars.example terraform.tfvars   # edit as needed
terraform init && terraform apply
```

The `Makefile` at the repo root drives the full install and selects the cloud
via `CLOUD` (defaults to `aws`):

```bash
make deploy            # CLOUD=aws by default
make deploy CLOUD=gcp  # once the gcp root exists
```

## The normalized output contract

Every cloud root MUST emit the same set of output keys with the same meaning,
so the Kubernetes / platform deploy step is cloud-blind. When more than one
backend exists, the deploy step is allowed to branch on exactly **one** of these
outputs — `inference_backend` — and nothing else. Today only `bedrock` ships, so
nothing branches yet; the key exists so the seam is ready when `vertex` lands.

| Output | Meaning |
|--------|---------|
| `cluster_name` | Kubernetes cluster name |
| `cluster_endpoint` | API server endpoint |
| `provider_region` | Region infra runs in |
| `kube_config_command` | Command to wire up `kubectl` |
| `container_registry` | Registry host to push platform images to |
| `database_url` (sensitive) | Control-plane Postgres connection string |
| `rds_endpoint` | Database host:port |
| `inference_backend` | `bedrock` \| `vertex` — the gateway Provider to use |
| `inference_region` | Region the hosted-model backend is called in |
| `bedrock_model_filter` | Catalog globs for the gateway, derived from the hosted-model IAM allowlist (AWS: `bedrock_model_patterns`) |
| `model_name` / `served_model_name` | Self-hosted vLLM model |

When you add a new cloud root, copy this contract verbatim and resolve every
cloud difference *inside* the Terraform so these outputs stay identical.
