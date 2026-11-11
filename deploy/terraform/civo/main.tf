# A small Civo Kubernetes cluster for Mayfly. Cilium enforces the per-session
# NetworkPolicy; Civo's default StorageClass backs the control plane PVC.
resource "civo_network" "this" {
  label = var.name
}

resource "civo_firewall" "this" {
  name                 = var.name
  network_id           = civo_network.this.id
  create_default_rules = false

  ingress_rule {
    label      = "kubernetes-api"
    protocol   = "tcp"
    port_range = "6443"
    cidr       = var.api_allowed_cidrs
    action     = "allow"
  }

  ingress_rule {
    label      = "http"
    protocol   = "tcp"
    port_range = "80"
    cidr       = ["0.0.0.0/0"]
    action     = "allow"
  }

  ingress_rule {
    label      = "https"
    protocol   = "tcp"
    port_range = "443"
    cidr       = ["0.0.0.0/0"]
    action     = "allow"
  }

  egress_rule {
    label      = "all"
    protocol   = "tcp"
    port_range = "1-65535"
    cidr       = ["0.0.0.0/0"]
    action     = "allow"
  }

  egress_rule {
    label      = "all-udp"
    protocol   = "udp"
    port_range = "1-65535"
    cidr       = ["0.0.0.0/0"]
    action     = "allow"
  }
}

resource "civo_kubernetes_cluster" "this" {
  name               = var.name
  network_id         = civo_network.this.id
  firewall_id        = civo_firewall.this.id
  kubernetes_version = var.kubernetes_version
  cluster_type       = "k3s"
  cni                = "cilium"
  # Keep cluster credentials out of the Terraform state. Fetch them with
  # `civo kubernetes config <name> --region <region> --save`.
  write_kubeconfig = false

  pools {
    size       = var.node_size
    node_count = var.node_count
  }
}
