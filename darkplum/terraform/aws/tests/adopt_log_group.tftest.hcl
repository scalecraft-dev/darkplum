# A smoke check only: the configuration still plans with adopt_eks_log_group set.
# It passes without the import block too, because a mocked plan cannot assert a
# planned import. What guards the block is TestImportBlockAdoptsTheLogGroupDeployChecks
# in the CLI, and the real plan and apply in E2E-RELEASE-CHECK.md row 8.
# Run: terraform init -backend=false && terraform test
# The mock providers are a copy of tests/mock_providers.hcl; keep them equal.

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

run "plan_without_adoption_creates_the_log_group" {
  command = plan

  assert {
    condition     = output.inference_region == "us-east-2"
    error_message = "the plan of a fresh install evaluates."
  }
}

run "plan_with_adoption_evaluates_against_an_empty_state" {
  command = plan

  variables {
    adopt_eks_log_group = true
  }

  # A mock provider cannot import: this stands in for the group AWS returns.
  override_resource {
    target = module.eks.module.eks.aws_cloudwatch_log_group.this[0]
    values = {
      name = "/aws/eks/acme/cluster"
    }
  }

  assert {
    condition     = output.inference_region == "us-east-2"
    error_message = "a plan that imports the kept log group must evaluate against an empty state."
  }
}
