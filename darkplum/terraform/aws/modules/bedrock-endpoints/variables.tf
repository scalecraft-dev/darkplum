variable "enabled" {
  description = "Create the endpoints. Must be false when Bedrock is called in a different region from the VPC."
  type        = bool
}

variable "name" {
  description = "Name prefix (cluster name)."
  type        = string
}

variable "region" {
  description = "Region of the VPC and of the Bedrock endpoints (they must match)."
  type        = string
}

variable "account_id" {
  description = "Account whose principals may use the endpoints."
  type        = string
}

variable "vpc_id" {
  type = string
}

variable "vpc_cidr" {
  type = string
}

variable "subnet_ids" {
  description = "Private subnets to place endpoint network interfaces in (one per AZ)."
  type        = list(string)
}

variable "tags" {
  type    = map(string)
  default = {}
}
