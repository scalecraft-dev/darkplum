output "vpc_id" {
  value = module.vpc.vpc_id
}

output "private_subnets" {
  value = module.vpc.private_subnets
  # Whatever is placed in the private subnets (EKS ENIs, nodes, RDS, endpoints)
  # waits for the web NLB's address reservations. No-op when there are none.
  depends_on = [aws_ec2_subnet_cidr_reservation.lb]
}

output "public_subnets" {
  value = module.vpc.public_subnets
}

output "lb_private_ips" {
  description = "The web NLB's reserved addresses, in private-subnet order (empty unless reserve_lb_ips). Read from the reservations, so whatever uses them waits for them."
  value       = [for r in aws_ec2_subnet_cidr_reservation.lb : split("/", r.cidr_block)[0]]
}

output "reserved_lb_cidrs" {
  value = tolist(aws_ec2_subnet_cidr_reservation.lb[*].cidr_block)
}
