variable "name" {
  type        = string
  description = "Name prefix for VPC resources"
}

variable "cidr" {
  type        = string
  default     = "10.0.0.0/16"
  description = "CIDR block for the VPC"
}

variable "azs" {
  type        = list(string)
  description = "Availability zones to use"
}

variable "tags" {
  type        = map(string)
  default     = {}
  description = "Additional tags for all resources"
}

variable "reserve_lb_ips" {
  type        = bool
  default     = false
  description = "Reserve one address per private subnet for the web NLB (modules/web-access). Off: nothing is created."
}
