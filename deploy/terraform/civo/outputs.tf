output "cluster_name" {
  value = civo_kubernetes_cluster.this.name
}

output "cluster_id" {
  value = civo_kubernetes_cluster.this.id
}

output "api_endpoint" {
  value = civo_kubernetes_cluster.this.api_endpoint
}

output "kubeconfig_command" {
  value = "civo kubernetes config ${civo_kubernetes_cluster.this.name} --region ${var.region} --save"
}
