output "repository_urls" {
  description = "Service name -> repository URL (no tag)."
  value       = { for k, r in aws_ecr_repository.this : k => r.repository_url }
}

output "repository_arns" {
  description = "Service name -> repository ARN."
  value       = { for k, r in aws_ecr_repository.this : k => r.arn }
}
