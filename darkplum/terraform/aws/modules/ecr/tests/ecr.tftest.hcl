# Offline: the aws provider is mocked, no AWS call is made. These assert the SHAPE
# of the first-party image repositories the CLI mirrors releases into.

mock_provider "aws" {}

variables {
  cluster_name = "acme"
}

run "repositories_are_per_cluster_immutable_and_deletable" {
  command = plan

  assert {
    condition     = join(",", sort([for r in aws_ecr_repository.this : r.name])) == "acme/darkplum-api,acme/darkplum-gateway,acme/darkplum-web"
    error_message = "Repositories must be <cluster_name>/darkplum-<service>: two installs in one account must not share one."
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_tag_mutability == "IMMUTABLE"])
    error_message = "A pushed tag must never point at different bytes."
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.force_delete])
    error_message = "destroy must not stop on the images in a repository."
  }
  assert {
    condition     = alltrue([for r in aws_ecr_repository.this : r.image_scanning_configuration[0].scan_on_push])
    error_message = "Scan on push must stay on."
  }
}

run "lifecycle_keeps_tagged_images_and_expires_only_untagged" {
  command = plan

  assert {
    condition     = length(aws_ecr_lifecycle_policy.this) == 3
    error_message = "Every repository needs a lifecycle policy."
  }
  assert {
    condition     = jsondecode(aws_ecr_lifecycle_policy.this["api"].policy).rules[0].selection.tagStatus == "untagged"
    error_message = "Only untagged images may expire by age."
  }
  assert {
    condition     = jsondecode(aws_ecr_lifecycle_policy.this["api"].policy).rules[1].selection.countNumber == 20 && jsondecode(aws_ecr_lifecycle_policy.this["api"].policy).rules[2].selection.countNumber == 20
    error_message = "Default retention is the last 20 images of each version-tag kind."
  }
  # ECR ANDs the patterns in one rule, so a rule listing both would match nothing.
  assert {
    condition     = alltrue([for r in slice(jsondecode(aws_ecr_lifecycle_policy.this["api"].policy).rules, 1, 3) : length(r.selection.tagPatternList) == 1])
    error_message = "Each count-based rule must name exactly one tag pattern."
  }
  assert {
    condition     = join(",", [for r in slice(jsondecode(aws_ecr_lifecycle_policy.this["api"].policy).rules, 1, 3) : r.selection.tagPatternList[0]]) == "*.*.*,rc-*"
    error_message = "Count-based expiry must only ever match version tags (X.Y.Z and rc-N-sha)."
  }
}

run "retention_below_two_is_rejected" {
  command = plan

  variables {
    keep_tagged_images = 1
  }
  expect_failures = [var.keep_tagged_images]
}
