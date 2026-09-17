output "cpu_node_group_name" {
  value = aws_eks_node_group.cpu.node_group_name
}

output "gpu_node_group_name" {
  value = aws_eks_node_group.gpu.node_group_name
}

output "cpu_node_role_arn" {
  value = aws_iam_role.cpu_node.arn
}

output "gpu_node_role_arn" {
  value = aws_iam_role.gpu_node.arn
}
