variable "cluster_name" {
  type        = string
  description = "Name of the EKS cluster"
}

variable "subnet_ids" {
  type        = list(string)
  description = "Subnet IDs for the node groups"
}

variable "cpu_instance_types" {
  type        = list(string)
  default     = ["m5.large"]
  description = "Instance types for the CPU system node group"
}

variable "cpu_desired_size" {
  type        = number
  default     = 2
  description = "Desired number of CPU nodes"
}

variable "gpu_instance_types" {
  type        = list(string)
  default     = ["g5.xlarge"]
  description = "Instance types for the GPU inference node group"
}

variable "gpu_desired_size" {
  type        = number
  default     = 1
  description = "Desired number of GPU nodes"
}

variable "gpu_min_size" {
  type        = number
  default     = 0
  description = "Minimum number of GPU nodes (set to 0 to save cost when idle)"
}

variable "gpu_disk_size" {
  type        = number
  default     = 100
  description = "Root volume size in GB for GPU nodes (needs space for model weights + download cache)"
}

variable "gpu_max_size" {
  type        = number
  default     = 2
  description = "Maximum number of GPU nodes"
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags for all resources"
}
