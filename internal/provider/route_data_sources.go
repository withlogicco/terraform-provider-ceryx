package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
)

func routeDataSourceAttributes(sourceRequired bool) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"source": schema.StringAttribute{
			Required:            sourceRequired,
			Computed:            !sourceRequired,
			MarkdownDescription: "Host name the route matches.",
		},
		"target":           schema.StringAttribute{Computed: true, MarkdownDescription: "Where requests go."},
		"enforce_https":    schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether plain HTTP requests redirect to HTTPS."},
		"mode":             schema.StringAttribute{Computed: true, MarkdownDescription: "`proxy` or `redirect`."},
		"certificate_path": schema.StringAttribute{Computed: true, MarkdownDescription: "TLS certificate path inside the Ceryx container, if set."},
		"key_path":         schema.StringAttribute{Computed: true, MarkdownDescription: "TLS private key path inside the Ceryx container, if set."},
	}
}

func clientFromProviderData(data any, diags interface{ AddError(string, string) }) *ceryx.Client {
	if data == nil {
		return nil
	}
	client, ok := data.(*ceryx.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("Expected *ceryx.Client, got %T.", data))
	}
	return client
}

// ceryx_route

type RouteDataSource struct{ client *ceryx.Client }

func NewRouteDataSource() datasource.DataSource { return &RouteDataSource{} }

func (d *RouteDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_route"
}

func (d *RouteDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads one Ceryx route. Fails if Ceryx has no route for `source`.",
		Attributes:          routeDataSourceAttributes(true),
	}
}

func (d *RouteDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *RouteDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m RouteModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	route, found, err := d.client.GetRoute(ctx, m.Source.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Ceryx request failed", err.Error())
		return
	}
	if !found {
		resp.Diagnostics.AddAttributeError(path.Root("source"), "Route not found", fmt.Sprintf("Ceryx has no route for %s.", m.Source.ValueString()))
		return
	}
	setRoute(&m, route)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// ceryx_routes

type RoutesDataSource struct{ client *ceryx.Client }

type RoutesModel struct {
	Routes  types.List `tfsdk:"routes"`
	Sources types.Set  `tfsdk:"sources"`
}

var routeObjectType = types.ObjectType{AttrTypes: map[string]attr.Type{
	"source":           types.StringType,
	"target":           types.StringType,
	"enforce_https":    types.BoolType,
	"mode":             types.StringType,
	"certificate_path": types.StringType,
	"key_path":         types.StringType,
}}

func NewRoutesDataSource() datasource.DataSource { return &RoutesDataSource{} }

func (d *RoutesDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_routes"
}

func (d *RoutesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Lists every route in Ceryx, including routes Terraform does not manage.",
		Attributes: map[string]schema.Attribute{
			"routes": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Routes in the order Ceryx returns them.",
				NestedObject:        schema.NestedAttributeObject{Attributes: routeDataSourceAttributes(false)},
			},
			"sources": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Source host names of all routes.",
			},
		},
	}
}

func (d *RoutesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFromProviderData(req.ProviderData, &resp.Diagnostics)
}

func (d *RoutesDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	routes, err := d.client.ListRoutes(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Ceryx request failed", err.Error())
		return
	}

	items := make([]RouteModel, 0, len(routes))
	sources := make([]string, 0, len(routes))
	for _, route := range routes {
		var m RouteModel
		setRoute(&m, route)
		items = append(items, m)
		sources = append(sources, route.Source)
	}

	var out RoutesModel
	var diags diag.Diagnostics
	out.Routes, diags = types.ListValueFrom(ctx, routeObjectType, items)
	resp.Diagnostics.Append(diags...)
	out.Sources, diags = types.SetValueFrom(ctx, types.StringType, sources)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}
