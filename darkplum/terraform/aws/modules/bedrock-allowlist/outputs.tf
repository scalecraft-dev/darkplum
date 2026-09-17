output "invoke_arns" {
  description = "Exact ARNs for the bedrock:InvokeModel* statement. Empty when no patterns are allowed."
  value       = local.invoke_arns
}

output "model_filter" {
  description = "Comma-separated catalog globs for the gateway's BEDROCK_MODEL_FILTER."
  value       = local.model_filter
}

output "destination_regions" {
  description = "Regions the foundation-model ARNs cover."
  value       = local.destination_regions
}
