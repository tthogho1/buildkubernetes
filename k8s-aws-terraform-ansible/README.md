# k8s-aws-terraform-ansible

A hands-on for building a Kubernetes cluster on AWS EC2 with Terraform and Ansible.

- Terraform: VPC / subnet / security group / EC2 (1 master + 2 workers)
- Ansible: installs containerd and kubeadm/kubelet/kubectl, initializes and joins
  the cluster, applies the CNI

Target versions: Ubuntu 22.04 LTS / Kubernetes v1.31 / Calico v3.29.1

The step-by-step walkthrough lives outside this directory in
[`../README.md`](../README.md), which embeds the files here verbatim.

## Infrastructure specification

Measured values from a real run in `ap-northeast-1` on 2026-09-05, taken all the
way through apply → cluster build → destroy. The versions are what was *verified
working*, not pins: the AMI and apt packages resolve to whatever is current at
run time, so they drift upward over time.

### Networking

| Item | Value | Notes |
|---|---|---|
| Region / AZ | `ap-northeast-1` / `ap-northeast-1a` | Single-AZ subnet |
| VPC CIDR | `10.0.0.0/16` | DNS hostnames and DNS resolution both enabled |
| Subnet | `10.0.1.0/24`, public | `map_public_ip_on_launch = true` |
| Routing | `0.0.0.0/0` → Internet Gateway | No NAT Gateway, to keep cost down |
| Pod network | `192.168.0.0/16` | Calico's default; does not overlap the VPC CIDR |

Keeping the VPC (`10.0.0.0/16`) and the Pod CIDR (`192.168.0.0/16`) disjoint
matters. If they overlap, node traffic and Pod traffic collide and the cluster
becomes unstable.

### Security group (`k8s-sg`)

| Direction | Port | Protocol | Source | Purpose |
|---|---|---|---|---|
| ingress | 22 | TCP | `var.my_ip_cidr` | SSH (Ansible connections) |
| ingress | 6443 | TCP | `var.my_ip_cidr` | kube-apiserver |
| ingress | 30000-32767 | TCP | `var.my_ip_cidr` | NodePort smoke test |
| ingress | all | all (`-1`) | itself (`self = true`) | Node-to-node and Pod traffic |
| egress | all | all (`-1`) | `0.0.0.0/0` | Package downloads, image pulls |

Everything exposed externally is restricted to your own IP. When your global IP
changes, update `my_ip_cidr` in `terraform.tfvars` and re-apply — otherwise you
lose SSH access.

A rule with `protocol = "-1"` must set `from_port` / `to_port` to **0**. That is
an AWS requirement; writing `0-65535` makes the apply fail.

### Compute

| Item | master | worker × 2 |
|---|---|---|
| Instance type | `t3.medium` | `t3.medium` |
| vCPU / memory | 2 / 4 GiB | 2 / 4 GiB |
| Root volume | 30 GiB `gp3` | 30 GiB `gp3` |
| Public IP | yes | yes |

- kubeadm's preflight requires **at least 2 vCPU and roughly 1700 MiB of RAM**.
  Every t3 size has 2 vCPU, so CPU is never the constraint — memory is.
  `t3.micro` (1024 MiB) fails the memory check; `t3.small` (2048 MiB) passes but
  leaves almost no headroom, so the control plane uses `t3.medium` (4096 MiB).
- t3 is burstable. In the default unlimited mode, CPU credit overage is billed,
  so `m5.large` is more predictable for anything long-running.
- The Ubuntu AMI's default root volume is only 8 GiB, which runs out while
  unpacking container images. It is raised to 30 GiB here.

### AMI

`data.aws_ami.ubuntu` selects `most_recent` matching the filters below rather
than hardcoding an ID.

| Filter | Value |
|---|---|
| owner | `099720109477` (Canonical) |
| name | `ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-amd64-server-*` |
| virtualization-type | `hvm` |

Measured: `ami-0fbfb486c68c1942d` (`...-20260904`) → Ubuntu 22.04.5 LTS,
kernel 6.8.0-1063-aws.

### Kubernetes stack

| Component | Measured version | Notes |
|---|---|---|
| Kubernetes | v1.31.14 | The apt repository is pinned to the `v1.31` line |
| Container runtime | containerd **2.3.4** | From the `containerd.io` package |
| containerd config | config **version 4** | Patched to `SystemdCgroup = true` |
| CNI | Calico v3.29.1 | Applied as a manifest |
| OS | Ubuntu 22.04.5 LTS | |

Key points about the design:

- **The runtime is containerd, not Docker.** dockershim was removed in
  Kubernetes v1.24, so installing `docker-ce` gives kubelet nothing it can use.
- The `containerd.io` package ships a `config.toml` containing nothing but
  `disabled_plugins = ["cri"]`. Skipping regeneration on a simple existence
  check leaves CRI disabled and `kubeadm init` fails, so the playbook detects
  that stub and regenerates the config.
- The cgroup driver is set to `SystemdCgroup = true` on the containerd side to
  match kubelet's default (systemd). A mismatch lets init succeed but leaves
  Pods unable to start.
- `--apiserver-advertise-address` gets the **private IP**. An EC2 public IP does
  not exist on any interface, so passing it makes init fail. The public IP goes
  into `--apiserver-cert-extra-sans` instead, so kubectl works from your machine.
- kubeadm's preflight requires `conntrack`, `socat`, and `ethtool`. None are on
  the minimal Ubuntu AMI, and none arrive as kubelet dependencies, so they are
  installed explicitly.

### Resulting cluster

```
NAME           STATUS   ROLES           VERSION    CONTAINER-RUNTIME
ip-10-0-1-69   Ready    control-plane   v1.31.14   containerd://2.3.4
ip-10-0-1-58   Ready    <none>          v1.31.14   containerd://2.3.4
ip-10-0-1-52   Ready    <none>          v1.31.14   containerd://2.3.4
```

13 Pods Running across all namespaces (the kube-system control-plane set plus
Calico). For verification, nginx was exposed as a Deployment + NodePort and
returned HTTP 200 from a local machine via all three nodes' public IPs
(14 Pods at that point).

### IAM

| Item | Value |
|---|---|
| Executing user | `k8s-handson-tf` |
| Policy | `K8sHandsonTerraformEC2` (`iam/k8s-handson-policy.json`) |
| Allows | `ec2:*`, **restricted to ap-northeast-1** |
| Explicit Deny | `iam:*` / `organizations:*` / `account:*` / `aws-portal:*` |
| Local profile | `k8s-handson` |

`terraform apply` and `destroy` were both confirmed to complete with only these
permissions. Root access keys are not used.

### Cost

| Item | Approximate (ap-northeast-1) |
|---|---|
| t3.medium × 3 | ~$0.16 / hour |
| gp3 30 GiB × 3 (90 GiB total) | ~$8.6 / month |
| Public IPv4 × 3 | ~$0.015 / hour |
| **Total** | **roughly $4-5 / day** |

Not free-tier eligible. Always `terraform destroy` when you are finished.

### What this setup does not provide

It is built for a hands-on, not for production.

- A single control-plane node (no redundancy, single etcd)
- Nodes sit directly in a public subnet (production would use private + NAT/SSM)
- No version pinning at `kubeadm init` time
- No Ingress controller, persistent volumes (EBS CSI), or monitoring
- The kubeconfig `server` points at a private IP, so it needs rewriting to use
  from your own machine

## Layout

```
k8s-aws-terraform-ansible/
├─ terraform/
│  ├─ main.tf
│  ├─ variables.tf
│  ├─ outputs.tf
│  └─ terraform.tfvars.example
├─ ansible/
│  ├─ ansible.cfg
│  ├─ inventory.ini        # Replace the IPs with the terraform output values
│  ├─ group_vars/all.yml   # k8s_minor, calico_version
│  └─ playbooks/
│     ├─ 01_deps.yml       # containerd + kubeadm/kubelet/kubectl
│     ├─ 02_k8s_init.yml   # kubeadm init / join
│     └─ 03_cni.yml        # Calico
├─ iam/
│  └─ k8s-handson-policy.json   # Permissions for the executing IAM user
├─ console/                     # Go + Fyne desktop app for editing settings (see below)
├─ cluster.sh                   # One-command wrapper (up / down / status / ssh)
└─ README.md
```

## Requirements

- AWS CLI (authenticated)
- Terraform v1.5 or newer
- Ansible v2.14 or newer
- An EC2 key pair and its `.pem` on your machine (`chmod 600`)

## AWS credentials

Do not use root access keys. Use the IAM user dedicated to this project.

- IAM user: `k8s-handson-tf`
- Policy: `K8sHandsonTerraformEC2` (`iam/k8s-handson-policy.json`)
  - `ec2:*` restricted to **ap-northeast-1 only**
  - Explicit Deny on `iam:*` and billing actions, to prevent privilege escalation
- Local profile: `k8s-handson`

**Specify the profile for every command** — there is no default profile configured:

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

## SSH key pair

Already created: `k8s-handson` (private key at `~/.ssh/k8s-handson.pem`).
To recreate it:

```bash
umask 077
aws ec2 create-key-pair --region ap-northeast-1 \
  --key-name k8s-handson --key-type rsa \
  --query KeyMaterial --output text > ~/.ssh/k8s-handson.pem
chmod 600 ~/.ssh/k8s-handson.pem
```

To confirm it matches the fingerprint AWS holds:

```bash
openssl pkcs8 -in ~/.ssh/k8s-handson.pem -nocrypt -topk8 -outform DER \
  | openssl sha1 -c | awk '{print $NF}'
aws ec2 describe-key-pairs --region ap-northeast-1 \
  --key-names k8s-handson --query 'KeyPairs[0].KeyFingerprint' --output text
```

## Quick start

`cluster.sh` wraps the whole sequence, so you do not have to drive Terraform and
Ansible separately:

```bash
./cluster.sh up        # Create EC2 and build the cluster (~7 min)
./cluster.sh status    # kubectl get nodes / pods
./cluster.sh ssh       # SSH into the master
./cluster.sh down      # Destroy everything
./cluster.sh scale 3   # Change the number of workers (drains removed nodes first)
```

`up` runs preflight checks (required tools, SSH key, AWS credentials, and
whether your global IP still matches `my_ip_cidr`), applies the Terraform,
refreshes `ansible/inventory.ini` from the outputs, waits for SSH, then runs all
three playbooks in order. It prompts once before creating billable resources;
add `-y` to skip that.

`./cluster.sh inventory` refreshes just the inventory, which is useful if you
applied by hand.

`./cluster.sh scale N` changes the number of workers on a running cluster (1–10):

- It plans with `worker_count=N` first, and refuses if the plan would change anything
  other than adding or removing workers. One case is a master replacement because
  Canonical published a newer Ubuntu AMI.
- **Scaling down**: it runs `kubectl drain` and `kubectl delete node` on the
  highest-numbered workers *before* Terraform destroys them.
- **Scaling up**: it creates the new instances, refreshes the inventory, and runs the
  playbooks only on the new workers (plus the master, to get a join token).
- It saves `worker_count` to `terraform.tfvars`, so a later `up` doesn't undo the change.
  If no cluster is running, it only saves the value.

Don't change the count of a running cluster with a plain `up` or `terraform apply`: they
destroy workers without draining them.

The manual steps below do the same thing, one command at a time.

## Steps

### 1. Create the infrastructure

```bash
export AWS_PROFILE=k8s-handson
cd terraform
# terraform.tfvars already exists (key_name / my_ip_cidr).
# If it does not, cp terraform.tfvars.example terraform.tfvars and fill it in.
terraform init
terraform plan -out=tfplan     # Expected: Plan: 9 to add
terraform apply tfplan
terraform output
```

If your global IP changes, the security group's allowed source no longer matches
and SSH stops working. Update `my_ip_cidr` in `terraform.tfvars` and re-run
`terraform apply`.

### 2. Update the inventory

Copy the IPs from `terraform output` into `ansible_host` in
`ansible/inventory.ini`, and point `ansible_ssh_private_key_file` at your key.

```bash
cd ../ansible
ansible all -m ping
```

### 3. Build the cluster

```bash
ansible-playbook playbooks/01_deps.yml
ansible-playbook playbooks/02_k8s_init.yml
ansible-playbook playbooks/03_cni.yml
```

### 4. Verify

```bash
ssh -i ~/.ssh/k8s-handson.pem ubuntu@$(cd ../terraform && terraform output -raw master_public_ip)
kubectl get nodes
kubectl get pods -A
```

### 5. Tear down

```bash
cd terraform
terraform destroy
```

## Management console

`console/` is a desktop app (Go + [Fyne](https://fyne.io)) that edits the cluster settings
and runs checks. It writes **data files only**:

| File | Fields |
|---|---|
| `terraform/terraform.tfvars` | `key_name`, `my_ip_cidr`, `master_instance_type`, `worker_instance_type`, `worker_count`, `root_volume_size` |
| `ansible/group_vars/all.yml` | `k8s_minor`, `calico_version` |
| `ansible/inventory.ini` (`[all:vars]` only) | `ansible_user`, `ansible_ssh_private_key_file` |

`region`, the pod/VPC CIDRs and the AMI are shown read-only because other
files pin them (IAM policy, `cluster.sh`, playbooks). Host IP lines are never written.

```bash
cd console
go run .                 # opens the window (-root to override auto-detection)
go test ./...            # includes a headless UI test (Fyne test driver)
```

Fyne needs cgo: on macOS the Xcode command-line tools are enough. On Linux, install a C
compiler and the GL/X11 headers (`libgl1-mesa-dev xorg-dev`). The Docker toolchain image has
no display, so run the console on the host.

Prebuilt binaries for Linux (amd64/arm64), Windows (amd64) and macOS (universal) are built
by `.github/workflows/console.yml` on every change to `console/`. Pushing a tag publishes them
as a GitHub Release:

```bash
git tag console-v1.0.0 && git push origin console-v1.0.0
```

A downloaded binary finds the repo by itself: it tries `-root`, then the last folder used,
the current directory, and the executable's folder. If none of those is the repo, it asks you
to choose the folder. The macOS binary is unsigned: run
`xattr -d com.apple.quarantine k8s-console` after extracting.

- Tabs: **Settings** (the form, with an estimated $/day and live warnings), **Nodes &
  read-only**, **Checks**.
- **Nodes** compares what is deployed (from terraform state, not `inventory.ini`) with
  `worker_count` in the form, and updates as you change it. Each worker shows `running`,
  `will be added by ./cluster.sh scale N`, or `WILL BE REMOVED (drained)`. Rows whose
  `inventory.ini` IP is out of date are flagged. With no cluster running, it lists what
  `./cluster.sh up` would create.
- **Save** checks every field first and writes only files whose content changes, atomically.
  It then shows the diff. Comments and layout in `terraform.tfvars` are kept.
- If `terraform.tfvars` is missing, the form loads `terraform.tfvars.example`, and **Save**
  creates the file (mode 0600).
- Check buttons: `terraform validate`, `terraform plan` (read-only AWS calls), and
  `ansible-playbook --syntax-check` plus `ansible-inventory --host master`. The console
  never runs `apply`, `destroy` or `cluster.sh`.

## Notes

- `terraform.tfvars`, `*.pem`, `kubeconfig`, and tfstate are all gitignored.
  tfstate contains sensitive values, so do not commit it.
- The security group opens SSH / 6443 / NodePort only to `my_ip_cidr`.
  Re-run `terraform apply` whenever your global IP changes.
- To start over, run `sudo kubeadm reset -f` on the affected nodes first.
