# Terraform Provider: Ceryx

The `ceryx` provider manages routes in a [Ceryx](https://github.com/withlogicco/ceryx) reverse proxy through the Ceryx API. Each `ceryx_route` maps a host name (`source`) to a target URL and its settings: HTTPS enforcement, proxy or redirect mode, and certificate paths.

Ceryx deployments usually keep the API unpublished, reachable only inside the Ceryx container. The provider therefore has two ways to reach the API:

- **`endpoint`**: a URL that Terraform can reach directly.
- **`docker`**: a Docker host plus a container selector (ID, name or Docker labels). Each API request is one `curl` call, run in the selected container's network namespace, so it reaches the API on the container's loopback interface.

## Requirements

- Terraform 1.9.8 or later
- Either network access to the Ceryx API, or access to the Docker daemon that runs Ceryx (local socket, `tcp://` with TLS, or `ssh://`)

## Installation

```hcl
terraform {
  required_version = ">= 1.9.8"

  required_providers {
    ceryx = {
      source  = "withlogicco/ceryx"
      version = "~> 0.1.0"
    }
  }
}
```

Run `terraform init` to install the provider.

## Usage

### Ceryx in a Docker Swarm service

```hcl
provider "ceryx" {
  docker {
    host = "ssh://root@proxy-01.example.com"

    container {
      labels = {
        "com.docker.swarm.service.name" = "ingress_ceryx"
      }
    }
  }
}

resource "ceryx_route" "app" {
  source        = "app.example.com"
  target        = "http://app:8000"
  enforce_https = true
}
```

Docker Swarm labels every task container with `com.docker.swarm.service.name`, so selecting by that label works like `docker ps --filter label=com.docker.swarm.service.name=ingress_ceryx`. The provider talks to one Docker daemon, so only the tasks running on that node are visible. Point `host` at the node that runs Ceryx.

### Selecting the container

The `container` block takes any combination of the following. Every criterion you set must match, and exactly one running container may match:

| Selector | Matches |
|---|---|
| `id` | A container ID or a unique prefix of one |
| `name` | The exact container name |
| `labels` | Containers that carry every label listed; an empty value matches any value |

The provider resolves the selector on every request, so it keeps working after Ceryx restarts or redeploys and the container ID changes. If several containers match, for example two replicas on one node, the request fails and the error names them. Narrow the selector with a label such as `com.docker.swarm.task.slot`.

### Connectors

The `connector` block chooses where `curl` runs. Each choice behaves like a `docker` command you could run by hand:

| `type` | Equivalent command | Needs |
|---|---|---|
| `sidecar` (default) | `docker run --rm --network container:<id> --entrypoint curl curlimages/curl:8.10.1 ...` | Nothing in the Ceryx image; the Docker host pulls the curl image on first use |
| `exec` | `docker exec <id> curl ...` | `curl` in the Ceryx image |

```hcl
docker {
  container {
    labels = { "com.docker.swarm.service.name" = "ingress_ceryx" }
  }

  connector {
    type  = "sidecar"
    image = "registry.example.com/mirror/curl:8.10.1" # optional, for hosts without Docker Hub access
  }
}
```

The sidecar shares the selected container's network namespace and joins no Docker network. It reaches exactly that container, never another replica behind the service's DNS name. Each request creates and removes one short-lived container, labelled `co.withlogic.terraform-provider-ceryx.sidecar=<ceryx container ID>`. `exec` starts nothing new, so it is faster, but most Ceryx images do not include `curl`.

`api_url` is the API address inside the container. It defaults to `http://127.0.0.1:8080`, where current Ceryx images serve the API. Older deployments with a separate `ceryx-api` container use `http://127.0.0.1:5555`; point the selector at that container.

Anyone who can run containers on a Docker host can already change anything on it. The provider needs no more access than that.

### Direct endpoint

```hcl
provider "ceryx" {
  endpoint = "http://ceryx:8080" # or set CERYX_ENDPOINT
}
```

### Existing routes

Creating a `ceryx_route` whose source already exists in Ceryx fails, so a route Terraform does not manage is never overwritten. Import it instead:

```sh
terraform import ceryx_route.app app.example.com
```

Removing an optional setting from the configuration resets it in Ceryx: `enforce_https` to `false`, `mode` to `proxy`, and certificate paths to unset.

Read routes with the `ceryx_route` and `ceryx_routes` data sources. `ceryx_routes` also lists routes that Terraform does not manage; see [its example](examples/data-sources/ceryx_routes/data-source.tf) for a check that flags them.

## Development and local testing

```sh
go test ./...
TF_ACC=1 go test ./...
go vet ./...
terraform fmt -check -recursive
```

The acceptance tests (`TF_ACC=1`) run Terraform against an in-memory fake of the Ceryx API. The connector tests run the real `curl` as a local process behind a fake Docker API, so neither set needs Docker.

The end-to-end test starts real Ceryx containers and manages a route through each connector. For the sidecar run, it first deletes `curl` from the Ceryx container:

```sh
docker build --target final -t ceryx:v1 --build-context root=../ceryx ../ceryx/v1
CERYX_E2E_IMAGE=ceryx:v1 TF_ACC=1 go test ./internal/provider -run E2E
```

Regenerate the documentation after schema changes:

```sh
go run github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs@v0.24.0 generate --provider-name ceryx --rendered-provider-name Ceryx
```

To try the provider CLI before a Registry release, install it in a filesystem mirror. For example, on macOS with Apple Silicon:

```sh
mkdir -p /tmp/ceryx-mirror/registry.terraform.io/withlogicco/ceryx/0.1.0/darwin_arm64
go build -o /tmp/ceryx-mirror/registry.terraform.io/withlogicco/ceryx/0.1.0/darwin_arm64/terraform-provider-ceryx_v0.1.0 .
```

Create `/tmp/ceryx.tfrc`:

```hcl
provider_installation {
  filesystem_mirror {
    path    = "/tmp/ceryx-mirror"
    include = ["registry.terraform.io/withlogicco/ceryx"]
  }

  direct {
    exclude = ["registry.terraform.io/withlogicco/ceryx"]
  }
}
```

Then, in a configuration that requires `withlogicco/ceryx` version `0.1.0`:

```sh
TF_CLI_CONFIG_FILE=/tmp/ceryx.tfrc terraform init
TF_CLI_CONFIG_FILE=/tmp/ceryx.tfrc terraform plan
```

## Documentation

- [Provider configuration](docs/index.md)
- [`ceryx_route` resource](docs/resources/route.md)
- [`ceryx_route` data source](docs/data-sources/route.md)
- [`ceryx_routes` data source](docs/data-sources/routes.md)
