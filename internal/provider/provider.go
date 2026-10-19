package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/withlogicco/terraform-provider-ceryx/internal/ceryx"
	"github.com/withlogicco/terraform-provider-ceryx/internal/dockercurl"
)

const (
	endpointEnv      = "CERYX_ENDPOINT"
	defaultDockerAPI = "http://127.0.0.1:8080"
	connectorSidecar = "sidecar"
	connectorExec    = "exec"
)

type CeryxProvider struct{ version string }

type ProviderModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	Docker   *DockerModel `tfsdk:"docker"`
}

type DockerModel struct {
	Host      types.String    `tfsdk:"host"`
	APIURL    types.String    `tfsdk:"api_url"`
	Container *ContainerModel `tfsdk:"container"`
	Connector *ConnectorModel `tfsdk:"connector"`
}

type ConnectorModel struct {
	Type  types.String `tfsdk:"type"`
	Image types.String `tfsdk:"image"`
}

type ContainerModel struct {
	ID     types.String `tfsdk:"id"`
	Name   types.String `tfsdk:"name"`
	Labels types.Map    `tfsdk:"labels"`
}

func New(version string) provider.Provider {
	return &CeryxProvider{version: version}
}

func (p *CeryxProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "ceryx"
	resp.Version = p.version
}

func (p *CeryxProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The Ceryx provider manages routes in a [Ceryx](https://github.com/withlogicco/ceryx) reverse proxy through its API. " +
			"It reaches the API either at a URL (`endpoint`) or, when the API is only reachable inside its container, by running curl " +
			"next to a container picked by a selector (`docker`).",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Ceryx API URL reachable from where Terraform runs, for example `http://ceryx:8080`. " +
					"Defaults to `" + endpointEnv + "`. Conflicts with `docker`.",
			},
		},
		Blocks: map[string]schema.Block{
			"docker": schema.SingleNestedBlock{
				MarkdownDescription: "Reach the API inside a Ceryx container on a Docker host. Each API request is one curl call, run in the " +
					"network namespace of the container `container` selects, so the API needs no published port. `connector` chooses where curl runs. " +
					"Conflicts with `endpoint`.",
				Attributes: map[string]schema.Attribute{
					"host": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Docker daemon address: `unix:///var/run/docker.sock`, `tcp://host:2376` or `ssh://user@host`. " +
							"Defaults to `DOCKER_HOST`, then the local daemon. `tcp` hosts use `DOCKER_TLS_VERIFY` and `DOCKER_CERT_PATH`. " +
							"`ssh` hosts run `docker system dial-stdio` through the local `ssh` command, so they use your SSH config and agent.",
					},
					"api_url": schema.StringAttribute{
						Optional: true,
						MarkdownDescription: "Ceryx API URL as seen from inside the container. Defaults to `" + defaultDockerAPI + "`, " +
							"where Ceryx serves its API. Older deployments with a separate `ceryx-api` container listen on `http://127.0.0.1:5555`.",
					},
				},
				Blocks: map[string]schema.Block{
					"container": schema.SingleNestedBlock{
						MarkdownDescription: "Selects the Ceryx container among the running containers on the Docker host. " +
							"Set at least one criterion; every criterion set must match, and exactly one container may match. " +
							"The provider resolves the selector on every request, so it follows the container across restarts and redeploys.",
						Attributes: map[string]schema.Attribute{
							"id": schema.StringAttribute{
								Optional:            true,
								MarkdownDescription: "Container ID, or a unique prefix of one.",
							},
							"name": schema.StringAttribute{
								Optional:            true,
								MarkdownDescription: "Exact container name.",
							},
							"labels": schema.MapAttribute{
								Optional:    true,
								ElementType: types.StringType,
								MarkdownDescription: "Labels the container must carry, like `docker ps --filter label=...`. An empty value matches " +
									"any value of that label. Select a Swarm service's tasks with `{ \"com.docker.swarm.service.name\" = \"ingress_ceryx\" }`, " +
									"or a Compose service with `{ \"com.docker.compose.service\" = \"ceryx\" }`.",
							},
						},
					},
					"connector": schema.SingleNestedBlock{
						MarkdownDescription: "Where curl runs. Defaults to `sidecar`.",
						Attributes: map[string]schema.Attribute{
							"type": schema.StringAttribute{
								Optional: true,
								MarkdownDescription: "`sidecar` runs curl in a throwaway container that joins the selected container's network namespace, " +
									"like `docker run --rm --network container:<id> --entrypoint curl <image> ...`; it works whatever the Ceryx image contains. " +
									"`exec` runs `docker exec <id> curl ...` in the selected container, which must include curl. Defaults to `sidecar`.",
								Validators: []validator.String{stringvalidator.OneOf(connectorSidecar, connectorExec)},
							},
							"image": schema.StringAttribute{
								Optional: true,
								MarkdownDescription: "Image the `sidecar` connector runs, with `curl` on its `PATH`. Defaults to `" + dockercurl.DefaultSidecarImage + "`. " +
									"The Docker host pulls it on first use if it does not have it. Only valid with `type = \"sidecar\"`.",
							},
						},
					},
				},
			},
		},
	}
}

func (p *CeryxProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var data ProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if data.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Unknown endpoint", "The endpoint must be known when Terraform plans.")
		return
	}
	endpoint := data.Endpoint.ValueString()

	if data.Docker != nil && endpoint != "" {
		resp.Diagnostics.AddError("Conflicting configuration", "Set either endpoint or a docker block, not both.")
		return
	}

	var client *ceryx.Client
	var err error
	switch {
	case data.Docker != nil:
		client = p.dockerClient(ctx, data.Docker, resp)
		if client == nil {
			return
		}
	default:
		if endpoint == "" {
			endpoint = os.Getenv(endpointEnv)
		}
		if endpoint == "" {
			resp.Diagnostics.AddError("Missing Ceryx API configuration", "Set endpoint, "+endpointEnv+", or a docker block.")
			return
		}
		client, err = ceryx.NewClient(endpoint, nil)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("endpoint"), "Invalid endpoint", err.Error())
			return
		}
	}

	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *CeryxProvider) dockerClient(ctx context.Context, m *DockerModel, resp *provider.ConfigureResponse) *ceryx.Client {
	dockerPath := path.Root("docker")
	containerPath := dockerPath.AtName("container")
	connectorPath := dockerPath.AtName("connector")

	known := map[string]types.String{"host": m.Host, "api_url": m.APIURL}
	if m.Connector != nil {
		known["type"], known["image"] = m.Connector.Type, m.Connector.Image
	}
	if m.Container != nil {
		known["id"], known["name"] = m.Container.ID, m.Container.Name
	}
	for name, v := range known {
		if !v.IsUnknown() {
			continue
		}
		at := dockerPath.AtName(name)
		switch name {
		case "type", "image":
			at = connectorPath.AtName(name)
		case "id", "name":
			at = containerPath.AtName(name)
		}
		resp.Diagnostics.AddAttributeError(at, "Unknown value", "The docker block must be known when Terraform plans.")
	}

	if m.Container == nil {
		resp.Diagnostics.AddAttributeError(containerPath, "Missing container selector", "Add a container block that selects the Ceryx container.")
		return nil
	}
	c := m.Container
	if c.Labels.IsUnknown() {
		resp.Diagnostics.AddAttributeError(containerPath.AtName("labels"), "Unknown value", "The docker block must be known when Terraform plans.")
	}
	if resp.Diagnostics.HasError() {
		return nil
	}

	selector := dockercurl.Selector{
		ID:   c.ID.ValueString(),
		Name: c.Name.ValueString(),
	}
	if !c.Labels.IsNull() {
		resp.Diagnostics.Append(c.Labels.ElementsAs(ctx, &selector.Labels, false)...)
		if resp.Diagnostics.HasError() {
			return nil
		}
	}
	if selector.IsZero() {
		resp.Diagnostics.AddAttributeError(containerPath, "Empty container selector", "Set at least one of id, name or labels.")
		return nil
	}

	connectorType, image := connectorSidecar, ""
	if m.Connector != nil {
		if t := m.Connector.Type.ValueString(); t != "" {
			connectorType = t
		}
		image = m.Connector.Image.ValueString()
	}
	if connectorType != connectorSidecar && image != "" {
		resp.Diagnostics.AddAttributeError(connectorPath.AtName("image"), "Image needs the sidecar connector", "image applies only to type = \"sidecar\".")
		return nil
	}

	docker, err := dockercurl.NewDockerClient(m.Host.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(dockerPath.AtName("host"), "Invalid Docker host", err.Error())
		return nil
	}

	var connector dockercurl.Connector = &dockercurl.Exec{API: docker}
	if connectorType == connectorSidecar {
		if image == "" {
			image = dockercurl.DefaultSidecarImage
		}
		connector = &dockercurl.Sidecar{API: docker, Image: image}
	}

	apiURL := m.APIURL.ValueString()
	if apiURL == "" {
		apiURL = defaultDockerAPI
	}
	client, err := ceryx.NewClient(apiURL, &dockercurl.Transport{API: docker, Selector: selector, Connector: connector})
	if err != nil {
		resp.Diagnostics.AddAttributeError(dockerPath.AtName("api_url"), "Invalid API URL", err.Error())
		return nil
	}
	return client
}

func (p *CeryxProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewRouteResource}
}

func (p *CeryxProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{NewRouteDataSource, NewRoutesDataSource}
}
