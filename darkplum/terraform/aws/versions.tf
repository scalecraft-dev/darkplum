terraform {
  required_version = ">= 1.10.0"

  # State lives in S3, in the customer's account. Only the backend TYPE is in the
  # code: the bucket, key, region and KMS key come from backend.hcl, written by
  # `darkplum init` (terraform init -backend-config=backend.hcl). The code that
  # ships inside the darkplum binary therefore never names a customer bucket, and
  # `terraform init -backend=false` (validate, test) needs nothing.
  backend "s3" {}

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.80"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
