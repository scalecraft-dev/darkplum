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

output "vpc_cidr" {
  description = "CIDR of the cluster's VPC. The gateway-egress NetworkPolicy scopes its database rule (Postgres :5432) to it."
  value       = var.vpc_cidr
}

output "kube_config_command" {
  description = "Deprecated, removed with the Makefile flow: it points the operator's own ~/.kube/config at the cluster. The darkplum CLI never does that; it writes a private kubeconfig per install."
  value       = "aws eks update-kubeconfig --region ${var.aws_region} --name ${module.eks.cluster_name}"
}

output "container_registry" {
  description = "Container registry host the first-party image repositories live on."
  value       = "${data.aws_caller_identity.current.account_id}.dkr.ecr.${var.aws_region}.amazonaws.com"
}

output "image_repositories" {
  description = "Service name -> ECR repository URL (<cluster_name>/darkplum-<service>) that `darkplum deploy` mirrors the verified release images into."
  value       = module.ecr.repository_urls
}

output "cluster_service_cidr" {
  description = "The cluster's Kubernetes service CIDR. The gateway-egress-services NetworkPolicy scopes its rule to it, so the CLI never has to ask EKS."
  value       = module.eks.cluster_service_cidr
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

output "rds_final_snapshot_identifier" {
  description = "Snapshot a destroy leaves behind (null when db_skip_final_snapshot = true)."
  value       = module.rds.final_snapshot_identifier
}

output "inference_backend" {
  description = "Gateway Provider backend for hosted models on this cloud (bedrock | vertex)."
  value       = local.inference_backend
}

output "inference_region" {
  description = "Region the gateway calls the hosted-model backend in."
  value       = local.bedrock_region
}

output "bedrock_model_filter" {
  description = "Comma-separated catalog globs for the gateway's BEDROCK_MODEL_FILTER, derived from bedrock_model_patterns so the catalog matches the IAM allowlist. Empty when no Bedrock models are allowed."
  value       = local.bedrock_model_filter
}

output "gpu_desired_size" {
  description = "Number of GPU nodes. 0 = cloud-managed models only; the deploy step skips self-hosted vLLM."
  value       = var.gpu_desired_size
}

output "model_name" {
  description = "Hugging Face model ID served by vLLM."
  value       = var.model_name
}

output "model_revision" {
  description = "Hugging Face revision of the default self-hosted model."
  value       = var.model_revision
}

output "bedrock_invoke_arns" {
  description = "The exact ARNs the gateway role may invoke (Layer 1 allowlist). Used by `make verify-bedrock-iam`."
  value       = local.bedrock_invoke_arns
}

output "bedrock_transport" {
  description = "How the gateway reaches Bedrock: privatelink (interface VPC endpoints, in-region) or nat (public endpoint via the NAT gateway)."
  value       = local.bedrock_transport
}

output "gateway_role_arn" {
  description = "IAM role the gateway assumes via EKS Pod Identity."
  value       = module.pod_identity_gateway.role_arn
}

output "served_model_name" {
  description = "Short model name exposed on the OpenAI-compatible API."
  value       = var.served_model_name
}

# --- Private web address (empty when web_domain is not set) ---

output "web_url" {
  description = "The team's address for the web UI (reachable from web_allowed_cidrs)."
  value       = var.web_domain != "" ? module.web_access[0].url : ""
}

output "web_hostname" {
  description = "Host name of web_url."
  value       = var.web_domain != "" ? module.web_access[0].hostname : ""
}

output "web_lb_private_ips" {
  description = "The internal NLB's pinned private addresses. `darkplum configure` writes them into nginx's trusted-proxy list and the web-ingress NetworkPolicy."
  value       = var.web_domain != "" ? module.web_access[0].lb_private_ips : []
}

output "web_name_servers" {
  description = "Name servers of the hosted zone Terraform created for the web hostname (empty when the zone was supplied)."
  value       = var.web_domain != "" ? module.web_access[0].name_servers : []
}

output "web_allowed_cidrs" {
  description = "Networks allowed to reach web_url."
  value       = var.web_domain != "" ? module.web_access[0].allowed_cidrs : []
}
