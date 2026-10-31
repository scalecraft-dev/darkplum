variable "aws_region" {
  description = "AWS region for the EKS cluster and supporting infrastructure."
  type        = string
  default     = "us-east-2"
}

variable "allowed_account_ids" {
  description = <<-EOT
    AWS account IDs this configuration may be applied to. The provider refuses
    any other account, so credentials for the wrong account fail at plan time
    instead of building a cluster there. `darkplum init` sets it to the account
    you chose. Empty means no restriction.
  EOT
  type        = list(string)
  default     = []

  validation {
    condition     = alltrue([for a in var.allowed_account_ids : can(regex("^[0-9]{12}$", a))])
    error_message = "Each allowed_account_ids entry must be a 12-digit AWS account ID."
  }
}

variable "aws_profile" {
  description = "AWS CLI profile to authenticate with. Leave null to use the default credential chain (env vars, instance profile, SSO)."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Name of the EKS cluster. Used as a prefix for related resources, including the RDS instance and its final snapshot, which is why the shape is restricted."
  type        = string
  default     = "darkplum"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{0,38}[a-z0-9]$", var.cluster_name)) && !strcontains(var.cluster_name, "--")
    error_message = "cluster_name must be 2-40 characters of lowercase letters, digits and single hyphens, starting with a letter and not ending with a hyphen (RDS identifier rules)."
  }
}

variable "cluster_version" {
  description = "Kubernetes version for the EKS control plane."
  type        = string
  default     = "1.35"
}

variable "vpc_cidr" {
  description = "CIDR block for the VPC."
  type        = string
  default     = "10.0.0.0/16"
}

variable "azs" {
  description = "Availability zones to spread subnets across. Must be valid for aws_region."
  type        = list(string)
  default     = ["us-east-2a", "us-east-2b", "us-east-2c"]

  validation {
    condition     = alltrue([for z in var.azs : startswith(z, var.aws_region)])
    error_message = "Every entry in azs must be an availability zone within aws_region (e.g. \"${var.aws_region}a\"). Update azs when you change aws_region."
  }
}

variable "cpu_instance_types" {
  description = "Instance types for the CPU node group (control-plane workloads)."
  type        = list(string)
  default     = ["m5.large"]
}

variable "gpu_instance_types" {
  description = "Instance types for the GPU node group (vLLM inference)."
  type        = list(string)
  default     = ["g5.xlarge"]
}

variable "gpu_desired_size" {
  description = "Number of GPU nodes. Set to 0 to scale the GPU node group to zero when idle."
  type        = number
  default     = 1
}

variable "model_name" {
  description = "Hugging Face model ID served by vLLM."
  type        = string
  default     = "mistralai/Mistral-7B-Instruct-v0.3"
}

variable "model_revision" {
  description = <<-EOT
    Hugging Face revision of model_name: a full commit SHA (40 hex characters),
    or the literal "main", accepted only for the default model so installs that
    predate pinning keep working. Any other model_name must set a SHA, so a
    re-download cannot silently change the weights. `darkplum init` always
    writes model_name, served_model_name and a pinned SHA together.
  EOT
  type        = string
  default     = "main"

  validation {
    condition     = var.model_revision == "main" || can(regex("^[0-9a-f]{40}$", var.model_revision))
    error_message = "model_revision must be a full 40-character commit SHA (lowercase hex), or \"main\" for the default model only. Not a tag or other branch."
  }

  validation {
    condition     = var.model_revision != "main" || var.model_name == "mistralai/Mistral-7B-Instruct-v0.3"
    error_message = "A model_name other than the default needs an explicit model_revision (a 40-character commit SHA); \"main\" is a moving target. Run `darkplum init` to pin one."
  }
}

variable "served_model_name" {
  description = "Short model name exposed on the OpenAI-compatible API."
  type        = string
  default     = "mistral-7b"
}

variable "db_backup_retention_days" {
  description = <<-EOT
    Days of automated RDS backups to keep. Point-in-time recovery reaches back
    this far. 7 is the floor for anything holding real conversations and
    library items; AWS allows 1-35.
  EOT
  type        = number
  default     = 7

  validation {
    condition     = var.db_backup_retention_days >= 1 && var.db_backup_retention_days <= 35
    error_message = "db_backup_retention_days must be between 1 and 35."
  }
}

variable "db_deletion_protection" {
  description = <<-EOT
    Refuse to delete the RDS instance until this is set to false and applied.
    Protects against an accidental `terraform destroy` or console deletion
    taking every conversation, library item and setting with it. `darkplum destroy`
    turns it off itself immediately before destroying.
  EOT
  type        = bool
  default     = true
}

variable "db_skip_final_snapshot" {
  description = <<-EOT
    Skip the final RDS snapshot on destroy. Leave false so a teardown keeps a
    last copy of the database (named <cluster_name>-db-final-<id>; delete it
    yourself once you are sure). Set true only for throwaway installs.
  EOT
  type        = bool
  default     = false
}

variable "bedrock_region" {
  description = <<-EOT
    Region the gateway calls Bedrock in. Defaults to aws_region, which keeps
    Bedrock traffic on PrivateLink (see bedrock_private_endpoints). Must be a
    region of bedrock_inference_geo (validated at plan time): a cluster outside
    that geography must set this explicitly, and its calls then leave the VPC
    via the NAT gateway. Upgrading from a version that hard-coded us-east-1:
    set bedrock_region = "us-east-1" to keep the old behaviour, or accept the
    move to the cluster region (the IAM allowlist is regenerated either way).
  EOT
  type        = string
  default     = ""
}

variable "bedrock_private_endpoints" {
  description = <<-EOT
    Create interface VPC endpoints (PrivateLink) for bedrock-runtime and bedrock
    so cloud-managed model traffic never leaves the AWS network. Only possible
    when bedrock_region equals aws_region; silently unused otherwise (see the
    bedrock_transport output). Roughly USD 0.01/hour per endpoint per AZ.
  EOT
  type        = bool
  default     = true
}

variable "bedrock_inference_geo" {
  description = <<-EOT
    Geography prefix of the Bedrock system inference profiles the gateway may use:
    "us", "eu" or "apac". A geo profile (e.g. "us.anthropic.claude-...") keeps
    inference inside that geography. "global" profiles, which can route a request
    to any commercial region worldwide, are deliberately not selectable — they are
    never granted and never listed.
  EOT
  type        = string
  default     = "us"

  validation {
    condition     = contains(["us", "eu", "apac"], var.bedrock_inference_geo)
    error_message = "bedrock_inference_geo must be one of: us, eu, apac."
  }
}

variable "bedrock_destination_regions" {
  description = <<-EOT
    Regions a geo inference profile may route to. IAM must allow the underlying
    foundation model in each of them. Leave empty to use the built-in list for
    bedrock_inference_geo; override when AWS adds a destination region or to
    narrow the boundary further. bedrock_region is always included.
  EOT
  type        = list(string)
  default     = []
}

variable "bedrock_model_patterns" {
  description = <<-EOT
    Bedrock model-ID patterns the gateway may invoke, e.g. "anthropic.claude-*" or
    "meta.llama*". This IAM allowlist is the Layer 1 (cloud IAM) enforcement
    boundary for cloud-managed model access. Each pattern expands to the geo
    inference-profile ARN in this account (what the gateway actually invokes,
    e.g. "us.anthropic.claude-...") AND the foundation-model ARN in each
    destination region of that geo — nothing outside the geo, nothing "global".
    The same patterns are emitted as the bedrock_model_filter output so the
    gateway's catalog only lists models this role can call. Model access must
    still be granted per model in the Bedrock console. [] allows no Bedrock models.
  EOT
  type        = list(string)
  default = [
    "anthropic.claude-*", # frontier
    "meta.llama*",        # open-weight
    "mistral.*",          # open-weight
  ]

  validation {
    condition     = alltrue([for p in var.bedrock_model_patterns : can(regex("^[a-z0-9][a-z0-9.*:_-]*$", p))])
    error_message = "Each pattern must be a Bedrock model ID or glob such as \"anthropic.claude-*\" (lowercase letters, digits, . * : _ -). Do not pass ARNs."
  }
}

# --- Private web address (modules/web-access) -------------------------------
# Written by `darkplum init`. With web_domain empty (installs from before this
# existed) nothing below is created and the web UI stays cluster-internal.

variable "web_domain" {
  description = "A domain you control, e.g. acme.com. The team reaches Darkplum at https://darkplum.<web_domain> (or web_hostname) from the allowed networks. Empty: no private address."
  type        = string
  default     = ""

  validation {
    condition     = var.web_domain == "" || (length(var.web_domain) <= 253 && can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.web_domain)))
    error_message = "web_domain must be a lowercase DNS name such as acme.com, without a trailing dot."
  }
}

variable "web_hostname" {
  description = "Name the team opens. Empty: darkplum.<web_domain>. Must be a subdomain of web_domain."
  type        = string
  default     = ""

  validation {
    condition = var.web_hostname == "" || (
      var.web_domain != "" && length(var.web_hostname) <= 253 &&
      can(regex("^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\\.)+[a-z][a-z0-9-]{0,61}[a-z0-9]$", var.web_hostname)) &&
      endswith(var.web_hostname, ".${var.web_domain}")
    )
    error_message = "web_hostname needs web_domain and must be a lowercase name inside it (e.g. chat.acme.com for acme.com), not the domain itself."
  }
}

variable "web_zone_id" {
  description = "Existing public Route53 hosted zone that covers the web hostname (yours, or the one darkplum init created when your domain's DNS is elsewhere). Records are added to it; it is never managed or deleted. Empty: Terraform creates a zone for the hostname, delegated from web_domain's zone in this account."
  type        = string
  default     = ""

  validation {
    condition     = var.web_zone_id == "" || (var.web_domain != "" && can(regex("^Z[A-Z0-9]{1,31}$", var.web_zone_id)))
    error_message = "web_zone_id needs web_domain and must be a Route53 hosted zone ID such as Z0123456789ABCDEFGHIJ."
  }
}

variable "web_certificate_arn" {
  description = "Existing ACM certificate (in aws_region) covering the web hostname. Never managed. Empty: Terraform issues one, validated through DNS."
  type        = string
  default     = ""

  validation {
    condition     = var.web_certificate_arn == "" || (var.web_domain != "" && can(regex(format("^arn:aws[a-z-]*:acm:%s:[0-9]{12}:certificate/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.aws_region), var.web_certificate_arn)))
    error_message = "web_certificate_arn needs web_domain and must be an ACM certificate ARN in aws_region."
  }
}

variable "web_allowed_cidrs" {
  description = "Private (RFC 1918) IPv4 networks allowed to reach the web address on 443 (VPN, Direct Connect, office ranges). Empty: the VPC only."
  type        = list(string)
  default     = []

  validation {
    condition = length(var.web_allowed_cidrs) == 0 || (var.web_domain != "" && alltrue([
      for c in var.web_allowed_cidrs : can(cidrnetmask(c)) && try(cidrsubnet(c, 0, 0) == c, false) && anytrue([
        for b in ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"] :
        try(tonumber(split("/", c)[1]) >= tonumber(split("/", b)[1]) && cidrsubnet("${cidrhost(c, 0)}/${split("/", b)[1]}", 0, 0) == b, false)
      ])
    ]))
    error_message = "web_allowed_cidrs needs web_domain; each entry must be an IPv4 network in canonical form (10.8.0.0/16, not 10.8.1.0/16) inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16: the address is private."
  }
}
