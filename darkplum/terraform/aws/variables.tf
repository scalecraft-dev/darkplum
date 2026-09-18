variable "aws_region" {
  description = "AWS region for the EKS cluster and supporting infrastructure."
  type        = string
  default     = "us-east-2"
}

variable "aws_profile" {
  description = "AWS CLI profile to authenticate with. Leave null to use the default credential chain (env vars, instance profile, SSO)."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Name of the EKS cluster. Used as a prefix for related resources."
  type        = string
  default     = "darkplum"
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
  description = "Hugging Face revision (branch, tag, or preferably a full commit SHA) of model_name. Pin a SHA so a re-download cannot silently change the weights."
  type        = string
  default     = "main"
}

variable "served_model_name" {
  description = "Short model name exposed on the OpenAI-compatible API."
  type        = string
  default     = "mistral-7b"
}

variable "bedrock_region" {
  description = <<-EOT
    Region the gateway calls Bedrock in. Defaults to aws_region, which keeps
    Bedrock traffic on PrivateLink (see bedrock_private_endpoints). A different
    region is supported but then calls leave the VPC via the NAT gateway.
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
