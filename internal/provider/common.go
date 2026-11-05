package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// routingName is the server's rule for group and tag names.
var routingName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const routingNameRule = "must be 1-40 lowercase letters, digits, - and _, starting with a letter or digit"

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
