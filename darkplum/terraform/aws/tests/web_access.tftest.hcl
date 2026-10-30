# Offline plan tests of the private web address (modules/web-access): when it
# exists, what it manages, and the NLB addresses configure hands to nginx.
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
}

run "off_without_a_domain" {
  command = plan

  assert {
    condition     = length(module.web_access) == 0
    error_message = "no web_domain must create nothing (installs from before the private address)"
  }
  assert {
    condition     = output.web_url == "" && length(output.web_lb_private_ips) == 0
    error_message = "outputs must be empty without web_domain"
  }
  assert {
    condition     = length(module.vpc.reserved_lb_cidrs) == 0
    error_message = "an install without web_domain must reserve nothing in its subnets (no change to existing installs)"
  }
}

run "created_zone_and_certificate" {
  command = plan

  variables {
    web_domain = "acme.com"
  }


  assert {
    condition     = length(module.web_access) == 1 && module.web_access[0].manages_zone && module.web_access[0].manages_certificate
    error_message = "a domain alone must create the zone and the certificate"
  }
  assert {
    condition     = output.web_url == "https://darkplum.acme.com"
    error_message = "default hostname must be darkplum.<web_domain>"
  }
  assert {
    condition     = output.web_lb_private_ips == tolist(["10.0.100.250", "10.0.101.250", "10.0.102.250"])
    error_message = "the NLB addresses must be pinned per private subnet and known at plan time"
  }
  assert {
    condition     = output.web_allowed_cidrs == tolist(["10.0.0.0/16"])
    error_message = "no allowed networks must mean the VPC only"
  }
  assert {
    condition     = module.vpc.reserved_lb_cidrs == tolist(["10.0.100.250/32", "10.0.101.250/32", "10.0.102.250/32"])
    error_message = "every pinned NLB address must be reserved in its subnet, in subnet order, by the vpc module"
  }
}

run "supplied_zone_and_certificate" {
  command = plan


  variables {
    web_domain          = "acme.com"
    web_hostname        = "chat.acme.com"
    web_zone_id         = "Z0123456789ABCDEFGHIJ"
    web_certificate_arn = "arn:aws:acm:us-east-2:123456789012:certificate/0b8f2a3c-1d2e-4f5a-8b9c-0d1e2f3a4b5c"
    web_allowed_cidrs   = ["10.8.0.0/16", "192.168.4.0/24"]
  }

  assert {
    condition     = !module.web_access[0].manages_zone && !module.web_access[0].manages_certificate
    error_message = "a supplied zone or certificate must never be managed"
  }
  assert {
    condition     = output.web_url == "https://chat.acme.com" && output.web_allowed_cidrs == tolist(["10.8.0.0/16", "192.168.4.0/24"])
    error_message = "web_hostname and web_allowed_cidrs must be used as given"
  }
}

# The module on its own: subnet IDs and reserved addresses that do not line up
# are refused.
run "subnets_and_addresses_must_line_up" {
  command = plan

  module {
    source = "./modules/web-access"
  }

  variables {
    name                        = "darkplum"
    hostname                    = "darkplum.acme.com"
    domain                      = "acme.com"
    allowed_cidrs               = ["10.0.0.0/16"]
    vpc_id                      = "vpc-0123"
    private_subnet_ids          = ["subnet-a", "subnet-b"]
    lb_private_ips              = ["10.0.100.250"]
    node_security_group_id      = "sg-0123"
    node_autoscaling_group_name = "asg"
  }


  expect_failures = [aws_lb.web]
}

run "module_refuses_public_networks" {
  command = plan

  module {
    source = "./modules/web-access"
  }

  variables {
    name                        = "darkplum"
    hostname                    = "darkplum.acme.com"
    domain                      = "acme.com"
    allowed_cidrs               = ["0.0.0.0/1", "128.0.0.0/1"]
    vpc_id                      = "vpc-0123"
    private_subnet_ids          = ["subnet-a", "subnet-b"]
    lb_private_ips              = ["10.0.100.250", "10.0.101.250"]
    node_security_group_id      = "sg-0123"
    node_autoscaling_group_name = "asg"
  }


  expect_failures = [var.allowed_cidrs]
}
