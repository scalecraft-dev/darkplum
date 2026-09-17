resource "aws_iam_role" "this" {
  name = var.role_name

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "pods.eks.amazonaws.com"
      }
      Action = [
        "sts:AssumeRole",
        "sts:TagSession"
      ]
      # Pod Identity injects kubernetes-namespace and kubernetes-service-account
      # as session tags. These conditions enforce that only the intended SA in the
      # intended namespace can use this role — equivalent to IRSA's OIDC subject
      # claim scoping, but at the IAM policy layer instead of the OIDC trust layer.
      Condition = {
        StringEquals = {
          "aws:PrincipalTag/kubernetes-namespace"       = var.namespace
          "aws:PrincipalTag/kubernetes-service-account" = var.service_account_name
        }
      }
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "this" {
  for_each = toset(var.policy_arns)

  policy_arn = each.value
  role       = aws_iam_role.this.name
}

resource "aws_iam_role_policy" "inline" {
  for_each = var.inline_policies

  name   = each.key
  role   = aws_iam_role.this.id
  policy = each.value
}

resource "aws_eks_pod_identity_association" "this" {
  cluster_name    = var.cluster_name
  namespace       = var.namespace
  service_account = var.service_account_name
  role_arn        = aws_iam_role.this.arn

  tags = var.tags
}
