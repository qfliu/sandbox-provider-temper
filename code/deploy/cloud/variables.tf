variable "subscription_id" {
  description = "Azure subscription to deploy into (az account show --query id -o tsv)."
  type        = string
}

variable "name" {
  description = "Name for the resource group, cluster and DNS prefix (registry: name + 6 random chars)."
  type        = string
  default     = "sandbox-provider"
}

variable "location" {
  description = "Azure region."
  type        = string
  default     = "westus2"
}

variable "node_size" {
  description = "Node VM size. Standard_D4s_v6: 4 vCPU, 16 GiB, x86 (Intel), with nested virtualization for Kata (D63, D70). arm64 sizes (e.g. Standard_D2ps_v5) still work for gVisor only."
  type        = string
  default     = "Standard_D4s_v6"
}

variable "node_count" {
  description = "Nodes in the pool."
  type        = number
  default     = 2
}

variable "kubernetes_version" {
  description = "AKS Kubernetes version; null for the region's default."
  type        = string
  default     = null
}

variable "admin_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API (your IP as x.x.x.x/32)."
  type        = list(string)
  validation {
    # An empty list would leave the API open to everyone.
    condition     = length(var.admin_cidrs) > 0 && alltrue([for c in var.admin_cidrs : can(cidrhost(c, 0))])
    error_message = "admin_cidrs must list at least one valid CIDR, e.g. [\"203.0.113.7/32\"]."
  }
}
