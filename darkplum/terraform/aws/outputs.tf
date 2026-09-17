# Normalized output contract.
#
# Every Darkplum cloud root module (terraform/aws, terraform/gcp, ...) MUST emit
# this same set of keys with the same semantics. The cloud-agnostic platform
# deploy step reads only these keys and is allowed to branch on exactly one of
# them — `inference_backend` — and nothing else. Any other per-cloud difference
# must be resolved inside the Terraform and surfaced here as a normalized value.

output "cluster_name" {
  description = "Name of the provisioned Kubernetes cluster."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "Kubernetes API server endpoint."
  value       = module.eks.cluster_endpoint
}

output "provider_region" {
  description = "Region the cluster and supporting infrastructure run in."
  value       = var.aws_region
}

output "kube_config_command" {
  description = "Command to point kubectl at the cluster."
  value       = "aws eks update-kubeconfig --region ${var.aws_region} --name ${module.eks.cluster_name}"
}

output "container_registry" {
  description = "Container registry host to push platform images to."
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.aws_region}.amazonaws.com"
}

output "database_url" {
  description = "Postgres connection string for the control-plane database."
  value       = module.rds.database_url
  sensitive   = true
}

output "rds_endpoint" {
  description = "Database endpoint host:port."
  value       = module.rds.endpoint
}

output "inference_backend" {
  description = "Gateway Provider backend for hosted models on this cloud (bedrock | vertex)."
  value       = local.inference_backend
}

output "inference_region" {
  description = "Region the gateway calls the hosted-model backend in."
  value       = var.bedrock_region
}

output "bedrock_model_filter" {
  description = "Comma-separated catalog globs for the gateway's BEDROCK_MODEL_FILTER, derived from bedrock_model_patterns so the catalog matches the IAM allowlist. Empty when no Bedrock models are allowed."
  value       = local.bedrock_model_filter
}

output "model_name" {
  description = "Hugging Face model ID served by vLLM."
  value       = var.model_name
}

output "served_model_name" {
  description = "Short model name exposed on the OpenAI-compatible API."
  value       = var.served_model_name
}
