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
  description = "Node VM size. Standard_D2ps_v5: 2 vCPU, 8 GiB, arm64 (Ampere), so the arm64 images run unchanged."
  type        = string
  default     = "Standard_D2ps_v5"
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
}
