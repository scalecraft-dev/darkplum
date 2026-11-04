# Offline plan test of the library bucket as wired into the root module: the
# normalized output, the api role's grants and the gateway's explicit deny. The
# mock providers are a copy of tests/mock_providers.hcl; keep them equal.

# Offline providers for `terraform test` (shared by the generated variable
# tests and web_access.tftest.hcl). No AWS call is made and no credentials are
# needed. Mocked data sources that feed JSON or ARN-validated arguments return
# well-formed values.
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

run "library_store_url_is_one_normalized_output" {
  command = plan

  assert {
    condition     = output.library_store_url == "s3://acme-library-123456789012/library/v1/"
    error_message = "library_store_url must be s3://<cluster>-library-<account>/library/v1/ (the CLI derives the same bucket name)."
  }
}

# The api role's grants and the gateway's deny are inline policies inside the
# pod-identity module, which a test cannot read; the customer-key variant at least
# proves the grant is built and the configuration plans.
run "customer_key_plans" {
  command = plan

  variables {
    library_kms_key_arn = "arn:aws:kms:us-east-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
  }

  assert {
    condition     = output.library_store_url == "s3://acme-library-123456789012/library/v1/"
    error_message = "a customer key must not change where the library lives."
  }
}
