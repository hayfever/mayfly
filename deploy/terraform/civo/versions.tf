terraform {
  required_version = ">= 1.6"
  required_providers {
    civo = {
      source  = "civo/civo"
      version = "~> 1.4"
    }
  }
}

# The token comes from the CIVO_TOKEN environment variable.
provider "civo" {
  region = var.region
}
