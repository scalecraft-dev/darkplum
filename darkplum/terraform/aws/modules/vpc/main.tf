locals {
  public_subnets  = [for i, az in var.azs : cidrsubnet(var.cidr, 8, i)]
  private_subnets = [for i, az in var.azs : cidrsubnet(var.cidr, 8, i + 100)]
}

module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.21.0"

  name = var.name
  cidr = var.cidr
  azs  = var.azs

  public_subnets  = local.public_subnets
  private_subnets = local.private_subnets

  enable_nat_gateway   = true
  single_nat_gateway   = true
  enable_dns_hostnames = true
  enable_dns_support   = true

  public_subnet_tags = {
    "kubernetes.io/role/elb" = 1
  }

  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = 1
  }

  tags = var.tags
}

# The web NLB's addresses (modules/web-access pins them): one per private
# subnet, near the top of the subnet, reserved right after the subnets exist so
# the EKS control-plane ENIs, nodes and pods (which take addresses from the
# same subnets) can never get them. The subnet outputs depend on these
# reservations, so everything placed in the subnets is created after them.
# Each CIDR comes from the subnet resource itself, in subnet-ID order.
locals {
  lb_subnet_cidrs = var.reserve_lb_ips ? module.vpc.private_subnets_cidr_blocks : []
  lb_private_ips  = [for c in local.lb_subnet_cidrs : cidrhost(c, -6)]
}

resource "aws_ec2_subnet_cidr_reservation" "lb" {
  count            = var.reserve_lb_ips ? length(local.private_subnets) : 0
  subnet_id        = module.vpc.private_subnets[count.index]
  cidr_block       = "${local.lb_private_ips[count.index]}/32"
  reservation_type = "explicit"
  description      = "darkplum ${var.name} web NLB address"

  lifecycle {
    precondition {
      condition     = length(module.vpc.private_subnets) == length(local.lb_subnet_cidrs) && try(local.lb_subnet_cidrs[count.index] == local.private_subnets[count.index], false)
      error_message = "The private subnet IDs and CIDRs must describe the same subnets, in the same order."
    }
  }
}
