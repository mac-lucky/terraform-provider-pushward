package provider

import (
	"context"
	"fmt"
	"regexp"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
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
