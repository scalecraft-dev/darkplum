# The CLI refuses to render the gateway without the inference_region output (the
# Makefile fell back to a hard-coded us-east-1). That is only safe if a fresh
# apply always sets it, including when no Bedrock model is allowed. The mock
# providers are a copy of tests/mock_providers.hcl; keep them equal.

mock_provider "aws" {
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[]}"
    }
  }
  mock_data "aws_caller_identity" {
    defaults = {
      account_id = "123456789012"
      arn        = "arn:aws:iam::123456789012:user/mock"
    }
  }
  mock_data "aws_iam_session_context" {
    defaults = {
      issuer_arn = "arn:aws:iam::123456789012:role/mock"
    }
  }
  mock_data "aws_partition" {
    defaults = {
      partition  = "aws"
      dns_suffix = "amazonaws.com"
    }
  }
  mock_data "aws_region" {
    defaults = {
      name = "us-east-2"
    }
  }
  mock_data "aws_route53_zone" {
    defaults = {
      zone_id = "Z0PARENTMOCK"
    }
  }
}

mock_provider "random" {}

variables {
  aws_region          = "us-east-2"
  azs                 = ["us-east-2a", "us-east-2b", "us-east-2c"]
  allowed_account_ids = ["123456789012"]
  cluster_name        = "acme"
}

run "inference_region_defaults_to_the_cluster_region" {
  command = plan

  assert {
    condition     = output.inference_region == "us-east-2"
    error_message = "with bedrock_region unset the output must be aws_region, never empty."
  }
}

run "inference_region_follows_bedrock_region" {
  command = plan

  variables {
    bedrock_region = "us-east-1"
  }

  assert {
    condition     = output.inference_region == "us-east-1"
    error_message = "an explicit bedrock_region must be the output."
  }
}

run "inference_region_is_set_with_no_bedrock_models_allowed" {
  command = plan

  variables {
    bedrock_model_patterns = []
  }

  assert {
    condition     = output.inference_region == "us-east-2" && output.bedrock_model_filter == ""
    error_message = "no allowed Bedrock models leaves the filter empty, not the region."
  }
}
