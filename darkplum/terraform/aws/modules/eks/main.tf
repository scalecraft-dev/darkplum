module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "20.37.2"

  cluster_name    = var.cluster_name
  cluster_version = var.cluster_version

  vpc_id     = var.vpc_id
  subnet_ids = var.subnet_ids

  # Public access is intentional for this tracer bullet.
  # For production, restrict cluster_endpoint_public_access_cidrs to known CIDR ranges.
  cluster_endpoint_public_access  = true
  cluster_endpoint_private_access = true

  # IRSA is kept enabled to avoid destroying the OIDC provider (destructive state
  # change). Remove when confirmed nothing else uses the OIDC provider.
  enable_irsa = true

  # KNOWN NON-REPRODUCIBLE: `most_recent` resolves to whatever add-on version EKS
  # offers for cluster_version on the day of the apply, so two installs of the
  # same Darkplum release can differ here. Pinning needs a tested version per
  # cluster_version and is tracked separately; the module and provider versions
  # (this file, modules/vpc, versions.tf, the lock file) are exact.
  cluster_addons = {
    vpc-cni = {
      most_recent = true
      # Enable Kubernetes NetworkPolicy enforcement. Without this the VPC CNI
      # admits NetworkPolicy objects but does NOT enforce them — the
      # gateway-egress policy (IMDS/SSRF egress block) would be decorative.
      # The flag is the add-on's top-level `enableNetworkPolicy` (a "true"/
      # "false" string). Current add-on versions reject it as an `env` entry
      # (`$.env.ENABLE_NETWORK_POLICY: is not defined in the schema`), which
      # fails CreateAddon on a fresh install. Check the schema with
      # `aws eks describe-addon-configuration --addon-name vpc-cni`.
      configuration_values = jsonencode({
        enableNetworkPolicy = "true"
      })
    }
    coredns = {
      most_recent = true
    }
    kube-proxy = {
      most_recent = true
    }
    eks-pod-identity-agent = {
      most_recent = true
    }
  }

  enable_cluster_creator_admin_permissions = true

  tags = var.tags
}
