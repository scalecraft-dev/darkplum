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
      # What actually scopes this role is the pod identity association below
      # (cluster + namespace + service account -> this role) and who holds
      # iam:PassRole on it and eks:CreatePodIdentityAssociation: anyone with both
      # can create another association to this role from any cluster in the
      # account that trusts pods.eks.amazonaws.com. Keep those permissions tight.
      #
      # The conditions here narrow the trust to this cluster, namespace and
      # service account. Pod Identity passes them as session tags, which a trust
      # policy sees as aws:RequestTag/* (there are no principal tags on the
      # pods.eks.amazonaws.com caller: aws:PrincipalTag/* denied every
      # AssumeRoleForPodIdentity). aws:SourceAccount pins the calling account.
      Condition = {
        StringEquals = {
          "aws:SourceAccount"                         = var.account_id
          "aws:RequestTag/eks-cluster-arn"            = var.cluster_arn
          "aws:RequestTag/kubernetes-namespace"       = var.namespace
          "aws:RequestTag/kubernetes-service-account" = var.service_account_name
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
