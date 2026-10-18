variable "name" {
  type = string
}

variable "vpc_id" {
  type = string
}

variable "subnet_ids" {
  type = list(string)
}

variable "allowed_security_group_ids" {
  type        = list(string)
  description = "Security group IDs allowed to connect to the database (e.g. EKS node SGs)"
}

variable "instance_class" {
  type    = string
  default = "db.t4g.micro"
}

variable "allocated_storage" {
  type    = number
  default = 20
}

variable "db_name" {
  type    = string
  default = "darkplum"
}

variable "db_username" {
  type    = string
  default = "darkplum"
}

variable "tags" {
  type    = map(string)
  default = {}
}

variable "backup_retention_period" {
  type        = number
  description = "Days of automated backups to keep (point-in-time recovery window). 1-35."
  default     = 7

  validation {
    condition     = var.backup_retention_period >= 1 && var.backup_retention_period <= 35
    error_message = "backup_retention_period must be between 1 and 35 days."
  }
}

variable "deletion_protection" {
  type        = bool
  description = "Refuse to delete the instance (terraform destroy, console, API) until this is turned off."
  default     = true
}

variable "skip_final_snapshot" {
  type        = bool
  description = "Skip the final snapshot when the instance is destroyed. false keeps a last copy of every conversation, library item and setting."
  default     = false
}
