# Pure computation of the Layer 1 (cloud IAM) Bedrock allowlist. No resources and
# no providers, so the boundary's shape is unit-testable offline:
#   cd terraform/aws/modules/bedrock-allowlist && terraform init && terraform test
#
# The gateway invokes geo inference-profile IDs ("us.anthropic.claude-..."). IAM
# authorises those against BOTH the profile ARN in the calling account/region
# and the underlying foundation-model ARN in every region the profile can route
# to. So the foundation-model side is enumerated per destination region of the
# chosen geography — never wildcarded across all regions — and "global."
# profiles (worldwide routing) are never matched.

locals {
  geo_regions = {
    us   = ["us-east-1", "us-east-2", "us-west-2"]
    eu   = ["eu-central-1", "eu-north-1", "eu-south-1", "eu-south-2", "eu-west-1", "eu-west-3"]
    apac = ["ap-northeast-1", "ap-northeast-2", "ap-northeast-3", "ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2", "ap-southeast-4"]
  }

  destination_regions = distinct(concat(
    length(var.destination_regions) > 0 ? var.destination_regions : local.geo_regions[var.geo],
    [var.region],
  ))

  invoke_arns = concat(
    [for pair in setproduct(local.destination_regions, var.model_patterns) : "arn:aws:bedrock:${pair[0]}::foundation-model/${pair[1]}"],
    [for p in var.model_patterns : "arn:aws:bedrock:${var.region}:${var.account_id}:inference-profile/${var.geo}.${p}"],
  )

  # Same patterns as gateway catalog globs (geo prefix + model ID), so the
  # catalog never lists a Bedrock model this role cannot invoke.
  model_filter = join(",", [for p in var.model_patterns : "${var.geo}.${p}"])
}
