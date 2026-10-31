variable "cluster_name" {
  type        = string
  description = "Cluster the repositories belong to. Repositories are named <cluster_name>/darkplum-<service>, so two installs in one account/region never share one."
}

variable "services" {
  type        = set(string)
  description = "First-party services that get a repository."
  default     = ["gateway", "api", "web"]
}

variable "keep_tagged_images" {
  type        = number
  description = "How many tagged images each repository keeps; older ones expire."
  default     = 20

  validation {
    condition     = var.keep_tagged_images >= 2
    error_message = "keep_tagged_images must be at least 2 so the previous release stays available for a rollback."
  }
}

variable "tags" {
  type    = map(string)
  default = {}
}
