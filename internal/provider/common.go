package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// routingName is the server's rule for group and tag names.
var routingName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const routingNameRule = "must be 1-40 lowercase letters, digits, - and _, starting with a letter or digit"

var noUpper = regexp.MustCompile(`^[^A-Z]*$`)

// lowerID refuses an uppercase id in a list: PushWard lowercases ids, so the
// list it returns would never match the configuration.
var lowerID = stringvalidator.RegexMatches(noUpper, "must be lowercase, as PushWard returns ids (use lower())")

func orgIDAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		MarkdownDescription: "The organization the resource belongs to, that of the provider's org admin key when the resource " +
			"was created or imported. An org admin key of another organization is an error, not a plan that creates the resource again there.",
		Computed:      true,
		PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
}

// sameOrg reports whether the provider's key belongs to the organization
// recorded in org, and records the key's organization when org is empty
// (state from before org_id). Another organization answers a resource's id
// with the resource's own 404, which must not read as the resource gone.
func sameOrg(c *client.Client, org *types.String, diags *diag.Diagnostics) bool {
	if v := org.ValueString(); v != "" && v != c.OrgID {
		diags.AddError("The org admin key belongs to another organization",
			"This resource belongs to organization "+v+", but the provider's org admin key belongs to organization "+c.OrgID+". "+
				"Configure a key of organization "+v+", or stop managing the resource here with terraform state rm.")
		return false
	}
	*org = types.StringValue(c.OrgID)
	return true
}

// providerClient takes the client the provider configured. It is nil while
// Terraform validates a configuration before the provider is configured.
func providerClient(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("got %T", data))
	}
	return c
}

func stringSet(ids []string) types.Set {
	vals := make([]attr.Value, 0, len(ids))
	for _, id := range ids {
		vals = append(vals, types.StringValue(id))
	}
	return types.SetValueMust(types.StringType, vals)
}

// setStrings returns the set's elements, sorted, and never nil so that an
// empty set is sent as [] and clears the list.
func setStrings(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	out := []string{}
	if s.IsNull() || s.IsUnknown() {
		return out
	}
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	slices.Sort(out)
	return out
}

func emptySet() types.Set { return types.SetValueMust(types.StringType, []attr.Value{}) }

// clearable is a description or note for a PATCH: the empty string clears it.
func clearable(s types.String) *string {
	v := s.ValueString()
	return &v
}

// createFailed explains a failed group or tag create. A 409 org.name_taken
// is a group or tag made outside Terraform, which can be imported instead.
func createFailed(diags *diag.Diagnostics, kind, name string, err error) {
	if client.HasCode(err, client.CodeNameTaken) {
		diags.AddError("A "+kind+" with this name already exists",
			"The organization already has a "+kind+" named "+name+". Import it with `terraform import <address> <id>` "+
				"(the id is in the console or the pushward_org_"+kind+"s data source), or pick another name.\n\n"+err.Error())
		return
	}
	diags.AddError("Creating the "+kind+" failed", err.Error())
}

// check is a string validator from a predicate; rule completes "must ...".
type check struct {
	rule string
	ok   func(string) bool
}

func (c check) Description(context.Context) string { return "must " + c.rule }

func (c check) MarkdownDescription(ctx context.Context) string { return c.Description(ctx) }

func (c check) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	v := req.ConfigValue
	if v.IsNull() || v.IsUnknown() || c.ok(v.ValueString()) {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid Attribute Value",
		fmt.Sprintf("Attribute %s %s, got: %q", req.Path, c.Description(ctx), v.ValueString()))
}

// trimmed refuses what the server would trim: the stored value would never
// match the configuration.
var trimmed = check{
	rule: "not start or end with whitespace, which PushWard trims (trimspace() or chomp() help)",
	ok:   func(s string) bool { return strings.TrimSpace(s) == s },
}

var rfc3339 = check{
	rule: "be an RFC 3339 time, such as 2027-01-31T00:00:00Z",
	ok:   func(s string) bool { _, err := time.Parse(time.RFC3339, s); return err == nil },
}
