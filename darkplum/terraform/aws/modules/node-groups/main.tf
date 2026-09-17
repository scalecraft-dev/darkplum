resource "aws_eks_node_group" "cpu" {
  cluster_name    = var.cluster_name
  node_group_name = "${var.cluster_name}-cpu-system"
  node_role_arn   = aws_iam_role.cpu_node.arn
  subnet_ids      = var.subnet_ids
  instance_types  = var.cpu_instance_types

  scaling_config {
    desired_size = var.cpu_desired_size
    min_size     = 1
    max_size     = var.cpu_desired_size + 1
  }

  labels = {
    "darkplum.ai/node-type" = "system"
  }

  tags = var.tags

  depends_on = [
    aws_iam_role_policy_attachment.cpu_node_AmazonEKSWorkerNodePolicy,
    aws_iam_role_policy_attachment.cpu_node_AmazonEKS_CNI_Policy,
    aws_iam_role_policy_attachment.cpu_node_AmazonEC2ContainerRegistryReadOnly,
  ]
}

resource "aws_launch_template" "gpu" {
  name_prefix = "${var.cluster_name}-gpu-"

  block_device_mappings {
    device_name = "/dev/xvda"
    ebs {
      volume_size = var.gpu_disk_size
      volume_type = "gp3"
    }
  }

  tag_specifications {
    resource_type = "instance"
    tags          = var.tags
  }
}

resource "aws_eks_node_group" "gpu" {
  cluster_name    = var.cluster_name
  node_group_name = "${var.cluster_name}-gpu-inference"
  node_role_arn   = aws_iam_role.gpu_node.arn
  subnet_ids      = var.subnet_ids
  instance_types  = var.gpu_instance_types
  ami_type        = "AL2023_x86_64_NVIDIA"

  launch_template {
    id      = aws_launch_template.gpu.id
    version = aws_launch_template.gpu.latest_version
  }

  scaling_config {
    desired_size = var.gpu_desired_size
    min_size     = var.gpu_min_size
    max_size     = var.gpu_max_size
  }

  labels = {
    "darkplum.ai/node-type" = "gpu-inference"
  }

  taint {
    key    = "nvidia.com/gpu"
    value  = "true"
    effect = "NO_SCHEDULE"
  }

  tags = var.tags

  depends_on = [
    aws_iam_role_policy_attachment.gpu_node_AmazonEKSWorkerNodePolicy,
    aws_iam_role_policy_attachment.gpu_node_AmazonEKS_CNI_Policy,
    aws_iam_role_policy_attachment.gpu_node_AmazonEC2ContainerRegistryReadOnly,
  ]
}

resource "aws_iam_role" "cpu_node" {
  name = "${var.cluster_name}-cpu-node"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })

  tags = var.tags
}

resource "aws_iam_role" "gpu_node" {
  name = "${var.cluster_name}-gpu-node"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
    }]
  })

  tags = var.tags
}

resource "aws_iam_role_policy_attachment" "cpu_node_AmazonEKSWorkerNodePolicy" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
  role       = aws_iam_role.cpu_node.name
}

resource "aws_iam_role_policy_attachment" "cpu_node_AmazonEKS_CNI_Policy" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
  role       = aws_iam_role.cpu_node.name
}

resource "aws_iam_role_policy_attachment" "cpu_node_AmazonEC2ContainerRegistryReadOnly" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
  role       = aws_iam_role.cpu_node.name
}

resource "aws_iam_role_policy_attachment" "gpu_node_AmazonEKSWorkerNodePolicy" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSWorkerNodePolicy"
  role       = aws_iam_role.gpu_node.name
}

resource "aws_iam_role_policy_attachment" "gpu_node_AmazonEKS_CNI_Policy" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKS_CNI_Policy"
  role       = aws_iam_role.gpu_node.name
}

resource "aws_iam_role_policy_attachment" "gpu_node_AmazonEC2ContainerRegistryReadOnly" {
  policy_arn = "arn:aws:iam::aws:policy/AmazonEC2ContainerRegistryReadOnly"
  role       = aws_iam_role.gpu_node.name
}
