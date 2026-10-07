output "resource_group" { value = azurerm_resource_group.main.name }
output "cluster_name" { value = azurerm_kubernetes_cluster.main.name }
output "acr_login_server" { value = azurerm_container_registry.main.login_server }
output "acr_name" { value = azurerm_container_registry.main.name }
output "kubernetes_version" { value = azurerm_kubernetes_cluster.main.current_kubernetes_version }
