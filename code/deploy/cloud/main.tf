# Azure infrastructure for the sandbox provider (Phase 7, D54): one resource
# group holding an AKS cluster (2 arm64 nodes) and a container registry the
# cluster pulls from. Applied and destroyed by code/scripts/cloud-up.sh and
# cloud-down.sh; state lives in .devenv/state, never in git.
terraform {
  required_version = ">= 1.16"
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 5.8"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.7"
    }
  }
  backend "local" {} # path given at init (-backend-config=path=...)
}

provider "azurerm" {
  features {
    resource_group {
      # Destroy removes the group even if Azure created extra resources in
      # it (e.g. the cluster's load balancer rules), so nothing keeps billing.
      prevent_deletion_if_contains_resources = false
    }
  }
  subscription_id = var.subscription_id
}

resource "azurerm_resource_group" "main" {
  name     = var.name
  location = var.location
}

# Images: the platform image and the two sandbox images. Basic SKU, no admin
# user; the cluster pulls with its kubelet identity (AcrPull below), so
# sandbox pods need no pull secret.
resource "azurerm_container_registry" "main" {
  name                = replace("${var.name}${random_string.acr.result}", "-", "")
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  sku                 = "Basic"
  admin_enabled       = false
}

resource "random_string" "acr" { # registry names are global
  length  = 6
  upper   = false
  special = false
}

resource "azurerm_kubernetes_cluster" "main" {
  name                = var.name
  resource_group_name = azurerm_resource_group.main.name
  location            = azurerm_resource_group.main.location
  dns_prefix          = var.name
  sku_tier            = "Free" # no control-plane charge
  kubernetes_version  = var.kubernetes_version

  default_node_pool {
    name       = "system"
    vm_size    = var.node_size
    node_count = var.node_count
    os_sku     = "Ubuntu"
    # The gVisor installer (step 7.3) edits containerd's config on the node,
    # so nodes are replaced rather than upgraded in place underneath it.
    upgrade_settings {
      max_surge = "1"
    }
  }

  # Our one fixed node pool above; no automatic node provisioning.
  node_provisioning_profile {
    mode = "Manual"
  }

  identity {
    type = "SystemAssigned"
  }

  network_profile {
    network_plugin      = "azure"
    network_plugin_mode = "overlay"
    network_policy      = "calico" # same enforcer as minikube (D37)
    load_balancer_sku   = "standard"
  }

  # Only the user's IP may reach the Kubernetes API.
  api_server_access_profile {
    authorized_ip_ranges = var.admin_cidrs
  }
}

resource "azurerm_role_assignment" "acr_pull" {
  scope                            = azurerm_container_registry.main.id
  role_definition_name             = "AcrPull"
  principal_id                     = azurerm_kubernetes_cluster.main.kubelet_identity[0].object_id
  skip_service_principal_aad_check = true
}
