variable "key_vault_name" {
  description = "Hermes-owned Key Vault for app secrets projected into Kubernetes."
  type        = string
  default     = "ng6-hermes"
}
