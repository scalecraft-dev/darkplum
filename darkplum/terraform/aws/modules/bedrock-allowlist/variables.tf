variable "account_id" {
  description = "AWS account the gateway role lives in (owner of the system inference-profile ARNs)."
  type        = string
}

variable "region" {
  description = "Region the gateway calls Bedrock in."
  type        = string
}

variable "geo" {
  description = "Inference-profile geography: us, eu or apac. \"global\" is deliberately not accepted."
  type        = string

  validation {
    condition     = contains(["us", "eu", "apac"], var.geo)
    error_message = "geo must be one of: us, eu, apac."
  }
}

variable "destination_regions" {
  description = "Override for the regions the geo profile may route to. Empty = built-in list for the geo."
  type        = list(string)
  default     = []
}

variable "model_patterns" {
  description = "Bedrock model-ID globs, e.g. \"anthropic.claude-*\". Not ARNs."
  type        = list(string)

  validation {
    condition     = alltrue([for p in var.model_patterns : can(regex("^[a-z0-9][a-z0-9.*:_-]*$", p))])
    error_message = "Each pattern must be a Bedrock model ID or glob such as \"anthropic.claude-*\" (lowercase letters, digits, . * : _ -). Do not pass ARNs."
  }
}
