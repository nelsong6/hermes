# ============================================================================
# Workload identity for hermes
# ============================================================================
# Hermes-owned identity lives in the `hermes` resource group:
# - `hermes-identity` for the StatefulSet pod
#
# Federated credentials use exact-match subjects. The Hermes pod uses
# `system:serviceaccount:hermes:hermes` (chart's namespace + service account).
#
# Scope is intentionally narrow at scaffold time. Runtime secrets are projected
# by External Secrets from the Hermes-owned Key Vault; add direct role grants
# here only as integrations come online (e.g. Cosmos data-plane RBAC if Hermes
# ever uses Cosmos, Storage Blob Data Contributor for an artifact store, etc.).
# ============================================================================

data "azurerm_resource_group" "infra" {
  name = local.infra.resource_group_name
}

resource "azurerm_resource_group" "hermes" {
  name     = "hermes"
  location = data.azurerm_resource_group.infra.location
}

resource "azurerm_user_assigned_identity" "hermes_dedicated" {
  name                = "hermes-identity"
  resource_group_name = azurerm_resource_group.hermes.name
  location            = azurerm_resource_group.hermes.location
}

resource "azurerm_federated_identity_credential" "hermes_dedicated" {
  name                = "aks-hermes"
  resource_group_name = azurerm_resource_group.hermes.name
  parent_id           = azurerm_user_assigned_identity.hermes_dedicated.id
  audience            = ["api://AzureADTokenExchange"]
  issuer              = local.aks_oidc_issuer_url
  subject             = "system:serviceaccount:hermes:hermes"
}

output "hermes_dedicated_identity_client_id" {
  value       = azurerm_user_assigned_identity.hermes_dedicated.client_id
  description = "client_id of the Hermes-owned hermes-identity. Pin this into k8s/values.yaml::serviceAccountClientId."
}
