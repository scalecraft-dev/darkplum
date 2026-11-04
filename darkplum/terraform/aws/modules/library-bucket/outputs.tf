output "bucket_name" {
  value = aws_s3_bucket.this.id
}

output "bucket_arn" {
  value = aws_s3_bucket.this.arn
}

output "store_url" {
  description = "The normalized library location the api is given."
  value       = "s3://${var.bucket_name}/library/v1/"
}
