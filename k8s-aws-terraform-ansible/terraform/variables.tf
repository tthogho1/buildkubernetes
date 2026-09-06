variable "region" {
  type    = string
  default = "ap-northeast-1"
}

variable "key_name" {
  type        = string
  description = "EC2 key pair name (already created in AWS)"
}

variable "my_ip_cidr" {
  type        = string
  description = "Your global IP in CIDR form, e.g. 203.0.113.10/32 (curl ifconfig.me)"
}

variable "master_instance_type" {
  type    = string
  default = "t3.medium" # kubeadm requires at least 2 vCPU and ~1700 MiB RAM
}

variable "worker_instance_type" {
  type    = string
  default = "t3.medium"
}

variable "worker_count" {
  type    = number
  default = 2
}

variable "root_volume_size" {
  type    = number
  default = 30 # The Ubuntu AMI default of 8GB runs out of space pulling images
}
