# The Library's bucket: every library item as a plain file under library/v1/.
#
# Layer 1 for this data is in the cloud's own primitives: no public access, TLS
# only, versioning with a bounded history, encrypted at rest, and an explicit
# deny for the gateway (it never reads library content; the api injects it).
# Who may open the bucket besides the api is the account's own IAM, so an
# administrator can browse and sync it from the console. The application's
# checks on who owns an item are Layer 2.

locals {
  bucket_arn = "arn:${var.partition}:s3:::${var.bucket_name}"
  objects    = "${local.bucket_arn}/*"
}

resource "aws_s3_bucket" "this" {
  bucket        = var.bucket_name
  force_destroy = var.force_destroy
  tags          = var.tags

  lifecycle {
    precondition {
      condition     = length(var.bucket_name) >= 3 && length(var.bucket_name) <= 63
      error_message = "The library bucket name ${var.bucket_name} is ${length(var.bucket_name)} characters; S3 allows 3 to 63. Use a shorter cluster_name."
    }
  }
}

resource "aws_s3_bucket_public_access_block" "this" {
  bucket = aws_s3_bucket.this.id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_ownership_controls" "this" {
  bucket = aws_s3_bucket.this.id

  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}

resource "aws_s3_bucket_versioning" "this" {
  bucket = aws_s3_bucket.this.id

  versioning_configuration {
    status = "Enabled"
  }
}

# Versioning is the history and the accidental-delete recovery; the lifecycle
# bounds how long a replaced or deleted version stays. No Object Lock: deleting
# a person's item must be able to purge every version.
resource "aws_s3_bucket_lifecycle_configuration" "this" {
  bucket = aws_s3_bucket.this.id

  rule {
    id     = "expire-noncurrent-versions"
    status = "Enabled"

    filter {}

    noncurrent_version_expiration {
      noncurrent_days = var.noncurrent_days
    }

    expiration {
      expired_object_delete_marker = true
    }

    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  depends_on = [aws_s3_bucket_versioning.this]
}

# SSE-S3 unless the customer brings a key.
resource "aws_s3_bucket_server_side_encryption_configuration" "this" {
  bucket = aws_s3_bucket.this.id

  rule {
    bucket_key_enabled = true

    apply_server_side_encryption_by_default {
      sse_algorithm     = var.kms_key_arn == null ? "AES256" : "aws:kms"
      kms_master_key_id = var.kms_key_arn
    }
  }
}

resource "aws_s3_bucket_policy" "this" {
  bucket = aws_s3_bucket.this.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat(
      [
        {
          Sid       = "DenyInsecureTransport"
          Effect    = "Deny"
          Principal = "*"
          Action    = "s3:*"
          Resource  = [local.bucket_arn, local.objects]
          Condition = { Bool = { "aws:SecureTransport" = "false" } }
        }
      ],
      length(var.gateway_role_arns) == 0 ? [] : [
        {
          Sid       = "DenyGateway"
          Effect    = "Deny"
          Principal = { AWS = var.gateway_role_arns }
          Action    = "s3:*"
          Resource  = [local.bucket_arn, local.objects]
        }
      ]
    )
  })

  # The public-access block lands first so the policy is never evaluated against
  # a bucket that could still be opened up.
  depends_on = [aws_s3_bucket_public_access_block.this]
}
