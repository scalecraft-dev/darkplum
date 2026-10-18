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

## Database durability (AWS)

The RDS instance holds everything the platform has: conversations, library items, policies, settings. Its defaults are set so that losing it takes a deliberate act:

- `db_backup_retention_days = 7` — automated backups with point-in-time recovery over the window (AWS allows 1–35).
- `db_deletion_protection = true` — the instance refuses deletion from Terraform, the console and the API until the flag is applied as `false`. `make destroy` does that itself right before `terraform destroy`.
- `db_skip_final_snapshot = false` — a teardown leaves a snapshot named `<cluster_name>-db-final-<id>`. It costs snapshot storage until you delete it; do that only once you are sure.

`make destroy` is the deliberate act: it turns protection off itself, warns and pauses when no snapshot will be kept, and prints the snapshot name when it is done. The protection stops a stray `terraform destroy`, a console click and an API call; it does not stop `make destroy`, by design.

Upgrading an existing install: all three apply in place without a restart (`terraform apply`; the module sets `apply_immediately` so the longer retention takes effect at once rather than at the next maintenance window). Backups and snapshots stay in the instance's region and are encrypted like the storage, with the AWS-managed `aws/rds` key; a customer-managed KMS key is a follow-up.

Restore is manual and not yet scripted: there is no variable to build a new install from a snapshot, and a point-in-time restore creates an instance Terraform does not track. Until that lands, restore from the console or CLI and point the cluster's `DATABASE_URL` at the result.

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
