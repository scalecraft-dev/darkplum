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

  tags = {
    Project   = "darkplum"
    ManagedBy = "terraform"
  }
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

module "pod_identity_gateway" {
  source = "./modules/pod-identity"

  role_name            = "${var.cluster_name}-gateway"
  cluster_name         = module.eks.cluster_name
  namespace            = "darkplum-system"
  service_account_name = "darkplum-gateway"
  inline_policies = {
    bedrock-invoke = jsonencode({
      Version = "2012-10-17"
      Statement = [
        {
          Sid      = "InvokeModels"
          Effect   = "Allow"
          Action   = ["bedrock:InvokeModel", "bedrock:InvokeModelWithResponseStream"]
          Resource = var.bedrock_model_arns
        },
        {
          Sid      = "DiscoverModels"
          Effect   = "Allow"
          Action   = ["bedrock:ListInferenceProfiles"]
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
  tags                       = local.tags
}
