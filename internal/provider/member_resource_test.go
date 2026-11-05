package provider

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

const memberName = "pushward_org_member.ann"

func memberConfig(f *fakeAPI, role string) string {
	return f.config(fmt.Sprintf(`
resource "pushward_org_member" "ann" {
  email = "ann@example.com"
  role  = %q
}

resource "pushward_org_group" "team" {
  name       = "team"
  member_ids = compact([pushward_org_member.ann.user_id])
}
`, role))
}

// onlyOwnerLeft checks that destroy removed every member and closed every
// invite Terraform made.
func (f *fakeAPI) onlyOwnerLeft(*terraform.State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.members) != 1 {
		return fmt.Errorf("%d members left after destroy", len(f.members))
	}
	for id, inv := range f.invites {
		if inv.RevokedAt == nil && inv.RedeemedBy == nil && time.Now().Before(inv.ExpiresAt) {
			return fmt.Errorf("invite %s still open after destroy", id)
		}
	}
	return nil
}

func TestAccMemberLifecycle(t *testing.T) {
	f := newFake(t)
	var invite, firstInvite, userID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.onlyOwnerLeft,
		Steps: []resource.TestStep{
			{
				Config: memberConfig(f, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					resource.TestCheckNoResourceAttr(memberName, "user_id"),
					resource.TestCheckResourceAttrPair(memberName, "id", memberName, "invite_id"),
					resource.TestCheckResourceAttr("pushward_org_group.team", "member_ids.#", "0"),
					saveAttr(memberName, "invite_id", &firstInvite),
				),
			},
			{
				// A role change while pending revokes the invite and mails a new one.
				Config: memberConfig(f, "viewer"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					resource.TestCheckResourceAttr(memberName, "role", "viewer"),
					attrNot(memberName, "invite_id", &firstInvite),
					saveAttr(memberName, "invite_id", &invite),
					func(*terraform.State) error {
						if !f.inviteRevoked(firstInvite) {
							return fmt.Errorf("the first invite was not revoked")
						}
						return nil
					},
				),
			},
			{
				// They join: the next plan sees it and the group picks them up.
				PreConfig: func() { userID = f.join(invite) },
				Config:    memberConfig(f, "viewer"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "joined"),
					attrIs(memberName, "user_id", &userID),
					attrIs(memberName, "id", &userID),
					attrIs(memberName, "invite_id", &invite),
					resource.TestCheckTypeSetElemAttrPair("pushward_org_group.team", "member_ids.*", memberName, "user_id"),
				),
			},
			{
				// Once joined a role change is a PATCH, not a new invite.
				Config: memberConfig(f, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "role", "member"),
					attrIs(memberName, "user_id", &userID),
					func(*terraform.State) error {
						if got := f.memberRole(userID); got != "member" {
							return fmt.Errorf("role on the server = %q", got)
						}
						if n := f.count("POST", "/invites"); n != 2 {
							return fmt.Errorf("%d invites sent, want 2", n)
						}
						return nil
					},
				),
			},
			{
				ResourceName:            memberName,
				ImportState:             true,
				ImportStateIdFunc:       func(*terraform.State) (string, error) { return userID, nil },
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"invite_id"},
			},
			{
				ResourceName:      memberName,
				ImportState:       true,
				ImportStateIdFunc: func(*terraform.State) (string, error) { return userID, nil },
				ImportStateKind:   resource.ImportBlockWithID,
			},
			{
				// They left: dropped from state and invited again.
				PreConfig: func() { f.removeMember(userID) },
				Config:    memberConfig(f, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					resource.TestCheckNoResourceAttr(memberName, "user_id"),
					attrNot(memberName, "invite_id", &invite),
					resource.TestCheckResourceAttr("pushward_org_group.team", "member_ids.#", "0"),
				),
			},
		},
	})
}

func TestAccMemberExpiredInvite(t *testing.T) {
	f := newFake(t)
	var invite string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.onlyOwnerLeft,
		Steps: []resource.TestStep{
			{
				Config: memberConfig(f, "member"),
				Check:  saveAttr(memberName, "invite_id", &invite),
			},
			{
				PreConfig: func() { f.expire(invite) },
				Config:    memberConfig(f, "member"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					attrNot(memberName, "invite_id", &invite),
					saveAttr(memberName, "invite_id", &invite),
				),
			},
			{
				// Joined now, so destroy removes the member (onlyOwnerLeft).
				PreConfig: func() { f.join(invite) },
				Config:    memberConfig(f, "member"),
				Check:     resource.TestCheckResourceAttr(memberName, "status", "joined"),
			},
		},
	})
}

func TestAccMemberEmailTaken(t *testing.T) {
	f := newFake(t)
	f.addMember("member", "ann@example.com")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:      memberConfig(f, "member"),
			ExpectError: regexp.MustCompile(`(?s)already has this email.*terraform import`),
		}},
	})
}

func TestAccMemberValidation(t *testing.T) {
	f := newFake(t)
	for _, tc := range []struct{ email, role, want string }{
		{"ann@example.com", "admin", `value must be one of`},
		{"Ann@example.com", "member", `must be lowercase`},
	} {
		resource.Test(t, resource.TestCase{
			ProtoV6ProviderFactories: factories,
			Steps: []resource.TestStep{{
				Config:      f.config(fmt.Sprintf("resource \"pushward_org_member\" \"x\" {\n  email = %q\n  role = %q\n}\n", tc.email, tc.role)),
				ExpectError: regexp.MustCompile(tc.want),
			}},
		})
	}
}
