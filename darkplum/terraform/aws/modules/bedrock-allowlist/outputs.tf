output "invoke_arns" {
  description = "Exact ARNs for the bedrock:InvokeModel* statement. Empty when no patterns are allowed."
  value       = local.invoke_arns

  # A geo inference profile can only be invoked from a region inside that
  # geography. With bedrock_region defaulting to the cluster region, a cluster
  # in eu-west-1 with the default "us" geo would otherwise be granted ARNs
  # that can never work — fail the plan instead.
  precondition {
    condition     = contains(local.geo_regions[var.geo], var.region)
    error_message = "bedrock_region (${var.region}) is not a ${var.geo} Bedrock region (${join(", ", local.geo_regions[var.geo])}). Set bedrock_region to one of those, or change bedrock_inference_geo to match the cluster's geography."
  }
}

output "model_filter" {
  description = "Comma-separated catalog globs for the gateway's BEDROCK_MODEL_FILTER."
  value       = local.model_filter
}

output "destination_regions" {
  description = "Regions the foundation-model ARNs cover."
  value       = local.destination_regions
}
