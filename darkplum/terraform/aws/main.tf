provider "aws" {
  region  = var.aws_region
  profile = var.aws_profile
}

# Account ID is needed to construct the ECR registry host, surfaced as the
# normalized container_registry output so the platform-deploy step never has to
# call `aws sts get-caller-identity` itself.
data "aws_caller_identity" "current" {}

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
}

module "eks" {
  source = "./modules/eks"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version
  vpc_id          = module.vpc.vpc_id
  subnet_ids      = module.vpc.private_subnets
  tags            = local.tags
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
  namespace            = "darkplum-system"
  service_account_name = "darkplum-gateway"
  inline_policies = {
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
            Sid      = "DiscoverModels"
            Effect   = "Allow"
            Action   = ["bedrock:ListInferenceProfiles"]
            Resource = "*"
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
  namespace            = "darkplum-system"
  service_account_name = "darkplum-api"
  inline_policies = {
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
  }
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
