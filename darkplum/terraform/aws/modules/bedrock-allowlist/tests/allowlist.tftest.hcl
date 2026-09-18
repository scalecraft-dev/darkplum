# Offline: no providers, no AWS. These assert the SHAPE of the Layer 1 boundary.
# `make verify-bedrock-iam` asserts the deployed role's actual behaviour.

variables {
  account_id     = "123456789012"
  region         = "us-east-1"
  geo            = "us"
  model_patterns = ["anthropic.claude-*", "meta.llama*", "mistral.*"]
}

run "us_geo_is_region_enumerated" {
  command = plan

  assert {
    condition     = alltrue([for a in output.invoke_arns : !can(regex("^arn:aws:bedrock:\\*:", a))])
    error_message = "No ARN may wildcard the region."
  }
  assert {
    condition     = alltrue([for a in output.invoke_arns : !can(regex("inference-profile/(global|\\*)\\.", a))])
    error_message = "global.* and wildcard-geo inference profiles must never be granted."
  }
  assert {
    condition     = alltrue([for a in output.invoke_arns : can(regex("^arn:aws:bedrock:(us-east-1|us-east-2|us-west-2):", a))])
    error_message = "us geo must only reference us-east-1, us-east-2, us-west-2."
  }
  assert {
    condition     = contains(output.invoke_arns, "arn:aws:bedrock:us-east-1:123456789012:inference-profile/us.meta.llama*")
    error_message = "Open-weight Llama geo profile should be granted."
  }
  assert {
    condition     = contains(output.invoke_arns, "arn:aws:bedrock:us-west-2::foundation-model/meta.llama*")
    error_message = "Foundation-model ARN must exist for each destination region."
  }
  assert {
    condition     = length(output.invoke_arns) == 3 * 3 + 3
    error_message = "Expected regions x patterns foundation-model ARNs plus one profile ARN per pattern."
  }
  assert {
    condition     = output.model_filter == "us.anthropic.claude-*,us.meta.llama*,us.mistral.*"
    error_message = "Catalog filter must mirror the IAM patterns with the geo prefix."
  }
}

run "eu_geo_never_references_us_regions" {
  command = plan

  variables {
    geo            = "eu"
    region         = "eu-central-1"
    model_patterns = ["anthropic.claude-*"]
  }

  assert {
    condition     = alltrue([for a in output.invoke_arns : can(regex("^arn:aws:bedrock:eu-", a))])
    error_message = "eu geo must only reference eu-* regions."
  }
  assert {
    condition     = contains(output.invoke_arns, "arn:aws:bedrock:eu-central-1:123456789012:inference-profile/eu.anthropic.claude-*")
    error_message = "Expected the eu geo inference-profile ARN."
  }
}

run "calling_region_is_always_covered" {
  command = plan

  variables {
    destination_regions = ["us-west-2"]
  }

  assert {
    condition     = contains(output.destination_regions, "us-east-1") && contains(output.destination_regions, "us-west-2") && length(output.destination_regions) == 2
    error_message = "An override narrows the regions but the calling region is always included."
  }
}

run "empty_patterns_grant_nothing" {
  command = plan

  variables {
    model_patterns = []
  }

  assert {
    condition     = length(output.invoke_arns) == 0 && output.model_filter == ""
    error_message = "No patterns must mean no invocable ARNs and an empty catalog filter."
  }
}

run "rejects_arns_as_patterns" {
  command = plan

  variables {
    model_patterns = ["arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3"]
  }

  expect_failures = [var.model_patterns]
}

run "rejects_global_geo" {
  command = plan

  variables {
    geo = "global"
  }

  expect_failures = [var.geo]
}

run "rejects_region_outside_geo" {
  command = plan

  variables {
    region = "eu-west-1" # cluster in Europe, geo left at the default "us"
  }

  expect_failures = [output.invoke_arns]
}
