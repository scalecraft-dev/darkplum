variable "role_name" {
  type        = string
  description = "Name of the IAM role"
}

variable "cluster_name" {
  type        = string
  description = "Name of the EKS cluster"
}

variable "cluster_arn" {
  type        = string
  description = "ARN of the EKS cluster; the trust policy only accepts Pod Identity sessions from it"
}

variable "account_id" {
  type        = string
  description = "AWS account ID the cluster runs in (aws:SourceAccount trust condition)"
}

variable "namespace" {
  type        = string
  description = "Kubernetes namespace for the service account"
}

variable "service_account_name" {
  type        = string
  description = "Name of the Kubernetes service account"
}

variable "policy_arns" {
  type        = list(string)
  default     = []
  description = "List of IAM policy ARNs to attach to the role"
}

variable "inline_policies" {
  type        = map(string)
  default     = {}
  description = "Map of inline policy name to JSON policy document"
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags for all resources"
}
