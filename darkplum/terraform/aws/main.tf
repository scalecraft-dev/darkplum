provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile

  # Pins the install to the account `darkplum init` recorded. Empty disables it.
  allowed_account_ids = var.allowed_account_ids
}

# Account ID is needed to construct the ECR registry host, surfaced as the
# normalized container_registry output so the platform-deploy step never has to
# call `aws sts get-caller-identity` itself.
data "aws_caller_identity" "current" {}
data "aws_partition" "current" {}

locals {
  # Bedrock is the inference backend on AWS. This value is surfaced as a
  # normalized output so the cloud-agnostic platform-deploy step can route to
  # the right gateway Provider (bedrock vs. vertex) without branching on cloud.
  inference_backend = "bedrock"

  bedrock_region = var.bedrock_region != "" ? var.bedrock_region : var.aws_region

  # PrivateLink only works in-region; out-of-region Bedrock goes through NAT.
  bedrock_private_endpoints = var.bedrock_private_endpoints && local.bedrock_region == var.aws_region
  bedrock_transport         = local.bedrock_private_endpoints ? "privatelink" : "nat"

  # Layer 1 allowlist for cloud-managed models — computed (and unit-tested) in
  # modules/bedrock-allowlist.
  bedrock_invoke_arns  = module.bedrock_allowlist.invoke_arns
  bedrock_model_filter = module.bedrock_allowlist.model_filter

  # <cluster_name>-library-<account_id>: at most 40 + 9 + 12 characters, so inside
  # S3's 63; the module still refuses a longer one, and `darkplum doctor` warns.
  library_bucket_name = "${var.cluster_name}-library-${data.aws_caller_identity.current.account_id}"

  tags = {
    Project   = "darkplum"
    ManagedBy = "terraform"
  }
}

module "bedrock_allowlist" {
  source = "./modules/bedrock-allowlist"

  account_id          = data.aws_caller_identity.current.account_id
  region              = local.bedrock_region
  geo                 = var.bedrock_inference_geo
  destination_regions = var.bedrock_destination_regions
  model_patterns      = var.bedrock_model_patterns
}

module "vpc" {
  source = "./modules/vpc"

  name = var.cluster_name
  cidr = var.vpc_cidr
  azs  = var.azs
  tags = local.tags

  # The web NLB's addresses are reserved with the subnets, before the cluster
  # takes addresses from them. Off (no change) without web_domain.
  reserve_lb_ips = var.web_domain != ""
}

module "eks" {
  source = "./modules/eks"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version
  vpc_id          = module.vpc.vpc_id
  subnet_ids      = module.vpc.private_subnets
  tags            = local.tags
}

# A reinstall finds the log group `darkplum destroy` kept. The import happens in the
# normal plan and apply (the plan shows "1 to import"), not in a separate
# `terraform import`, which evaluates the whole configuration against an empty state
# and fails on any for_each that depends on a resource not yet created.
import {
  for_each = var.adopt_eks_log_group ? toset(["eks"]) : toset([])
  to       = module.eks.module.eks.aws_cloudwatch_log_group.this[0]
  id       = "/aws/eks/${var.cluster_name}/cluster"
}

module "node_groups" {
  source = "./modules/node-groups"

  cluster_name       = module.eks.cluster_name
  subnet_ids         = module.vpc.private_subnets
  cpu_instance_types = var.cpu_instance_types
  gpu_instance_types = var.gpu_instance_types
  gpu_desired_size   = var.gpu_desired_size
  tags               = local.tags
}

module "bedrock_endpoints" {
  source = "./modules/bedrock-endpoints"

  enabled    = local.bedrock_private_endpoints
  name       = var.cluster_name
  region     = var.aws_region
  account_id = data.aws_caller_identity.current.account_id
  vpc_id     = module.vpc.vpc_id
  vpc_cidr   = var.vpc_cidr
  subnet_ids = module.vpc.private_subnets
  tags       = local.tags
}

module "pod_identity_gateway" {
  source = "./modules/pod-identity"

  role_name            = "${var.cluster_name}-gateway"
  cluster_name         = module.eks.cluster_name
  cluster_arn          = module.eks.cluster_arn
  account_id           = data.aws_caller_identity.current.account_id
  namespace            = "darkplum-system"
  service_account_name = "darkplum-gateway"
  inline_policies = {
    # The gateway never reads library content (the api injects it into a request).
    # An explicit deny, so no policy added later grants it by accident.
    deny-library = jsonencode({
      Version = "2012-10-17"
      Statement = [
        {
          Sid      = "NoLibraryBucket"
          Effect   = "Deny"
          Action   = ["s3:*"]
          Resource = ["arn:${data.aws_partition.current.partition}:s3:::${local.library_bucket_name}", "arn:${data.aws_partition.current.partition}:s3:::${local.library_bucket_name}/*"]
        }
      ]
    })
    bedrock-invoke = jsonencode({
      Version = "2012-10-17"
      # An IAM statement cannot have an empty Resource list, so the invoke
      # statement is omitted entirely when no patterns are allowed.
      Statement = concat(
        length(local.bedrock_invoke_arns) == 0 ? [] : [
          {
            Sid      = "InvokeModels"
            Effect   = "Allow"
            Action   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
            Resource = local.bedrock_invoke_arns
          }
        ],
        [
          {
            Sid    = "DiscoverModels"
            Effect = "Allow"
            # Read-only listings. ListFoundationModels carries each model's
            # lifecycle (ACTIVE / LEGACY / end-of-life time), so a model AWS has
            # retired is shown as retired instead of offered as ready.
            Action   = ["bedrock:ListInferenceProfiles", "bedrock:ListFoundationModels"]
            Resource = "*"
          },
          {
            # AWS omits a retired model from the list while its inference profile
            # stays active; GetFoundationModel answers for it with the
            # end-of-life error. Scoped to foundation-model ARNs (any region: a
            # profile's models can be in other regions, and global.* profiles
            # have an empty region in the ARN).
            Sid      = "ReadModelLifecycle"
            Effect   = "Allow"
            Action   = ["bedrock:GetFoundationModel"]
            Resource = "arn:${data.aws_partition.current.partition}:bedrock:*::foundation-model/*"
          },
          {
            # Read-only: lets the gateway tell an administrator, before anyone
            # sends a prompt, that a model (Claude Fable 5 and later) needs the
            # account's Bedrock data-retention mode raised. The write
            # (bedrock:PutAccountDataRetention) is deliberately NOT granted —
            # that is a data-handling decision for the account owner, made with
            # their own credentials, never by the platform.
            Sid      = "ReadDataRetentionMode"
            Effect   = "Allow"
            Action   = ["bedrock:GetAccountDataRetention"]
            Resource = "*"
          }
        ]
      )
    })
  }
  tags = local.tags
}

# The api's own identity. Its only cloud permission is reading Bedrock spend
# from Cost Explorer for the optional usage/cost reconciliation view — a
# read-only, account-level billing query, no model access of any kind. Kept
# separate from the gateway role on purpose: the gateway never reads billing
# and the api never invokes models.
module "pod_identity_api" {
  source = "./modules/pod-identity"

  role_name            = "${var.cluster_name}-api"
  cluster_name         = module.eks.cluster_name
  cluster_arn          = module.eks.cluster_arn
  account_id           = data.aws_caller_identity.current.account_id
  namespace            = "darkplum-system"
  service_account_name = "darkplum-api"
  inline_policies = merge({
    cost-explorer-read = jsonencode({
      Version = "2012-10-17"
      Statement = [
        {
          Sid      = "ReadBedrockSpend"
          Effect   = "Allow"
          Action   = ["ce:GetCostAndUsage"]
          Resource = "*"
        }
      ]
    })
    }, var.library_bucket_enabled ? {
    library-store = jsonencode({
      Version = "2012-10-17"
      Statement = concat(
        [
          {
            # Unconditioned on purpose: a GetObject or HeadObject on a key that is not
            # there is answered 404 only when the caller holds s3:ListBucket, and that
            # check has no s3:prefix context, so a prefix condition turns every miss
            # into a 403. The data stays scoped: objects are limited to library/*
            # below, and listing returns names only (the bucket holds nothing else).
            Sid      = "ListLibraryBucket"
            Effect   = "Allow"
            Action   = ["s3:ListBucket", "s3:ListBucketVersions"]
            Resource = "arn:${data.aws_partition.current.partition}:s3:::${local.library_bucket_name}"
          },
          {
            Sid      = "ReadWriteLibraryObjects"
            Effect   = "Allow"
            Action   = ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:DeleteObjectVersion"]
            Resource = "arn:${data.aws_partition.current.partition}:s3:::${local.library_bucket_name}/library/*"
          }
        ],
        var.library_kms_key_arn == null ? [] : [
          {
            Sid      = "UseLibraryKey"
            Effect   = "Allow"
            Action   = ["kms:GenerateDataKey", "kms:Decrypt", "kms:DescribeKey"]
            Resource = var.library_kms_key_arn
          }
        ]
      )
    })
  } : {})
  tags = local.tags
}

module "rds" {
  source = "./modules/rds"

  name                       = var.cluster_name
  vpc_id                     = module.vpc.vpc_id
  subnet_ids                 = module.vpc.private_subnets
  allowed_security_group_ids = [module.eks.cluster_primary_security_group_id]
  instance_class             = "db.t4g.micro"
  backup_retention_period    = var.db_backup_retention_days
  deletion_protection        = var.db_deletion_protection
  skip_final_snapshot        = var.db_skip_final_snapshot
  tags                       = local.tags
}

# The private HTTPS address the team uses (internal NLB + DNS + certificate).
# Off for installs without web_domain.
module "web_access" {
  source = "./modules/web-access"
  count  = var.web_domain != "" ? 1 : 0

  name                        = var.cluster_name
  domain                      = var.web_domain
  hostname                    = var.web_hostname != "" ? var.web_hostname : "darkplum.${var.web_domain}"
  zone_id                     = var.web_zone_id
  certificate_arn             = var.web_certificate_arn
  allowed_cidrs               = length(var.web_allowed_cidrs) > 0 ? var.web_allowed_cidrs : [var.vpc_cidr]
  vpc_id                      = module.vpc.vpc_id
  private_subnet_ids          = module.vpc.private_subnets
  lb_private_ips              = module.vpc.lb_private_ips
  node_security_group_id      = module.eks.cluster_primary_security_group_id
  node_autoscaling_group_name = module.node_groups.cpu_autoscaling_group_name
  tags                        = local.tags
}

# The first-party image repositories (<cluster_name>/darkplum-{gateway,api,web}).
# `darkplum deploy` mirrors the verified release images into them.
module "ecr" {
  source = "./modules/ecr"

  cluster_name = var.cluster_name
  tags         = local.tags
}

# The Library's optional bucket (library/v1/ holds every item as a plain file;
# the Library itself is in Postgres). Off by default; library_bucket_enabled
# creates it. A reinstall finds the bucket `darkplum destroy` kept: it is
# imported in the normal plan, like the EKS log group. Only the bucket needs
# importing: its configuration resources are re-applied over it, and those are
# idempotent.
module "library_bucket" {
  source = "./modules/library-bucket"
  count  = var.library_bucket_enabled ? 1 : 0

  bucket_name       = local.library_bucket_name
  partition         = data.aws_partition.current.partition
  gateway_role_arns = [module.pod_identity_gateway.role_arn]
  noncurrent_days   = var.library_noncurrent_days
  kms_key_arn       = var.library_kms_key_arn
  tags              = local.tags
}

# Installs made before the bucket was optional hold it at the unindexed address.
moved {
  from = module.library_bucket
  to   = module.library_bucket[0]
}

import {
  for_each = var.adopt_library_bucket && var.library_bucket_enabled ? toset(["library"]) : toset([])
  to       = module.library_bucket[0].aws_s3_bucket.this
  id       = local.library_bucket_name
}
