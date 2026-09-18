output "endpoint_ids" {
  description = "VPC endpoint IDs by service (runtime, control). Empty when disabled."
  value       = { for k, v in aws_vpc_endpoint.bedrock : k => v.id }
}
