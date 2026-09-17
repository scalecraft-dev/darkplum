variable "cluster_name" {
  type        = string
  description = "Name of the EKS cluster"
}

variable "cluster_version" {
  type        = string
  default     = "1.31"
  description = "Kubernetes version for the EKS cluster"
}

variable "vpc_id" {
  type        = string
  description = "VPC ID where the cluster will be created"
}

variable "subnet_ids" {
  type        = list(string)
  description = "Subnet IDs for the EKS cluster (private subnets)"
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags for all resources"
}
