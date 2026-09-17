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

variable "served_model_name" {
  description = "Short model name exposed on the OpenAI-compatible API."
  type        = string
  default     = "mistral-7b"
}

variable "bedrock_region" {
  description = "Region the gateway calls Bedrock in. May differ from aws_region; cross-region Bedrock calls are supported."
  type        = string
  default     = "us-east-1"
}

variable "bedrock_model_arns" {
  description = "Foundation-model ARNs the gateway is allowed to invoke. This IAM allowlist is the Layer 1 (cloud IAM) enforcement boundary for model access."
  type        = list(string)
  default = [
    "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-5-sonnet-20241022-v2:0",
    "arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-haiku-20240307-v1:0",
  ]
}
