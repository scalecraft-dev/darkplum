# Offline plan test of the first-party image repositories (modules/ecr) as wired
# into the root module. The mock providers are a copy of tests/mock_providers.hcl;
# keep them equal.

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

run "first_party_repositories_are_owned_by_terraform" {
  command = plan

  assert {
    condition     = join(",", sort(keys(output.image_repositories))) == "api,gateway,web"
    error_message = "image_repositories must name exactly the three first-party services (the CLI mirrors into these)."
  }
  assert {
    condition     = length(module.ecr.repository_arns) == 3 && output.container_registry == "123456789012.dkr.ecr.us-east-2.amazonaws.com"
    error_message = "every repository must exist in the plan, on the account's registry host."
  }
}
