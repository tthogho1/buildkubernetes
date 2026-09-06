# Kubernetes on AWS EC2 with Terraform and Ansible

A hands-on guide to building a Kubernetes cluster on AWS EC2 with Terraform and Ansible.

- **Terraform**: provisions the VPC, subnet, security group, and EC2 instances (master/worker)
- **Ansible**: installs containerd and kubeadm/kubelet/kubectl on the EC2 nodes, then
  initializes the cluster, joins the workers, and applies the CNI with kubeadm
- **Assumes**: AWS CLI, Terraform, and Ansible are available on your local machine (or a
  CloudShell equivalent)

Target versions: Ubuntu 22.04 LTS / Kubernetes v1.31 / Calico v3.29.1

The measured infrastructure specification — instance sizing, versions from a real run,
IAM, and cost — is in [`k8s-aws-terraform-ansible/README.md`](k8s-aws-terraform-ansible/README.md).

## Prerequisites and setup

### Required tools

- AWS CLI (authenticated)
- Terraform CLI (v1.5 or newer)
- Ansible (v2.14 or newer)
- An SSH key pair for connecting to EC2 (a key pair already created in AWS, plus the
  matching `.pem` file on your machine)

### AWS credentials

Do not use root access keys. This hands-on uses a dedicated IAM user:

- IAM user: `k8s-handson-tf`
- Policy: `K8sHandsonTerraformEC2` (`iam/k8s-handson-policy.json`)
  - `ec2:*` restricted to **ap-northeast-1 only**
  - Explicit Deny on `iam:*` and billing actions, to prevent privilege escalation
- Local profile: `k8s-handson`

There is no default profile configured, so specify the profile for every command:

```bash
export AWS_PROFILE=k8s-handson
aws sts get-caller-identity   # OK if it returns arn:...:user/k8s-handson-tf
```

To recreate the IAM user:

```bash
aws iam create-user   --user-name k8s-handson-tf
aws iam create-policy --policy-name K8sHandsonTerraformEC2 \
  --policy-document file://iam/k8s-handson-policy.json
aws iam attach-user-policy --user-name k8s-handson-tf \
  --policy-arn arn:aws:iam::<ACCOUNT_ID>:policy/K8sHandsonTerraformEC2
```

### SSH key pair

The key pair used here is `k8s-handson`, with the private key at `~/.ssh/k8s-handson.pem`.
To recreate it:

```bash
umask 077
aws ec2 create-key-pair --region ap-northeast-1 \
  --key-name k8s-handson --key-type rsa \
  --query KeyMaterial --output text > ~/.ssh/k8s-handson.pem
chmod 600 ~/.ssh/k8s-handson.pem
```

The private key must be mode `600`, otherwise SSH refuses to use it.

## The overall flow

1. Terraform creates the VPC and EC2 instances (1 master + 2 workers)
2. Ansible installs containerd, kubeadm, kubelet, and kubectl on every node
3. Ansible runs `kubeadm init` on the master and `kubeadm join` on the workers
4. The CNI (Calico) is applied and the cluster is complete
5. Verify with kubectl and deploy a test workload

Sections 1-6 below walk through this one command at a time. Section 7 covers `cluster.sh`,
which runs the whole sequence for you.

## 1. Build the EC2 infrastructure with Terraform

### Directory layout

```text
k8s-aws-terraform-ansible/
├─ terraform/
│  ├─ main.tf
│  ├─ variables.tf
│  ├─ outputs.tf
│  └─ terraform.tfvars.example
├─ ansible/
│  ├─ ansible.cfg
│  ├─ inventory.ini        # Replace the IPs with the terraform output values
│  └─ playbooks/
│     ├─ 01_deps.yml       # containerd + kubeadm/kubelet/kubectl
│     ├─ 02_k8s_init.yml   # kubeadm init / join
│     └─ 03_cni.yml        # Calico
├─ iam/
│  └─ k8s-handson-policy.json   # Permissions for the executing IAM user
├─ cluster.sh                   # One-command wrapper (up / down / status / ssh)
└─ README.md
```

### `terraform/variables.tf`

```hcl
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
```

### `terraform/main.tf`

```hcl
terraform {
  required_version = ">= 1.5.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

# VPC
resource "aws_vpc" "k8s_vpc" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true

  tags = {
    Name = "k8s-vpc"
  }
}

# Public subnet
resource "aws_subnet" "k8s_subnet" {
  vpc_id                  = aws_vpc.k8s_vpc.id
  cidr_block              = "10.0.1.0/24"
  availability_zone       = "${var.region}a"
  map_public_ip_on_launch = true

  tags = {
    Name = "k8s-subnet"
  }
}

# Internet Gateway
resource "aws_internet_gateway" "k8s_igw" {
  vpc_id = aws_vpc.k8s_vpc.id

  tags = {
    Name = "k8s-igw"
  }
}

# Route table
resource "aws_route_table" "k8s_rt" {
  vpc_id = aws_vpc.k8s_vpc.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.k8s_igw.id
  }

  tags = {
    Name = "k8s-rt"
  }
}

resource "aws_route_table_association" "k8s_rta" {
  subnet_id      = aws_subnet.k8s_subnet.id
  route_table_id = aws_route_table.k8s_rt.id
}

# Security group: unrestricted between nodes, your IP only from outside
resource "aws_security_group" "k8s_sg" {
  name        = "k8s-sg"
  description = "Security group for K8s nodes"
  vpc_id      = aws_vpc.k8s_vpc.id

  # SSH (your IP only)
  ingress {
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.my_ip_cidr]
  }

  # kube-apiserver (your IP only)
  ingress {
    from_port   = 6443
    to_port     = 6443
    protocol    = "tcp"
    cidr_blocks = [var.my_ip_cidr]
  }

  # NodePort, for smoke-testing (your IP only)
  ingress {
    from_port   = 30000
    to_port     = 32767
    protocol    = "tcp"
    cidr_blocks = [var.my_ip_cidr]
  }

  # Node-to-node traffic: allow everything between instances in this SG
  # With protocol = "-1", from_port / to_port must both be 0
  ingress {
    from_port = 0
    to_port   = 0
    protocol  = "-1"
    self      = true
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "k8s-sg"
  }
}

# Look up the latest Ubuntu 22.04 LTS AMI
data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# Master EC2
resource "aws_instance" "k8s_master" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.master_instance_type
  key_name               = var.key_name
  subnet_id              = aws_subnet.k8s_subnet.id
  vpc_security_group_ids = [aws_security_group.k8s_sg.id]

  root_block_device {
    volume_size = var.root_volume_size
    volume_type = "gp3"
  }

  tags = {
    Name = "k8s-master"
    Role = "master"
  }
}

# Worker EC2 instances (count for multiple)
resource "aws_instance" "k8s_worker" {
  count                  = var.worker_count
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = var.worker_instance_type
  key_name               = var.key_name
  subnet_id              = aws_subnet.k8s_subnet.id
  vpc_security_group_ids = [aws_security_group.k8s_sg.id]

  root_block_device {
    volume_size = var.root_volume_size
    volume_type = "gp3"
  }

  tags = {
    Name = "k8s-worker-${count.index}"
    Role = "worker"
  }
}
```

### `terraform/outputs.tf`

```hcl
output "master_public_ip" {
  value = aws_instance.k8s_master.public_ip
}

output "master_private_ip" {
  value = aws_instance.k8s_master.private_ip
}

output "worker_public_ips" {
  value = aws_instance.k8s_worker[*].public_ip
}
```

### `terraform/terraform.tfvars.example`

`key_name` and `my_ip_cidr` have no defaults, so they are supplied through
`terraform.tfvars`. That file is gitignored; copy the example and fill in your own values.

```hcl
# Copy to terraform.tfvars and replace with your own values:
#   cp terraform.tfvars.example terraform.tfvars
# (terraform.tfvars is gitignored)

key_name   = "your-key-name"      # An EC2 key pair that already exists in AWS
my_ip_cidr = "203.0.113.10/32"    # Output of `curl -s ifconfig.me`, plus /32

# Optional below; omitting them uses the defaults in variables.tf
# region               = "ap-northeast-1"
# master_instance_type = "t3.medium"
# worker_instance_type = "t3.medium"
# worker_count         = 2
# root_volume_size     = 30
```

### Running Terraform

```bash
export AWS_PROFILE=k8s-handson
cd terraform
cp terraform.tfvars.example terraform.tfvars   # then edit key_name / my_ip_cidr
terraform init
terraform plan -out=tfplan     # Expected: Plan: 9 to add
terraform apply tfplan
terraform output
```

Example output:

```text
master_public_ip    = "3.112.xxx.xxx"
master_private_ip   = "10.0.1.10"
worker_public_ips   = ["13.230.xxx.xxx", "13.231.xxx.xxx"]
```

These IPs are what you put into the Ansible inventory.

If your global IP changes, the security group's allowed source no longer matches and SSH
stops working. Update `my_ip_cidr` in `terraform.tfvars` and re-run `terraform apply`.

## 2. Create the Ansible inventory

### `ansible/ansible.cfg`

```ini
[defaults]
inventory      = inventory.ini
host_key_checking = False
retry_files_enabled = False
interpreter_python = /usr/bin/python3

[ssh_connection]
pipelining = True
```

The inventory path is relative, so run the ansible commands from inside the `ansible/`
directory (that is what the examples below do).

### `ansible/inventory.ini`

```ini
# Placeholder IPs (RFC 5737 documentation range).
# Populate with real values via: ./cluster.sh inventory

[masters]
master ansible_host=203.0.113.10

[workers]
worker1 ansible_host=203.0.113.11
worker2 ansible_host=203.0.113.12

[all:vars]
ansible_user=ubuntu
ansible_ssh_private_key_file=~/.ssh/k8s-handson.pem
ansible_python_interpreter=/usr/bin/python3
ansible_ssh_common_args='-o StrictHostKeyChecking=accept-new'
```

The checked-in IPs are RFC 5737 documentation placeholders. Replace them with the values
from `terraform output`, or let `./cluster.sh inventory` fill them in for you.

Check SSH connectivity:

```bash
cd ../ansible
ansible all -m ping
```

(SSH is not always up immediately after the EC2 instances boot. If this fails, wait a
minute and try again.)

## 3. Ansible playbook: container runtime and Kubernetes packages

dockershim was removed in Kubernetes v1.24, so the container runtime is containerd (which
speaks CRI). Installing Docker Engine (`docker-ce`) gives kubelet nothing it can use.

### `ansible/playbooks/01_deps.yml`

```yaml
- name: Install Kubernetes dependencies on all nodes
  hosts: all
  become: true
  vars:
    k8s_minor: "v1.31"

  tasks:
    # --- kubeadm prerequisites: swap off, kernel modules, sysctl ---
    - name: Disable swap immediately
      command: swapoff -a
      when: ansible_swaptotal_mb | int > 0

    - name: Disable swap permanently in /etc/fstab
      replace:
        path: /etc/fstab
        regexp: '^([^#].*\sswap\s.*)$'
        replace: '# \1'

    - name: Load required kernel modules at boot
      copy:
        dest: /etc/modules-load.d/k8s.conf
        content: |
          overlay
          br_netfilter
        mode: "0644"

    - name: Load kernel modules now
      # Avoid community.general.modprobe: it cannot be resolved by a bare
      # ansible-core install. modprobe exits 0 on an already-loaded module,
      # so this stays idempotent.
      command: "modprobe {{ item }}"
      loop:
        - overlay
        - br_netfilter
      changed_when: false

    - name: Set sysctl params required by kubeadm
      copy:
        dest: /etc/sysctl.d/99-kubernetes.conf
        content: |
          net.bridge.bridge-nf-call-iptables  = 1
          net.bridge.bridge-nf-call-ip6tables = 1
          net.ipv4.ip_forward                 = 1
        mode: "0644"
      notify: Reload sysctl

    # --- Repository setup (apt_key is deprecated, so use the keyring method) ---
    - name: Install common packages
      apt:
        name:
          - apt-transport-https
          - ca-certificates
          - curl
          - gnupg
          # Commands kubeadm's preflight requires. They are absent from the
          # minimal Ubuntu AMI and are not pulled in as kubelet dependencies.
          - conntrack
          - socat
          - ethtool
        state: present
        update_cache: yes
        cache_valid_time: 3600

    - name: Create apt keyring directory
      file:
        path: /etc/apt/keyrings
        state: directory
        mode: "0755"

    - name: Add Docker GPG key (dearmored)
      shell: |
        set -o pipefail
        curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
          | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
      args:
        executable: /bin/bash
        creates: /etc/apt/keyrings/docker.gpg

    - name: Add Docker repository (provides containerd.io)
      apt_repository:
        repo: >-
          deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg]
          https://download.docker.com/linux/ubuntu
          {{ ansible_distribution_release }} stable
        filename: docker
        state: present

    - name: Install containerd
      apt:
        name: containerd.io
        state: present
        update_cache: yes

    # --- containerd config (the docker-ce build ships with CRI disabled) ---
    # The containerd.io package ships a config.toml containing nothing but
    # disabled_plugins = ["cri"]. A plain `creates:` existence check would skip
    # regeneration, leaving CRI disabled and making kubeadm init fail.
    - name: Check containerd config state
      shell: |
        test -f /etc/containerd/config.toml || exit 2   # missing
        grep -q 'disabled_plugins.*"cri"' /etc/containerd/config.toml && exit 0  # shipped stub
        exit 1                                          # already generated
      args:
        executable: /bin/bash
      register: containerd_cfg_state
      failed_when: false
      changed_when: false

    - name: Generate containerd config with CRI enabled
      shell: containerd config default > /etc/containerd/config.toml
      when: containerd_cfg_state.rc != 1
      notify: Restart containerd

    - name: Use systemd cgroup driver (match kubelet's default)
      replace:
        path: /etc/containerd/config.toml
        regexp: 'SystemdCgroup = false'
        replace: 'SystemdCgroup = true'
      notify: Restart containerd

    - name: Ensure containerd is running and enabled
      systemd:
        name: containerd
        state: started
        enabled: yes

    # --- Kubernetes packages ---
    - name: Add Kubernetes GPG key
      shell: |
        set -o pipefail
        curl -fsSL https://pkgs.k8s.io/core:/stable:/{{ k8s_minor }}/deb/Release.key \
          | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
      args:
        executable: /bin/bash
        creates: /etc/apt/keyrings/kubernetes-apt-keyring.gpg

    - name: Add Kubernetes repository
      apt_repository:
        repo: >-
          deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg]
          https://pkgs.k8s.io/core:/stable:/{{ k8s_minor }}/deb/ /
        filename: kubernetes
        state: present

    - name: Install kubelet, kubeadm, kubectl
      apt:
        name:
          - kubelet
          - kubeadm
          - kubectl
        state: present
        update_cache: yes

    - name: Hold kube packages (stop an auto-upgrade from breaking the cluster)
      dpkg_selections:
        name: "{{ item }}"
        selection: hold
      loop:
        - kubelet
        - kubeadm
        - kubectl

    - name: Enable kubelet
      systemd:
        name: kubelet
        enabled: yes

  handlers:
    - name: Reload sysctl
      command: sysctl --system

    - name: Restart containerd
      systemd:
        name: containerd
        state: restarted
        enabled: yes
        daemon_reload: yes
```

Run it:

```bash
ansible-playbook playbooks/01_deps.yml
```

## 4. Ansible playbook: initialize and join the cluster with kubeadm

`--apiserver-advertise-address` takes the private IP actually assigned to the node's NIC.
An EC2 public IP does not exist on any interface, so passing it makes `kubeadm init` fail.
The public IP goes into `--apiserver-cert-extra-sans` instead, so that the certificate
covers it and you can run kubectl from your own machine.

### `ansible/playbooks/02_k8s_init.yml`

```yaml
- name: Initialize Kubernetes control-plane
  hosts: masters
  become: true
  tasks:
    - name: Initialize cluster with kubeadm
      command: >
        kubeadm init
        --pod-network-cidr=192.168.0.0/16
        --apiserver-advertise-address={{ ansible_default_ipv4.address }}
        --apiserver-cert-extra-sans={{ ansible_host }}
      args:
        creates: /etc/kubernetes/admin.conf

    - name: Prepare .kube directory for ubuntu user
      file:
        path: /home/ubuntu/.kube
        state: directory
        owner: ubuntu
        group: ubuntu
        mode: "0755"

    - name: Copy admin.conf to user's kube config
      copy:
        src: /etc/kubernetes/admin.conf
        dest: /home/ubuntu/.kube/config
        remote_src: yes
        owner: ubuntu
        group: ubuntu
        mode: "0600" # Holds a client cert and key; keep it unreadable to other users

    - name: Generate join command
      command: kubeadm token create --print-join-command
      register: join_cmd
      changed_when: false
      no_log: true # Keep the token out of the logs

- name: Join worker nodes to the cluster
  hosts: workers
  become: true
  tasks:
    - name: Join cluster
      command: "{{ hostvars[groups['masters'][0]]['join_cmd']['stdout'] }}"
      args:
        creates: /etc/kubernetes/kubelet.conf
      no_log: true
```

Run it:

```bash
ansible-playbook playbooks/02_k8s_init.yml
```

Note: the join command is registered on the master and passed to the workers through
`hostvars`. Writing it that way keeps a file containing the token off of disk.

## 5. Ansible playbook: apply the CNI (Calico)

### `ansible/playbooks/03_cni.yml`

```yaml
- name: Apply CNI (Calico)
  hosts: masters
  become: true
  vars:
    calico_version: "v3.29.1" # Use a full tag including the patch version
  tasks:
    - name: Apply Calico manifest
      command: >
        kubectl apply -f
        https://raw.githubusercontent.com/projectcalico/calico/{{ calico_version }}/manifests/calico.yaml
      environment:
        KUBECONFIG: /etc/kubernetes/admin.conf
      register: calico_apply
      changed_when: "'created' in calico_apply.stdout or 'configured' in calico_apply.stdout"

    - name: Wait for all nodes to become Ready
      command: kubectl wait --for=condition=Ready nodes --all --timeout=300s
      environment:
        KUBECONFIG: /etc/kubernetes/admin.conf
      changed_when: false
```

`kubectl apply` is idempotent on its own, so no flag-file guard is needed.

Run it:

```bash
ansible-playbook playbooks/03_cni.yml
```

## 6. Check the cluster and verify it works

On the master node:

```bash
ssh -i ~/.ssh/k8s-handson.pem ubuntu@<master-public-ip>
kubectl get nodes
kubectl get pods -A
```

It worked if every node is Ready and the kube-system Pods are Running:

```text
NAME           STATUS   ROLES           VERSION    CONTAINER-RUNTIME
ip-10-0-1-69   Ready    control-plane   v1.31.14   containerd://2.3.4
ip-10-0-1-58   Ready    <none>          v1.31.14   containerd://2.3.4
ip-10-0-1-52   Ready    <none>          v1.31.14   containerd://2.3.4
```

A sample deployment (nginx):

```bash
kubectl create deployment nginx --image=nginx
kubectl expose deployment nginx --port=80 --type=NodePort
kubectl get svc nginx
```

The assigned NodePort (30000-32767) is already opened to your own IP by the Terraform
security group, so you can reach it from a browser at
`http://<any node's public IP>:<NodePort>`.

### (Optional) Using kubectl from your local machine

```bash
scp -i ~/.ssh/k8s-handson.pem ubuntu@<master-public-ip>:~/.kube/config ./kubeconfig
# Rewrite the server's private IP to the public IP (BSD sed; on GNU sed drop the '')
sed -i '' "s#server: https://10\..*:6443#server: https://<master-public-ip>:6443#" ./kubeconfig
export KUBECONFIG=$PWD/kubeconfig
kubectl get nodes
```

(The public IP is in `--apiserver-cert-extra-sans`, so this does not produce a certificate
error.)

## 7. `cluster.sh`: all of the above in one command

`cluster.sh` wraps the whole sequence, so you do not have to drive Terraform and Ansible
separately:

```bash
./cluster.sh up        # Create EC2 and build the cluster (~7 min)
./cluster.sh status    # kubectl get nodes / pods
./cluster.sh ssh       # SSH into the master
./cluster.sh down      # Destroy everything
./cluster.sh inventory # Refresh ansible/inventory.ini from terraform output
```

`up` runs preflight checks (required tools, SSH key, AWS credentials, and whether your
global IP still matches `my_ip_cidr`), applies the Terraform, refreshes
`ansible/inventory.ini` from the outputs, waits for SSH, then runs all three playbooks in
order. It prompts once before creating billable resources; add `-y` to skip that.

### `cluster.sh`

```bash
#!/usr/bin/env bash
#
# Single entry point for the k8s hands-on cluster.
#
#   ./cluster.sh up        Create EC2 + build the Kubernetes cluster (~7 min)
#   ./cluster.sh down      Destroy everything
#   ./cluster.sh status    Show cluster state
#   ./cluster.sh ssh       SSH into the master node
#   ./cluster.sh inventory Refresh ansible/inventory.ini from terraform output
#
# Add -y to skip the confirmation prompt: ./cluster.sh up -y
#
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

: "${AWS_PROFILE:=k8s-handson}"
export AWS_PROFILE

REGION="ap-northeast-1"
KEY="$HOME/.ssh/k8s-handson.pem"
TF="terraform -chdir=terraform"

red()  { printf '\033[31m%s\033[0m\n' "$*"; }
grn()  { printf '\033[32m%s\033[0m\n' "$*"; }
bold() { printf '\033[1m%s\033[0m\n' "$*"; }
die()  { red "ERROR: $*" >&2; exit 1; }

preflight() {
  for c in terraform ansible ansible-playbook aws; do
    command -v "$c" >/dev/null || die "$c is not installed"
  done
  [ -f "$KEY" ] || die "SSH key not found: $KEY (see README, 'SSH key pair')"
  aws sts get-caller-identity >/dev/null 2>&1 \
    || die "AWS profile '$AWS_PROFILE' has no valid credentials"

  # Warn if the security group's allowed source no longer matches this machine.
  local want have
  want=$(grep -E '^my_ip_cidr' terraform/terraform.tfvars 2>/dev/null \
         | sed 's/.*= *"\([^"]*\)".*/\1/' || true)
  have="$(curl -s --max-time 10 ifconfig.me || true)"
  if [ -n "$want" ] && [ -n "$have" ] && [ "$want" != "$have/32" ]; then
    red "WARNING: your global IP ($have) does not match my_ip_cidr ($want)."
    red "         SSH will be blocked. Update terraform/terraform.tfvars and re-run."
  fi
}

refresh_inventory() {
  local m w0 w1
  m=$($TF output -raw master_public_ip)
  read -r w0 w1 <<<"$($TF output -json worker_public_ips \
        | python3 -c 'import json,sys; print(*json.load(sys.stdin))')"
  [ -n "$m" ] && [ -n "$w0" ] && [ -n "$w1" ] || die "could not read terraform outputs"

  # Portable in-place sed for both macOS (BSD) and Linux (GNU) sed
  if sed --version >/dev/null 2>&1; then
    # GNU sed (Linux / Docker container)
    sed -i \
      -e "s/^master ansible_host=.*/master ansible_host=$m/" \
      -e "s/^worker1 ansible_host=.*/worker1 ansible_host=$w0/" \
      -e "s/^worker2 ansible_host=.*/worker2 ansible_host=$w1/" \
      ansible/inventory.ini
  else
    # BSD sed (macOS)
    sed -i '' \
      -e "s/^master ansible_host=.*/master ansible_host=$m/" \
      -e "s/^worker1 ansible_host=.*/worker1 ansible_host=$w0/" \
      -e "s/^worker2 ansible_host=.*/worker2 ansible_host=$w1/" \
      ansible/inventory.ini
  fi
  grn "inventory updated: master=$m worker1=$w0 worker2=$w1"
}

wait_for_ssh() {
  bold "Waiting for SSH on all nodes..."
  local i
  for i in $(seq 1 40); do
    if (cd ansible && ansible all -m ping >/dev/null 2>&1); then
      grn "all nodes reachable (~$((i * 5))s)"
      return 0
    fi
    sleep 5
  done
  die "nodes still unreachable after 200s"
}

cmd_up() {
  preflight
  if [ "${1:-}" != "-y" ]; then
    bold "This creates 3x t3.medium in $REGION — roughly \$4-5/day."
    read -r -p "Continue? [y/N] " a
    [[ "$a" =~ ^[Yy]$ ]] || { echo "aborted"; exit 0; }
  fi

  bold "[1/4] terraform apply"
  $TF init -input=false >/dev/null
  $TF apply -auto-approve

  bold "[2/4] refresh inventory"
  refresh_inventory
  wait_for_ssh

  bold "[3/4] install containerd + kubeadm"
  (cd ansible && ansible-playbook playbooks/01_deps.yml)

  bold "[4/4] kubeadm init/join + CNI"
  (cd ansible && ansible-playbook playbooks/02_k8s_init.yml)
  (cd ansible && ansible-playbook playbooks/03_cni.yml)

  grn "Cluster is up."
  cmd_status
  echo
  bold "Remember: ./cluster.sh down  (it bills ~\$4-5/day while running)"
}

cmd_status() {
  if [ "$($TF state list 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then
    echo "No infrastructure. Run: ./cluster.sh up"
    return 0
  fi
  (cd ansible && ansible master -b -m shell \
     -a "KUBECONFIG=/etc/kubernetes/admin.conf kubectl get nodes; \
         echo; KUBECONFIG=/etc/kubernetes/admin.conf kubectl get pods -A" \
     2>/dev/null | sed -n '2,60p')
}

cmd_ssh() {
  exec ssh -i "$KEY" -o StrictHostKeyChecking=accept-new \
    "ubuntu@$($TF output -raw master_public_ip)"
}

cmd_down() {
  if [ "${1:-}" != "-y" ]; then
    read -r -p "Destroy all infrastructure? [y/N] " a
    [[ "$a" =~ ^[Yy]$ ]] || { echo "aborted"; exit 0; }
  fi
  $TF destroy -auto-approve
  grn "Destroyed. Billing stopped."
}

case "${1:-}" in
  up)        shift; cmd_up "${1:-}" ;;
  down)      shift; cmd_down "${1:-}" ;;
  status)    cmd_status ;;
  ssh)       cmd_ssh ;;
  inventory) refresh_inventory ;;
  *) sed -n '3,12p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
```

## 8. Cleanup

Deleting the EC2 instances deletes the cluster with them:

```bash
cd terraform
terraform destroy
```

Or:

```bash
./cluster.sh down
```

This is not free-tier eligible — three `t3.medium` instances plus their gp3 volumes and
public IPv4 addresses run roughly **$4-5 per day**. Always destroy when you are finished.

## Common stumbling blocks

- If swap is still enabled, `kubeadm init` stops with a preflight error (`01_deps.yml`
  disables it).
- An unloaded `br_netfilter`, or `net.ipv4.ip_forward=0`, is rejected by preflight the
  same way.
- kubeadm's preflight also requires `conntrack`, `socat`, and `ethtool`. None of them are
  on the minimal Ubuntu AMI and none arrive as kubelet dependencies, so `01_deps.yml`
  installs them explicitly.
- When containerd comes from the docker-ce repository, its default `config.toml` has the
  CRI plugin disabled, so the config has to be regenerated with `containerd config default`.
- If the cgroup driver differs between containerd (cgroupfs) and kubelet (systemd), init
  succeeds but Pods never start. Always set `SystemdCgroup = true`.
- A security group rule with `protocol = "-1"` must set `from_port` / `to_port` to **0**.
  That is an AWS requirement; writing `0-65535` makes the apply fail.
- If your global IP changes, SSH and the apiserver become unreachable. Update `my_ip_cidr`
  in `terraform.tfvars` and re-apply.
- To retry after a failure, run `sudo kubeadm reset -f` on the affected node first.

## Notes and next steps

- **Using Kubespray**: for a more production-shaped setup with stronger version management,
  it is common to run Kubespray (an Ansible-based K8s distribution) against EC2 instances
  created by Terraform.
- **Redundancy**: in production you would run three control-plane nodes behind an NLB, with
  etcd in a quorum configuration.
- **Private subnet + SSM**: for tighter security you can drop the public IPs entirely and
  connect through AWS SSM Session Manager.
- **Generating the inventory automatically**: piping `terraform output -json` through `jq`
  to generate `inventory.ini` removes the hand-copying of IPs. `cluster.sh` does this
  already, in its `refresh_inventory` function.
- **Containers for the toolchain**: the `Dockerfile` and `docker-compose.yml` in the
  project directory build an image with Terraform, Ansible, and the AWS CLI, mounting
  `~/.aws` and `~/.ssh` read-only, so you can run the hands-on without installing anything
  locally.
