variable "region" {
  description = "Civo region."
  type        = string
  default     = "NYC1"
}

variable "name" {
  description = "Cluster, network, and firewall name."
  type        = string
  default     = "mayfly"
}

variable "kubernetes_version" {
  description = "k3s version from `civo kubernetes versions`. Mayfly needs 1.27+; the e2e suite needs 1.30+."
  type        = string
  default     = "1.35.0-k3s1"
}

variable "node_size" {
  description = "Node size from `civo kubernetes size`. g4s.kube.medium is 2 vCPU / 4 GB."
  type        = string
  default     = "g4s.kube.medium"
}

variable "node_count" {
  type    = number
  default = 1
}

variable "api_allowed_cidrs" {
  description = "CIDRs allowed to reach the Kubernetes API (6443)."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}
