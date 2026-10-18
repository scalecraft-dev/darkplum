output "endpoint" {
  value = aws_db_instance.this.endpoint
}

output "database_url" {
  value     = "postgres://${var.db_username}:${random_password.db.result}@${aws_db_instance.this.endpoint}/${var.db_name}?sslmode=require"
  sensitive = true
}

output "password" {
  value     = random_password.db.result
  sensitive = true
}

output "final_snapshot_identifier" {
  description = "Name of the snapshot taken when the instance is destroyed; null when skip_final_snapshot is true."
  value       = local.final_snapshot_identifier
}
