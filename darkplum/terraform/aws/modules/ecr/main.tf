# The three first-party image repositories, owned by Terraform so `darkplum
# destroy` removes them with the rest of the install and nothing is created
# out of band with an imperative `aws ecr create-repository`.
#
# IMMUTABLE: a tag, once pushed, can never point at different bytes. The CLI
# mirrors a release into these repositories by digest and aborts if a tag already
# exists at another digest.
#
# force_delete: destroy must not stop on the images in them; they are copies of
# signed release artifacts and can be mirrored again.
resource "aws_ecr_repository" "this" {
  for_each = var.services

  name                 = "${var.cluster_name}/darkplum-${each.key}"
  image_tag_mutability = "IMMUTABLE"
  force_delete         = true

  image_scanning_configuration {
    scan_on_push = true
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = var.tags
}

resource "aws_ecr_lifecycle_policy" "this" {
  for_each   = aws_ecr_repository.this
  repository = each.value.name

  # Untagged images are expired only because the mirror always pushes a version
  # tag with the manifest; nothing the install runs is ever untagged.
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 7 days"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 7
        }
        action = { type = "expire" }
      },
      # Count-based expiry applies only to version tags. ECR requires ALL patterns
      # in a rule's tagPatternList to match, so each pattern is its own rule. The
      # mirror pushes X.Y.Z (releases) and rc-N-sha (release candidates) and never
      # tags anything else, so nothing else is ever pruned by count. ECR patterns
      # only know "*".
      {
        rulePriority = 2
        description  = "Keep the last ${var.keep_tagged_images} release-tagged images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["*.*.*"]
          countType      = "imageCountMoreThan"
          countNumber    = var.keep_tagged_images
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 3
        description  = "Keep the last ${var.keep_tagged_images} release-candidate images"
        selection = {
          tagStatus      = "tagged"
          tagPatternList = ["rc-*"]
          countType      = "imageCountMoreThan"
          countNumber    = var.keep_tagged_images
        }
        action = { type = "expire" }
      }
    ]
  })
}
