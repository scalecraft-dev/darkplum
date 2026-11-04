variable "bucket_name" {
  description = "Name of the library bucket (<cluster_name>-library-<account_id>). The caller computes it so the bucket policy, the api role's grants and the CLI all name the same bucket without depending on the resource."
  type        = string
}

variable "partition" {
  description = "AWS partition of the ARNs."
  type        = string
  default     = "aws"
}

variable "gateway_role_arns" {
  description = "Roles that get an explicit deny on the whole bucket (the gateway never reads library content; the api injects it)."
  type        = list(string)
  default     = []
}

variable "noncurrent_days" {
  description = "Days a replaced or deleted version is kept before S3 expires it. The recovery window for an accidental delete."
  type        = number
  default     = 30

  validation {
    condition     = var.noncurrent_days >= 1 && floor(var.noncurrent_days) == var.noncurrent_days
    error_message = "noncurrent_days must be a whole number of days, at least 1."
  }
}

variable "kms_key_arn" {
  description = "Customer-managed KMS key (ARN). null = SSE-S3. When set, default encryption is SSE-KMS with this key; its key policy must let the api role use it, and Darkplum does not manage it."
  type        = string
  default     = null

  validation {
    condition     = var.kms_key_arn == null || can(regex("^arn:aws[a-z-]*:kms:[a-z0-9-]+:[0-9]{12}:key/[0-9a-zA-Z-]+$", var.kms_key_arn))
    error_message = "kms_key_arn must be a KMS key ARN (arn:aws:kms:<region>:<account>:key/<id>), not an alias."
  }
}

variable "force_destroy" {
  description = "Let Terraform delete a bucket that still holds objects. Off: the bucket holds customer data, and `darkplum destroy` keeps it by default."
  type        = bool
  default     = false
}

variable "tags" {
  description = "Tags for every resource."
  type        = map(string)
  default     = {}
}
