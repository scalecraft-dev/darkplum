variable "name" {
  description = "Prefix for resource names (the cluster name)."
  type        = string
}

variable "hostname" {
  description = "Fully qualified name the team opens, e.g. darkplum.acme.com."
  type        = string
}

variable "domain" {
  description = "The customer's domain. With create_zone, its public Route53 zone in this account receives the NS delegation."
  type        = string
}

variable "zone_id" {
  description = "Existing public hosted zone that holds hostname (supplied, or created by darkplum init). Empty: create a zone for hostname and delegate it from the domain's zone."
  type        = string
  default     = ""
}

variable "certificate_arn" {
  description = "Existing ACM certificate covering hostname. Empty: issue one, DNS-validated in the zone."
  type        = string
  default     = ""
}

variable "allowed_cidrs" {
  description = "Networks allowed to reach port 443: private (RFC 1918) IPv4 networks only."
  type        = list(string)

  validation {
    condition = length(var.allowed_cidrs) > 0 && alltrue([
      for c in var.allowed_cidrs : can(cidrnetmask(c)) && try(cidrsubnet(c, 0, 0) == c, false) && anytrue([
        for b in ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"] :
        try(tonumber(split("/", c)[1]) >= tonumber(split("/", b)[1]) && cidrsubnet("${cidrhost(c, 0)}/${split("/", b)[1]}", 0, 0) == b, false)
      ])
    ])
    error_message = "allowed_cidrs must be canonical IPv4 networks inside 10.0.0.0/8, 172.16.0.0/12 or 192.168.0.0/16: the address is private."
  }
}

variable "vpc_id" {
  type = string
}

variable "private_subnet_ids" {
  type = list(string)
}

variable "lb_private_ips" {
  description = "The NLB's address in each of private_subnet_ids, same order, reserved by the vpc module (reserve_lb_ips)."
  type        = list(string)
}

variable "node_security_group_id" {
  description = "Security group of the CPU nodes (the cluster primary security group: the CPU node group has no launch template of its own)."
  type        = string
}

variable "node_autoscaling_group_name" {
  description = "Auto Scaling group of the CPU node group; its instances are the NLB's targets."
  type        = string
}

variable "node_port" {
  description = "NodePort of the darkplum-web-lb Service (nginx's PROXY-protocol port; k8s/base/web/service.yaml)."
  type        = number
  default     = 30080
}

variable "tags" {
  type    = map(string)
  default = {}
}
