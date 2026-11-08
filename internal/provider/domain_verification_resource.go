package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/mac-lucky/terraform-provider-pushward/internal/client"
)

// verifyEvery is the wait between verify attempts while the TXT record is
// not visible yet.
var verifyEvery = 10 * time.Second

const verifyTimeout = 10 * time.Minute

type domainVerificationResource struct{ c *client.Client }

type domainVerificationModel struct {
	ID       types.String   `tfsdk:"id"`
	DomainID types.String   `tfsdk:"domain_id"`
	OrgID    types.String   `tfsdk:"org_id"`
	Timeouts timeouts.Value `tfsdk:"timeouts"`
}

func newDomainVerificationResource() resource.Resource { return &domainVerificationResource{} }

func (r *domainVerificationResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_org_domain_verification"
}

func (r *domainVerificationResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Waits until a `pushward_org_domain` is verified. Make it depend on the DNS record that publishes " +
			"the domain's TXT record; creating it asks PushWard to look the record up, and asks again while the record is not " +
			"visible yet or the lookup fails, until `timeouts.create` (10 minutes by default). A domain another organization " +
			"verified first fails at once.\n\n" +
			"When PushWard sets the domain back to `pending` (seven missed daily checks in a row), this resource stays in the " +
			"state, so whatever depends on it is not held up, and the domain's `status` shows `pending`. Fix the TXT record, then " +
			"`terraform apply -replace=<address>` verifies the domain again. A removed domain drops this resource from the state. " +
			"Destroying it changes nothing in PushWard.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				MarkdownDescription: "The domain id.",
				Computed:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"domain_id": schema.StringAttribute{
				MarkdownDescription: "The `pushward_org_domain` to verify. Changing it verifies the other domain.",
				Required:            true,
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:          []validator.String{lowerID},
			},
			"org_id": orgIDAttribute(),
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true}),
		},
	}
}

func (r *domainVerificationResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.c = providerClient(req.ProviderData, &resp.Diagnostics)
}

// verifyAgain reports whether a failed verify may pass later: the record is
// not visible yet, or PushWard or the connection to it failed. Verify is
// idempotent, so asking again is safe.
func verifyAgain(err error) bool {
	var e *client.Error
	if errors.As(err, &e) {
		return e.Code == client.CodeDomainNotVerified || e.Status >= http.StatusInternalServerError
	}
	// A certificate the client refuses is refused again on every try.
	var netErr *url.Error
	var certErr *tls.CertificateVerificationError
	return errors.As(err, &netErr) && !errors.As(err, &certErr)
}

func (r *domainVerificationResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan domainVerificationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	limit, diags := plan.Timeouts.Create(ctx, verifyTimeout)
	if resp.Diagnostics.Append(diags...); resp.Diagnostics.HasError() {
		return
	}
	plan.OrgID = types.StringValue(r.c.OrgID)
	id := plan.DomainID.ValueString()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	var last error // PushWard's last answer, for the timeout message
	for {
		err := r.c.Do(ctx, http.MethodPost, r.c.OrgPath("domains", id, "verify"), nil, nil)
		if err == nil {
			break
		}
		if client.HasCode(err, client.CodeDomainTaken) {
			resp.Diagnostics.AddError("Another organization verified this domain",
				"A domain is verified by one organization at most, the first to verify it.\n\n"+err.Error())
			return
		}
		// A request the timeout or a cancel cut short ends below.
		if !verifyAgain(err) && ctx.Err() == nil {
			resp.Diagnostics.AddError("Verifying the domain failed", err.Error())
			return
		}
		if (verifyAgain(err) && ctx.Err() == nil) || last == nil {
			last = err
		}
		t := time.NewTimer(verifyEvery)
		select {
		case <-ctx.Done():
			t.Stop()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				resp.Diagnostics.AddError("Verifying the domain was cancelled",
					"Terraform stopped before PushWard verified the domain. The next apply asks again.")
				return
			}
			resp.Diagnostics.AddError("The domain was not verified in time",
				"PushWard did not find the domain's TXT record within "+limit.String()+". Publish a TXT record named txt_name "+
					"with the value txt_value (both are attributes of pushward_org_domain), make this resource depend on it, and "+
					"apply again. DNS changes can take a while to appear; timeouts.create sets how long to wait.\n\n"+last.Error())
			return
		case <-t.C:
		}
	}
	plan.ID = plan.DomainID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Read keeps the verification while the domain exists, also once it lapsed
// back to pending: pushward_org_domain's status shows that, and dropping the
// verification would hold every resource that depends on it behind a new
// verify. A domain gone drops it.
func (r *domainVerificationResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state domainVerificationModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	d, err := findDomain(ctx, r.c, domainWithID(state.DomainID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Reading the domain failed", err.Error())
		return
	}
	if d == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update only changes the timeouts.
func (r *domainVerificationResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state domainVerificationModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !sameOrg(r.c, &state.OrgID, &resp.Diagnostics) {
		return
	}
	plan.ID, plan.OrgID = state.ID, state.OrgID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete only forgets the verification: the domain stays verified.
func (r *domainVerificationResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}
