# Offline: the aws provider is mocked, no AWS call is made. These assert the SHAPE
# of the library bucket's Layer 1 boundary. `darkplum verify` asserts the deployed
# roles' actual behaviour.

mock_provider "aws" {}

variables {
  bucket_name       = "acme-library-123456789012"
  gateway_role_arns = ["arn:aws:iam::123456789012:role/acme-gateway"]
}

run "bucket_is_private_owned_versioned_and_encrypted" {
  command = plan

  assert {
    condition = (aws_s3_bucket_public_access_block.this.block_public_acls
      && aws_s3_bucket_public_access_block.this.block_public_policy
      && aws_s3_bucket_public_access_block.this.ignore_public_acls
    && aws_s3_bucket_public_access_block.this.restrict_public_buckets)
    error_message = "All four public-access blocks must be on."
  }
  assert {
    condition     = one(aws_s3_bucket_ownership_controls.this.rule).object_ownership == "BucketOwnerEnforced"
    error_message = "ACLs must be disabled: the bucket owner owns every object."
  }
  assert {
    condition     = one(aws_s3_bucket_versioning.this.versioning_configuration).status == "Enabled"
    error_message = "Versioning is the history and the delete recovery."
  }
  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.this.rule).apply_server_side_encryption_by_default).sse_algorithm == "AES256"
    error_message = "Default encryption is SSE-S3 unless a customer key is given."
  }
  assert {
    condition     = one(aws_s3_bucket_server_side_encryption_configuration.this.rule).bucket_key_enabled
    error_message = "The bucket key stays on."
  }
  assert {
    condition     = aws_s3_bucket.this.force_destroy == false
    error_message = "force_destroy must default to false: the bucket holds customer data."
  }
}

run "lifecycle_expires_old_versions_and_cleans_up" {
  command = plan

  assert {
    condition     = one(one(aws_s3_bucket_lifecycle_configuration.this.rule).noncurrent_version_expiration).noncurrent_days == 30
    error_message = "Noncurrent versions expire after 30 days by default."
  }
  assert {
    condition     = one(one(aws_s3_bucket_lifecycle_configuration.this.rule).expiration).expired_object_delete_marker
    error_message = "Orphaned delete markers must expire."
  }
  assert {
    condition     = one(one(aws_s3_bucket_lifecycle_configuration.this.rule).abort_incomplete_multipart_upload).days_after_initiation == 7
    error_message = "Incomplete multipart uploads are aborted after 7 days."
  }
}

run "lifecycle_days_are_a_variable" {
  command = plan

  variables {
    noncurrent_days = 90
  }

  assert {
    condition     = one(one(aws_s3_bucket_lifecycle_configuration.this.rule).noncurrent_version_expiration).noncurrent_days == 90
    error_message = "noncurrent_days must reach the lifecycle rule."
  }
}

run "policy_denies_plain_http_and_the_gateway" {
  command = plan

  assert {
    condition = anytrue([for s in jsondecode(aws_s3_bucket_policy.this.policy).Statement :
      s.Effect == "Deny" && try(s.Principal == "*", false) && s.Action == "s3:*" && try(s.Condition.Bool["aws:SecureTransport"], "") == "false"
      && contains(s.Resource, "arn:aws:s3:::acme-library-123456789012") && contains(s.Resource, "arn:aws:s3:::acme-library-123456789012/*")
    ])
    error_message = "Requests without TLS must be denied on the bucket and its objects."
  }
  assert {
    condition = anytrue([for s in jsondecode(aws_s3_bucket_policy.this.policy).Statement :
      s.Effect == "Deny" && s.Action == "s3:*" && try(s.Principal.AWS, []) == ["arn:aws:iam::123456789012:role/acme-gateway"]
      && contains(s.Resource, "arn:aws:s3:::acme-library-123456789012/*")
    ])
    error_message = "The gateway role must be explicitly denied every action on the bucket."
  }
  assert {
    condition     = length(jsondecode(aws_s3_bucket_policy.this.policy).Statement) == 2
    error_message = "The bucket policy holds exactly the two denies: anyone else's access is the account's own IAM."
  }
}

run "customer_key_switches_to_kms" {
  command = plan

  variables {
    kms_key_arn = "arn:aws:kms:us-east-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
  }

  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.this.rule).apply_server_side_encryption_by_default).sse_algorithm == "aws:kms"
    error_message = "A customer key means SSE-KMS."
  }
  assert {
    condition     = one(one(aws_s3_bucket_server_side_encryption_configuration.this.rule).apply_server_side_encryption_by_default).kms_master_key_id == "arn:aws:kms:us-east-2:123456789012:key/1234abcd-12ab-34cd-56ef-1234567890ab"
    error_message = "The bucket must use the customer's key."
  }
}

run "rejects_a_key_alias" {
  command = plan

  variables {
    kms_key_arn = "alias/mine"
  }

  expect_failures = [var.kms_key_arn]
}

run "rejects_a_bucket_name_over_63_characters" {
  command = plan

  variables {
    bucket_name = "a-cluster-name-that-is-far-too-long-for-s3-at-all-library-123456789012"
  }

  expect_failures = [aws_s3_bucket.this]
}

run "force_destroy_is_opt_in" {
  command = plan

  variables {
    force_destroy = true
  }

  assert {
    condition     = aws_s3_bucket.this.force_destroy
    error_message = "force_destroy must follow the variable."
  }
}
