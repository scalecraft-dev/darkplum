resource "random_password" "db" {
  length  = 24
  special = false
}

resource "aws_db_subnet_group" "this" {
  name       = "${var.name}-db"
  subnet_ids = var.subnet_ids
  tags       = var.tags
}

resource "aws_security_group" "db" {
  name_prefix = "${var.name}-db-"
  vpc_id      = var.vpc_id
  description = "Allow PostgreSQL access from EKS nodes"

  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = var.allowed_security_group_ids
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

# Suffix for the final snapshot's name. A snapshot identifier must be unique in
# the account, and a fixed name would make the second destroy of a re-created
# instance fail. No keepers on purpose: the id lives and dies with the rest
# of this module's state, so every create/destroy cycle names its own
# snapshot and never collides with the one the previous cycle left behind.
resource "random_id" "final_snapshot" {
  byte_length = 4
}

locals {
  final_snapshot_identifier = var.skip_final_snapshot ? null : "${var.name}-db-final-${random_id.final_snapshot.hex}"
}

resource "aws_db_instance" "this" {
  identifier = "${var.name}-db"

  engine         = "postgres"
  engine_version = "17.10"
  instance_class = var.instance_class

  allocated_storage = var.allocated_storage
  storage_type      = "gp3"
  storage_encrypted = true

  db_name  = var.db_name
  username = var.db_username
  password = random_password.db.result

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.db.id]

  multi_az            = false
  publicly_accessible = false

  # Durability. The database holds every conversation, library item, policy and
  # setting the platform has -- the whole point of running Darkplum is that this
  # data is the customer's and survives. So: a week of automated backups (PITR
  # within the window), a final snapshot on destroy, and deletion protection so
  # neither `terraform destroy` nor a console click removes the instance until
  # an operator deliberately turns protection off (make destroy does this
  # itself, right before the destroy, see Makefile).
  backup_retention_period   = var.backup_retention_period
  deletion_protection       = var.deletion_protection
  skip_final_snapshot       = var.skip_final_snapshot
  final_snapshot_identifier = local.final_snapshot_identifier
  copy_tags_to_snapshot     = true
  # Without this, AWS defers a retention change to the next maintenance
  # window (up to a week) and the instance keeps 1-day backups meanwhile.
  # None of the settings above restarts the instance; a future instance_class
  # change would also apply at once, which is the trade-off being made.
  apply_immediately = true

  tags = var.tags
}
