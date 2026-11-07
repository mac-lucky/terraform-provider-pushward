# terraform-provider-pushward

Terraform provider for [PushWard](https://pushward.app) organizations. It keeps an
organization in code: its name, members (invited by email), groups, device tags and the
devices that carry them, routing rules and their order, and the organization's sending keys.
Data sources read the organization, its members and their devices, and its groups and tags,
so ones made in the console can be referenced by name.

## Keys

PushWard has two kinds of API key, and this provider takes the first:

- `hlo_...` is an org admin key. It manages one organization and sends nothing. A `manage`
  key acts as an admin, a `read` key as a viewer. An owner or admin creates it in the
  [PushWard console](https://console.pushward.app). A viewer cannot read sending keys or
  open invites, so a plan with a `pushward_org_key` or a pending `pushward_org_member`
  needs a `manage` key.
- `hlk_...` keys send activities, notifications and widgets. `pushward_org_key` creates
  them for the organization; your integrations use those.

The provider looks up the key's organization when it starts, so nothing takes an org id.
Each resource records it in `org_id`. A key of another organization is an error, not a
plan that creates everything again over there.

The open invites a `manage` key sent belong to that key. Revoking it, or lowering it to
`read`, revokes them too. The key also stops working when the person who created it leaves
the organization, is removed, loses admin rights or deletes their account, and its invites
go the same way. A `read` key cannot see invites and a revoked one is refused, so plans
with pending members fail until a working `manage` key is configured; the next apply with
one mails every pending member a new invite.

Make the Terraform key from an owner account. To rotate it, roll it in the console: that
keeps its id and its invites.

## Example

```hcl
terraform {
  required_providers {
    pushward = {
      source = "mac-lucky/pushward"
    }
  }
}

# api_key falls back to PUSHWARD_ORG_API_TOKEN.
provider "pushward" {}

resource "pushward_org_member" "ann" {
  email = "ann@example.com"
  role  = "member"
}

resource "pushward_org_group" "oncall" {
  name       = "oncall"
  member_ids = compact([pushward_org_member.ann.user_id])
}

resource "pushward_org_route" "deploys" {
  slug_pattern    = "deploy-*"
  applies_to      = "both"
  allow_group_ids = [pushward_org_group.oncall.id]
}

resource "pushward_org_key" "ci" {
  name   = "ci"
  target = { group_ids = [pushward_org_group.oncall.id] }
}
```

An invited member stays `pending` with no `user_id` until they join, so the group is empty
at first. The first apply after they join adds them; nothing else has to change.

More in [`docs/`](docs/) and [`examples/`](examples/).

## What stays in the console

An admin key cannot do everything an owner can, on purpose. The organization's name is
`pushward_org_settings`; these stay in the console:

- billing changes: plan, seats, payment method
- owners, admins and billing members (the provider invites and manages `member` and
  `viewer` only)
- org admin keys themselves
- deleting the organization

## Building and testing

Go (the version in `go.mod`) and Terraform 1.5 or newer.

```sh
go build ./...
make testacc    # TF_ACC=1 go test -race ./...
```

The acceptance tests run real `terraform` against an in-process fake of the API, so they
need no account and touch nothing. If Terraform cannot be downloaded where you run them,
point `TF_ACC_TERRAFORM_PATH` at a local binary.

To try a local build with your own configurations, install it and add a dev override to
`~/.terraformrc`:

```sh
go install .
```

```hcl
provider_installation {
  dev_overrides {
    "mac-lucky/pushward" = "/Users/you/go/bin"
  }
  direct {}
}
```

Terraform then uses the binary from that directory and skips `terraform init` for this
provider (it warns about it on every run). `make generate` rebuilds `docs/` from the schema
and `examples/`.

A release is a `v*` tag.

## License

MIT
