# Interface VPC endpoints (PrivateLink) for Bedrock, so the gateway's calls to
# cloud-managed models never traverse the public internet or the NAT gateway:
# they resolve to private IPs inside the customer's VPC and stay on the AWS
# network. Two services are needed — bedrock-runtime for inference (Converse)
# and bedrock for model discovery (ListInferenceProfiles).
#
# Interface endpoints are regional, so this only works when Bedrock is called
# in the cluster's own region; the root module disables it otherwise.

locals {
  services = var.enabled ? {
    runtime = "com.amazonaws.${var.region}.bedrock-runtime"
    control = "com.amazonaws.${var.region}.bedrock"
  } : {}
}

resource "aws_security_group" "endpoints" {
  count = var.enabled ? 1 : 0

  name        = "${var.name}-bedrock-endpoints"
  description = "HTTPS from inside the VPC to the Bedrock interface endpoints"
  vpc_id      = var.vpc_id

  ingress {
    description = "HTTPS from the VPC"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = [var.vpc_cidr]
  }

  tags = merge(var.tags, { Name = "${var.name}-bedrock-endpoints" })
}

# Only principals from this account may use the endpoints. The gateway role's
# own IAM policy (the model allowlist) still decides which models it can invoke;
# this stops the endpoint being a path for credentials from any other account.
data "aws_iam_policy_document" "endpoint" {
  count = var.enabled ? 1 : 0

  statement {
    effect    = "Allow"
    actions   = ["*"]
    resources = ["*"]

    principals {
      type        = "*"
      identifiers = ["*"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:PrincipalAccount"
      values   = [var.account_id]
    }
  }
}

resource "aws_vpc_endpoint" "bedrock" {
  for_each = local.services

  vpc_id              = var.vpc_id
  service_name        = each.value
  vpc_endpoint_type   = "Interface"
  subnet_ids          = var.subnet_ids
  security_group_ids  = [aws_security_group.endpoints[0].id]
  private_dns_enabled = true
  policy              = data.aws_iam_policy_document.endpoint[0].json

  tags = merge(var.tags, { Name = "${var.name}-bedrock-${each.key}" })
}
