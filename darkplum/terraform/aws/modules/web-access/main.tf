# The install's private HTTPS address: an internal NLB in the private subnets,
# TLS terminated with an ACM certificate, a DNS name in a public zone that
# resolves to the NLB's private addresses.
#
# Everything here is owned by Terraform (no in-cluster load balancer
# controller, no security group rules made from inside the cluster). The NLB
# forwards to NodePort 30080 on the CPU nodes with a PROXY protocol v2 header,
# so nginx sees each browser's address; `darkplum configure` tells nginx which
# peers (this NLB's pinned addresses) may set it.

locals {
  create_zone = var.zone_id == ""
  create_cert = var.certificate_arn == ""
  zone_id     = local.create_zone ? aws_route53_zone.web[0].zone_id : var.zone_id
  cert_arn    = local.create_cert ? aws_acm_certificate_validation.web[0].certificate_arn : var.certificate_arn

  validation = local.create_cert ? {
    for o in aws_acm_certificate.web[0].domain_validation_options : o.domain_name => o
  } : {}

  # NLB and target group names are limited to 32 characters.
  short = substr(var.name, 0, 24)
}

# --- DNS -------------------------------------------------------------------

# Created only when no zone was supplied and darkplum init found the domain's
# public zone in this account: delegated from there in the same apply.
data "aws_route53_zone" "parent" {
  count        = local.create_zone ? 1 : 0
  name         = var.domain
  private_zone = false
}

resource "aws_route53_zone" "web" {
  count   = local.create_zone ? 1 : 0
  name    = var.hostname
  comment = "darkplum ${var.name}: private web address (managed by Terraform)"
  # Never delete a zone that still holds records someone added by hand.
  force_destroy = false
  tags          = var.tags
}

resource "aws_route53_record" "delegation" {
  count           = local.create_zone ? 1 : 0
  zone_id         = data.aws_route53_zone.parent[0].zone_id
  name            = var.hostname
  type            = "NS"
  ttl             = 300
  records         = aws_route53_zone.web[0].name_servers
  allow_overwrite = false
}

# --- Certificate -----------------------------------------------------------

resource "aws_acm_certificate" "web" {
  count             = local.create_cert ? 1 : 0
  domain_name       = var.hostname
  validation_method = "DNS"
  tags              = var.tags

  lifecycle {
    create_before_destroy = true
  }
}

# Keyed by the hostname (known at plan time), not by the validation options
# (known only once the certificate exists).
resource "aws_route53_record" "cert_validation" {
  for_each        = local.create_cert ? toset([var.hostname]) : toset([])
  zone_id         = local.zone_id
  name            = local.validation[each.key].resource_record_name
  type            = local.validation[each.key].resource_record_type
  ttl             = 60
  records         = [local.validation[each.key].resource_record_value]
  allow_overwrite = false

  depends_on = [aws_route53_record.delegation]
}

resource "aws_acm_certificate_validation" "web" {
  count                   = local.create_cert ? 1 : 0
  certificate_arn         = aws_acm_certificate.web[0].arn
  validation_record_fqdns = [for r in aws_route53_record.cert_validation : r.fqdn]

  # The zone is delegated before this runs (same apply, or by darkplum init),
  # so validation takes minutes. Bounded, so a broken delegation fails the
  # apply with a message instead of hanging it.
  timeouts {
    create = "30m"
  }
}

# --- Load balancer ---------------------------------------------------------

resource "aws_security_group" "nlb" {
  name_prefix = "${local.short}-web-nlb-"
  description = "darkplum ${var.name} web NLB: 443 from the allowed networks"
  vpc_id      = var.vpc_id
  tags        = merge(var.tags, { Name = "${var.name}-web-nlb" })

  lifecycle {
    create_before_destroy = true
  }
}

resource "aws_vpc_security_group_ingress_rule" "https" {
  for_each          = toset(var.allowed_cidrs)
  security_group_id = aws_security_group.nlb.id
  description       = "HTTPS from an allowed network"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
  cidr_ipv4         = each.value
}

resource "aws_vpc_security_group_egress_rule" "to_nodes" {
  security_group_id            = aws_security_group.nlb.id
  description                  = "To the web NodePort and its health check"
  ip_protocol                  = "tcp"
  from_port                    = var.node_port
  to_port                      = var.node_port
  referenced_security_group_id = var.node_security_group_id
}

# The nodes accept the NodePort only from the NLB. This is the cluster primary
# security group, which the GPU nodes carry too (their launch template sets
# none): acceptable, because the NLB's targets are the CPU node group only,
# no web pod runs on a GPU node, and with externalTrafficPolicy Local a node
# without a web pod forwards nothing on 30080.
resource "aws_vpc_security_group_ingress_rule" "node_port" {
  security_group_id            = var.node_security_group_id
  description                  = "darkplum web NodePort from the web NLB"
  ip_protocol                  = "tcp"
  from_port                    = var.node_port
  to_port                      = var.node_port
  referenced_security_group_id = aws_security_group.nlb.id
}

resource "aws_lb" "web" {
  name               = "${local.short}-web"
  internal           = true
  load_balancer_type = "network"
  security_groups    = [aws_security_group.nlb.id]

  dynamic "subnet_mapping" {
    for_each = [for i, id in var.private_subnet_ids : { subnet_id = id, ip = var.lb_private_ips[i] }]
    content {
      subnet_id            = subnet_mapping.value.subnet_id
      private_ipv4_address = subnet_mapping.value.ip
    }
  }

  tags = var.tags

  lifecycle {
    precondition {
      condition     = length(var.private_subnet_ids) == length(var.lb_private_ips) && length(var.private_subnet_ids) >= 2
      error_message = "The web NLB needs at least two private subnets, with one reserved address per subnet ID, in the same order."
    }
  }
}

resource "aws_lb_target_group" "web" {
  name        = "${local.short}-web"
  port        = var.node_port
  protocol    = "TCP"
  target_type = "instance"
  vpc_id      = var.vpc_id

  # The browser's address travels in the PROXY header; nginx accepts it only
  # from this NLB's pinned addresses.
  proxy_protocol_v2  = true
  preserve_client_ip = false

  health_check {
    protocol = "TCP"
    port     = "traffic-port"
  }

  tags = var.tags
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.web.arn
  port              = 443
  protocol          = "TLS"
  certificate_arn   = local.cert_arn
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-2-2021-06"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }

  tags = var.tags
}

resource "aws_autoscaling_attachment" "web" {
  autoscaling_group_name = var.node_autoscaling_group_name
  lb_target_group_arn    = aws_lb_target_group.web.arn
}

resource "aws_route53_record" "web" {
  zone_id         = local.zone_id
  name            = var.hostname
  type            = "A"
  allow_overwrite = false

  alias {
    name    = aws_lb.web.dns_name
    zone_id = aws_lb.web.zone_id
    # The NLB is the only address; answering with it while targets recover
    # beats answering with nothing.
    evaluate_target_health = false
  }

  depends_on = [aws_route53_record.delegation]
}
