package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
)

var (
	_ resource.ResourceWithConfigure   = &RouteResource{}
	_ resource.ResourceWithImportState = &RouteResource{}
)

type RouteResource struct{ client *ceryx.Client }

type RouteModel struct {
	Source          types.String `tfsdk:"source"`
	Target          types.String `tfsdk:"target"`
	EnforceHTTPS    types.Bool   `tfsdk:"enforce_https"`
	Mode            types.String `tfsdk:"mode"`
	CertificatePath types.String `tfsdk:"certificate_path"`
	KeyPath         types.String `tfsdk:"key_path"`
}

func NewRouteResource() resource.Resource { return &RouteResource{} }

func (r *RouteResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_route"
}

func (r *RouteResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	nonEmpty := []validator.String{stringvalidator.LengthAtLeast(1)}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Ceryx route: requests whose `Host` header (or TLS SNI) is `source` are proxied or redirected to `target`. " +
			"Creating a route that already exists in Ceryx fails, so an existing route is never overwritten; import it instead.",
		Attributes: map[string]schema.Attribute{
			"source": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Host name the route matches, for example `app.example.com`. `$wildcard` is the fallback route " +
					"for hosts with no route of their own. Changing it replaces the route.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    nonEmpty,
			},
			"target": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Where requests go, for example `http://app:8000`. Ceryx resolves host names with its own DNS resolver, usually Docker's.",
				Validators:          nonEmpty,
			},
			"enforce_https": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				Default:             booldefault.StaticBool(false),
				MarkdownDescription: "Redirect plain HTTP requests to HTTPS. Defaults to `false`.",
			},
			"mode": schema.StringAttribute{
				Optional:            true,
				Computed:            true,
				Default:             stringdefault.StaticString("proxy"),
				MarkdownDescription: "`proxy` forwards requests to `target`; `redirect` answers with a redirect to it. Defaults to `proxy`.",
				Validators:          []validator.String{stringvalidator.OneOf("proxy", "redirect")},
			},
			"certificate_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path, inside the Ceryx container, to a TLS certificate for `source`. Unset, Ceryx uses Let's Encrypt or its default certificate.",
				Validators:          nonEmpty,
			},
			"key_path": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Path, inside the Ceryx container, to the private key for `certificate_path`.",
				Validators:          nonEmpty,
			},
		},
	}
}

func (r *RouteResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*ceryx.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("Expected *ceryx.Client, got %T.", req.ProviderData))
		return
	}
	r.client = client
}

func (r *RouteResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m RouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	source := m.Source.ValueString()

	existing, found, err := r.client.GetRoute(ctx, source)
	if err != nil {
		resp.Diagnostics.AddError("Ceryx request failed", err.Error())
		return
	}
	if found {
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Route already exists",
			fmt.Sprintf("Ceryx already routes %s to %s. Import it with: terraform import <address> %s", source, existing.Target, source))
		return
	}

	if _, err := r.client.CreateRoute(ctx, ceryx.Route{Source: source, Target: m.Target.ValueString(), Settings: settingsFromModel(m)}); err != nil {
		resp.Diagnostics.AddError("Creating route failed", err.Error())
		return
	}
	r.readBack(ctx, &m, &resp.State, &resp.Diagnostics)
}

func (r *RouteResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m RouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	route, found, err := r.client.GetRoute(ctx, m.Source.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Ceryx request failed", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	setRoute(&m, route)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *RouteResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m RouteModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, err := r.client.UpdateRoute(ctx, m.Source.ValueString(), m.Target.ValueString(), settingsFromModel(m))
	if ceryx.IsNotFound(err) {
		resp.Diagnostics.AddError("Route not found", fmt.Sprintf("Ceryx has no route for %s anymore. Run terraform apply again to recreate it.", m.Source.ValueString()))
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Updating route failed", err.Error())
		return
	}
	r.readBack(ctx, &m, &resp.State, &resp.Diagnostics)
}

// readBack stores the route as Ceryx now reports it. Write responses differ
// between Ceryx versions; a GET does not.
func (r *RouteResource) readBack(ctx context.Context, m *RouteModel, state *tfsdk.State, diags *diag.Diagnostics) {
	route, found, err := r.client.GetRoute(ctx, m.Source.ValueString())
	if err != nil {
		diags.AddError("Reading route back failed", err.Error())
		return
	}
	if !found {
		diags.AddError("Route not stored", fmt.Sprintf("Ceryx accepted the route for %s but does not report it.", m.Source.ValueString()))
		return
	}
	setRoute(m, route)
	diags.Append(state.Set(ctx, m)...)
}

func (r *RouteResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m RouteModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteRoute(ctx, m.Source.ValueString()); err != nil {
		resp.Diagnostics.AddError("Deleting route failed", err.Error())
	}
}

func (r *RouteResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if req.ID == "" {
		resp.Diagnostics.AddError("Invalid import ID", "Use the route's source host name as the import ID.")
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("source"), req, resp)
}

func settingsFromModel(m RouteModel) ceryx.Settings {
	return ceryx.Settings{
		EnforceHTTPS:    m.EnforceHTTPS.ValueBool(),
		Mode:            m.Mode.ValueString(),
		CertificatePath: m.CertificatePath.ValueStringPointer(),
		KeyPath:         m.KeyPath.ValueStringPointer(),
	}
}

func setRoute(m *RouteModel, route ceryx.Route) {
	if route.Source != "" {
		m.Source = types.StringValue(route.Source)
	}
	m.Target = types.StringValue(route.Target)
	m.EnforceHTTPS = types.BoolValue(route.Settings.EnforceHTTPS)
	mode := route.Settings.Mode
	if mode == "" {
		mode = "proxy"
	}
	m.Mode = types.StringValue(mode)
	m.CertificatePath = optionalString(route.Settings.CertificatePath)
	m.KeyPath = optionalString(route.Settings.KeyPath)
}

func optionalString(s *string) types.String {
	if s == nil || *s == "" {
		return types.StringNull()
	}
	return types.StringValue(*s)
}
