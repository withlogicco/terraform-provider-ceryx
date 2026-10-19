terraform {
  required_providers {
    ceryx = {
      source = "withlogicco/ceryx"
    }
  }
}

# Ceryx runs as a Docker Swarm service on a remote host. Its API is not
# published, so the provider runs curl in a sidecar container that shares
# the Ceryx container's network namespace.
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

# A Compose project on the local Docker daemon, selected by labels. Its
# Ceryx image includes curl, so the provider runs curl there with docker exec.
provider "ceryx" {
  alias = "local"

  docker {
    container {
      labels = {
        "com.docker.compose.project" = "edge"
        "com.docker.compose.service" = "ceryx"
      }
    }

    connector {
      type = "exec"
    }
  }
}

# The API is reachable directly, for example from a container on the same
# Docker network or through an SSH tunnel.
provider "ceryx" {
  alias    = "direct"
  endpoint = "http://ceryx:8080"
}
