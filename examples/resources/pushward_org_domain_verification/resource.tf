resource "pushward_org_domain" "example" {
  domain = "example.com"
}

# The TXT record, here in Route 53; a cloudflare_dns_record or any other DNS
# provider's record works the same way.
resource "aws_route53_record" "pushward" {
  zone_id = var.zone_id
  name    = pushward_org_domain.example.txt_name
  type    = "TXT"
  ttl     = 300
  records = [pushward_org_domain.example.txt_value]
}

# Asks PushWard to look the record up until it shows, for up to 15 minutes.
# Should the domain lapse back to pending later, fix the record and run
# terraform apply -replace=pushward_org_domain_verification.example.
resource "pushward_org_domain_verification" "example" {
  domain_id = pushward_org_domain.example.id

  timeouts {
    create = "15m"
  }

  depends_on = [aws_route53_record.pushward]
}
