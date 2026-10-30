output "url" {
  value = "https://${var.hostname}"
}

output "hostname" {
  value = var.hostname
}

output "lb_private_ips" {
  description = "The NLB's pinned private addresses: the only peers nginx trusts to name the client."
  value       = var.lb_private_ips
}

output "name_servers" {
  description = "Name servers of the zone Terraform created (empty when the zone was supplied)."
  value       = local.create_zone ? aws_route53_zone.web[0].name_servers : []
}

output "allowed_cidrs" {
  value = var.allowed_cidrs
}

output "manages_zone" {
  description = "Whether Terraform created (and will delete) the hosted zone."
  value       = local.create_zone
}

output "manages_certificate" {
  description = "Whether Terraform issued (and will delete) the certificate."
  value       = local.create_cert
}
