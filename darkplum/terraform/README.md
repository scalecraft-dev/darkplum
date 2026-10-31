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
darkplum init               # records the install's settings and S3 state backend
darkplum deploy             # Terraform init, plan and apply, then the platform
```

`darkplum deploy` is what drives Terraform. The `darkplum` binary embeds this
directory, so the Terraform it runs is exactly the version it was released
with; there is no checkout to keep in step. Each install has its own workspace
under `~/.darkplum/installs/<name>`, which holds the settings `darkplum init`
wrote, the state backend settings and a private kubeconfig.
`aws/terraform.tfvars.example` is an advanced reference for teams that manage
settings from their own pipeline.

Driving `terraform` by hand after `init` is supported for inspection and for
pipelines. The state backend is declared as `backend "s3" {}` in
`aws/versions.tf` with no settings, so a manual run must supply them
(`terraform init -backend-config=<the install's backend settings>`, plus
`-backend-config=profile=<profile>` for a named profile); otherwise Terraform
stops and asks for a bucket. Prefer `darkplum deploy`: it also applies the
platform in the right order and verifies the images.

A checkout of this repository is only needed to work on darkplum itself
(see CONTRIBUTING.md); a build from a checkout is a development build and
deploys images only through `--release-images` or `--unsigned-images`.

## Database durability (AWS)

The RDS instance holds everything the platform has: conversations, library items, policies, settings. Its defaults are set so that losing it takes a deliberate act:

- `db_backup_retention_days = 7` — automated backups with point-in-time recovery over the window (AWS allows 1–35).
- `db_deletion_protection = true` — the instance refuses deletion from Terraform, the console and the API until the flag is applied as `false`. `darkplum destroy` does that itself right before `terraform destroy`.
- `db_skip_final_snapshot = false` — a teardown leaves a snapshot named `<cluster_name>-db-final-<id>`. It costs snapshot storage until you delete it; do that only once you are sure.

`darkplum destroy` is the deliberate act: you type the cluster name, it turns protection off itself, requires a second typed confirmation when no snapshot will be kept, and prints the snapshot name when it is done. The protection stops a stray `terraform destroy`, a console click and an API call; it does not stop `darkplum destroy`, by design.

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
| `container_registry` | Registry host the first-party image repositories live on |
| `image_repositories` | Service (`gateway`, `api`, `web`) -> repository URL, `<cluster_name>/darkplum-<service>`, IMMUTABLE tags, owned by Terraform (destroyed with the install) |
| `cluster_service_cidr` | The cluster's Kubernetes service CIDR (scopes the gateway-egress-services NetworkPolicy) |
| `database_url` (sensitive) | Control-plane Postgres connection string |
| `rds_endpoint` | Database host:port |
| `inference_backend` | `bedrock` \| `vertex` — the gateway Provider to use |
| `inference_region` | Region the hosted-model backend is called in |
| `bedrock_model_filter` | Catalog globs for the gateway, derived from the hosted-model IAM allowlist (AWS: `bedrock_model_patterns`) |
| `model_name` / `served_model_name` | Self-hosted vLLM model |
| `web_url` / `web_hostname` | The team's private HTTPS address (empty without `web_domain`) |
| `web_lb_private_ips` | The internal NLB's pinned addresses; `darkplum configure` trusts only these to pass client addresses |
| `web_name_servers` | Name servers of a zone Terraform created for the address |
| `web_allowed_cidrs` | Networks allowed to reach the address |

When you add a new cloud root, copy this contract verbatim and resolve every
cloud difference *inside* the Terraform so these outputs stay identical.

## Operator permissions for the image repositories

The person running `darkplum deploy` mirrors the verified release images into the ECR repositories above, so their AWS role needs, on `arn:aws:ecr:<region>:<account>:repository/<cluster_name>/darkplum-*`:
`ecr:BatchCheckLayerAvailability`, `ecr:BatchGetImage`, `ecr:CompleteLayerUpload`, `ecr:DescribeImages`, `ecr:GetDownloadUrlForLayer`, `ecr:InitiateLayerUpload`, `ecr:PutImage`, `ecr:UploadLayerPart`, and, on `*`, `ecr:GetAuthorizationToken`.
Creating and deleting the repositories themselves (`ecr:CreateRepository`, `ecr:DeleteRepository`, `ecr:PutLifecyclePolicy`, `ecr:TagResource` and the matching reads) is part of the Terraform apply and destroy, not of the mirror. The nodes pull with the cluster's node role (`AmazonEC2ContainerRegistryReadOnly`), unchanged.
