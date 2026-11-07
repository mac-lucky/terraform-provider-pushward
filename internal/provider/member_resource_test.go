package provider

import (
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

const memberName = "pushward_org_member.ann"

func memberConfig(f *fakeAPI, role string) string {
	return memberEmailConfig(f, "ann@example.com", role)
}

func memberEmailConfig(f *fakeAPI, email, role string) string {
	return f.config(fmt.Sprintf(`
resource "pushward_org_member" "ann" {
  email = %q
  role  = %q
}

resource "pushward_org_group" "team" {
  name       = "team"
  member_ids = compact([pushward_org_member.ann.user_id])
}
`, email, role))
}

func (f *fakeAPI) memberEmail(uid string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m := f.member(uid); m != nil && m.Email != nil {
		return *m.Email
	}
	return ""
}

func (f *fakeAPI) inviteEmail(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if inv := f.invites[id]; inv != nil && inv.Email != nil {
		return *inv.Email
	}
	return ""
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
		if usable(inv) {
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

func TestAccMemberEmailChange(t *testing.T) {
	f := newFake(t)
	var firstInvite, invite, userID string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.onlyOwnerLeft,
		Steps: []resource.TestStep{
			{
				Config: memberEmailConfig(f, "ann@example.com", "member"),
				Check:  saveAttr(memberName, "invite_id", &firstInvite),
			},
			{
				// While pending, a new email revokes the invite and mails one to the new address.
				Config: memberEmailConfig(f, "ann@corp.example.com", "member"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberName, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					resource.TestCheckResourceAttr(memberName, "email", "ann@corp.example.com"),
					attrNot(memberName, "invite_id", &firstInvite),
					saveAttr(memberName, "invite_id", &invite),
					func(*terraform.State) error {
						if !f.inviteRevoked(firstInvite) {
							return fmt.Errorf("the first invite was not revoked")
						}
						if got := f.inviteEmail(invite); got != "ann@corp.example.com" {
							return fmt.Errorf("new invite mailed to %q", got)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() { userID = f.join(invite) },
				Config:    memberEmailConfig(f, "ann@corp.example.com", "member"),
				Check:     resource.TestCheckResourceAttr(memberName, "status", "joined"),
			},
			{
				// Once joined, a new email only changes the member's label.
				Config: memberEmailConfig(f, "ann.b@example.com", "member"),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberName, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "email", "ann.b@example.com"),
					attrIs(memberName, "user_id", &userID),
					func(*terraform.State) error {
						if got := f.memberEmail(userID); got != "ann.b@example.com" {
							return fmt.Errorf("label on the server = %q", got)
						}
						if n := f.count("POST", "/invites"); n != 2 {
							return fmt.Errorf("%d invites sent, want 2", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// A member who joined with a code has no label: importing them and setting
// email updates them in place instead of removing them.
func TestAccMemberImportWithoutLabel(t *testing.T) {
	f := newFake(t)
	uid := f.addMember("viewer", "")
	cfg := memberEmailConfig(f, "ann@example.com", "viewer")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy:             f.onlyOwnerLeft,
		Steps: []resource.TestStep{
			{
				Config:             cfg,
				ResourceName:       memberName,
				ImportState:        true,
				ImportStateId:      uid,
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					if len(is) != 1 || is[0].Attributes["email"] != "" || is[0].Attributes["status"] != "joined" {
						return fmt.Errorf("imported %+v", is)
					}
					return nil
				},
			},
			{
				Config: cfg,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(memberName, plancheck.ResourceActionUpdate)},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "user_id", uid),
					resource.TestCheckResourceAttr(memberName, "email", "ann@example.com"),
					func(*terraform.State) error {
						if got := f.memberEmail(uid); got != "ann@example.com" {
							return fmt.Errorf("label on the server = %q", got)
						}
						if n := f.count("POST", "/invites"); n != 0 {
							return fmt.Errorf("%d invites sent, want 0", n)
						}
						return nil
					},
				),
			},
		},
	})
}

// An invite nobody can join with any more drops the member from the state,
// and the next apply invites them again.
func TestAccMemberUnusableInvite(t *testing.T) {
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
				// Used up without redeemed_by: whoever joined deleted their account.
				PreConfig: func() { f.useUp(invite) },
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

// An admin cannot be managed with an org admin key: importing one fails with
// the console hint instead of planning a role change that 403s on every apply.
func TestAccMemberUnmanageableRole(t *testing.T) {
	f := newFake(t)
	uid := f.addMember("admin", "ann@example.com")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:        memberConfig(f, "member"),
			ResourceName:  memberName,
			ImportState:   true,
			ImportStateId: uid,
			ExpectError:   regexp.MustCompile(`(?s)admin\s+role.*console`),
		}},
	})
	if got := f.memberRole(uid); got != "admin" {
		t.Errorf("role on the server = %q", got)
	}
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
		{" bob@example.com", "member", `whitespace`},
		{"ann@example.com\r", "member", `whitespace`},
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

func TestAccMemberImportByEmail(t *testing.T) {
	f := newFake(t)
	// Past the first page of two.
	f.addMember("viewer", "zed@example.com")
	f.addMember("member", "")
	uid := f.addMember("member", "ann@example.com")
	cfg := memberEmailConfig(f, "ann@example.com", "member")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: func(*terraform.State) error {
			if f.memberRole(uid) != "" {
				return fmt.Errorf("member %s left after destroy", uid)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config:             cfg,
				ResourceName:       memberName,
				ImportState:        true,
				ImportStateId:      " Ann@Example.com",
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					a := is[0].Attributes
					if a["id"] != uid || a["user_id"] != uid || a["status"] != "joined" || a["email"] != "ann@example.com" {
						return fmt.Errorf("imported %v", a)
					}
					return nil
				},
			},
			{
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "user_id", uid),
					func(*terraform.State) error {
						if n := f.count("POST", "/invites"); n != 0 {
							return fmt.Errorf("%d invites sent, want 0", n)
						}
						return nil
					},
				),
			},
		},
	})
}

func TestAccMemberImportPendingByEmail(t *testing.T) {
	f := newFake(t)
	// Invites list newest first, two a page. Only the open one is taken.
	now := time.Now()
	f.addInvite("ann@example.com", "member", func(inv *client.Invite) { inv.RevokedAt = &now })
	open := f.addInvite("ann@example.com", "viewer", nil)
	f.addInvite("ann@example.com", "member", func(inv *client.Invite) { inv.ExpiresAt = now.Add(-time.Hour) })
	f.addInvite("bob@example.com", "member", nil)
	f.addInvite("ann@example.com", "member", func(inv *client.Invite) { inv.Uses = inv.MaxUses })
	cfg := memberEmailConfig(f, "ann@example.com", "viewer")
	var uid string
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		CheckDestroy: func(*terraform.State) error {
			if f.memberRole(uid) != "" {
				return fmt.Errorf("member %s left after destroy", uid)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config:             cfg,
				ResourceName:       memberName,
				ImportState:        true,
				ImportStateId:      "ann@example.com",
				ImportStatePersist: true,
				ImportStateCheck: func(is []*terraform.InstanceState) error {
					a := is[0].Attributes
					if a["id"] != open || a["invite_id"] != open || a["status"] != "pending" || a["role"] != "viewer" {
						return fmt.Errorf("imported %v", a)
					}
					if v, ok := a["user_id"]; ok {
						return fmt.Errorf("user_id = %q, want null", v)
					}
					return nil
				},
			},
			{
				// The pending member is the one invited: nothing is mailed again.
				Config: cfg,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(memberName, "status", "pending"),
					func(*terraform.State) error {
						if n := f.count("POST", "/invites"); n != 0 {
							return fmt.Errorf("%d invites sent, want 0", n)
						}
						return nil
					},
				),
			},
			{
				PreConfig: func() { uid = f.join(open) },
				Config:    cfg,
				Check:     resource.TestCheckResourceAttr(memberName, "status", "joined"),
			},
		},
	})
}

func TestAccMemberImportByEmailNotFound(t *testing.T) {
	f := newFake(t)
	now := time.Now()
	f.addInvite("nobody@example.com", "member", func(inv *client.Invite) { inv.RevokedAt = &now })
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: factories,
		Steps: []resource.TestStep{{
			Config:        memberEmailConfig(f, "nobody@example.com", "member"),
			ResourceName:  memberName,
			ImportState:   true,
			ImportStateId: "nobody@example.com",
			ExpectError:   regexp.MustCompile(`(?s)no\s+member\s+with\s+the\s+email\s+nobody@example\.com\s+and\s+no\s+open\s+invite`),
		}},
	})
}
