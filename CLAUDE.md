# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this repo is

A hands-on for building a kubeadm Kubernetes cluster (1 master + 2 workers, Ubuntu 22.04,
Kubernetes v1.31, Calico v3.29.1) on AWS EC2 in `ap-northeast-1`. Terraform provisions the
infrastructure; Ansible configures the nodes. All code lives in `k8s-aws-terraform-ansible/`.

There is no application code, build, or test suite. "Testing" means running against real
AWS, which **creates billable resources (~$4-5/day)** — don't run `up`/`apply` unless the
user asks, and remind them to `down` afterwards.

## Commands

Run from `k8s-aws-terraform-ansible/`. All AWS calls use the profile `k8s-handson`
(there is no default profile); `cluster.sh` sets it automatically.

```bash
./cluster.sh up [-y]     # preflight → terraform apply → refresh inventory → wait for SSH → playbooks 01,02,03
./cluster.sh status      # kubectl get nodes / pods -A on the master
./cluster.sh ssh         # SSH to master
./cluster.sh inventory   # rewrite ansible/inventory.ini IPs from terraform output
./cluster.sh scale N [-y] # change worker count on a running cluster (drains before destroy)
./cluster.sh down [-y]   # terraform destroy
```

Step by step:

```bash
export AWS_PROFILE=k8s-handson
terraform -chdir=terraform plan -out=tfplan      # expect "Plan: 9 to add"
cd ansible && ansible all -m ping                # must run inside ansible/ (relative inventory in ansible.cfg)
ansible-playbook playbooks/01_deps.yml           # also 02_k8s_init.yml, 03_cni.yml, in order
```

Static checks that don't touch AWS: `terraform -chdir=terraform validate` / `fmt`,
`ansible-playbook --syntax-check playbooks/<file>.yml`, `bash -n cluster.sh`.

`console/` is a Go + Fyne desktop app (`cd console && go run .`, `go test ./...`; needs cgo) that edits only the
data files `terraform/terraform.tfvars`, `ansible/group_vars/all.yml`, and the `[all:vars]`
lines of `ansible/inventory.ini`. Ansible version vars (`k8s_minor`, `calico_version`)
live in `group_vars/all.yml`. Don't add them back as play `vars:`, because play vars
override group_vars and would silently ignore console edits.

A containerized toolchain (Terraform, Ansible, AWS CLI) is available via
`docker compose run k8s-builder`; it mounts `~/.aws` and `~/.ssh` read-only.

## Architecture and how the pieces connect

- **Terraform → Ansible handoff**: `terraform/outputs.tf` exposes `master_public_ip` and
  `worker_public_ips`; `cluster.sh refresh_inventory` uses awk to rewrite the `master` line and
  rebuild `[workers]` as `worker1..workerN` (any `worker_count`). `workerK` is Terraform's
  `aws_instance.k8s_worker[K-1]`. `cmd_scale` relies on that mapping to drain the
  highest-numbered nodes, which `count` destroys first.
- **Scaling**: `./cluster.sh scale N` refuses unless the plan only creates or deletes
  `aws_instance.k8s_worker[*]`. It drains and deletes nodes before destroying them, and
  saves `worker_count` to tfvars. A plain `up`/`apply` with a lower count destroys
  workers without draining.
- **Inventory is checked in with RFC 5737 placeholder IPs** (`203.0.113.x`). Running the
  cluster overwrites them with real IPs; don't commit those — restore the placeholders.
- **Required Terraform inputs** `key_name` and `my_ip_cidr` come from the gitignored
  `terraform/terraform.tfvars` (copy from `.example`). The security group exposes 22, 6443,
  and NodePorts only to `my_ip_cidr`; node-to-node traffic is allowed via `self = true`.
- **Playbook sequence**:
  - `01_deps.yml` (all hosts): swap off, kernel modules/sysctl, containerd from Docker's
    repo, kube packages pinned to `k8s_minor` and held.
  - `02_k8s_init.yml`: `kubeadm init` on master, join command passed to workers through
    `hostvars` (never written to disk, `no_log`).
  - `03_cni.yml`: applies Calico and waits for nodes Ready.

  Idempotency relies on `creates:` guards (`admin.conf`, `kubelet.conf`); to retry a failed
  init/join, run `sudo kubeadm reset -f` on the node.
- **IAM**: `iam/k8s-handson-policy.json` limits the `k8s-handson-tf` user to EC2 in
  `ap-northeast-1` and denies IAM/billing. New AWS resource types won't work without
  updating this policy.

## Non-obvious constraints (don't "simplify" these away)

- The containerd.io package ships a stub `config.toml` with CRI disabled; `01_deps.yml`
  detects the stub and regenerates it, so a plain `creates:` check is wrong. `SystemdCgroup`
  must be `true` to match kubelet.
- `conntrack`, `socat`, `ethtool` are installed explicitly because kubeadm preflight needs
  them and the minimal AMI lacks them.
- Kernel modules are loaded via `command: modprobe`, not `community.general.modprobe`, so a
  bare ansible-core install works.
- `--apiserver-advertise-address` must be the private IP (`ansible_default_ipv4.address`);
  the public IP goes in `--apiserver-cert-extra-sans`.
- SG rules with `protocol = "-1"` must use `from_port`/`to_port` = 0.
- Pod CIDR `192.168.0.0/16` must not overlap the VPC CIDR `10.0.0.0/16`.
- `cluster.sh` must keep working with both BSD (macOS) and GNU tools. File rewrites use awk
  plus a temp file (`set_tfvar`, `refresh_inventory`), not `sed -i`.

## Documentation sync

The root `README.md` is the step-by-step guide and **embeds every source file verbatim**
(`main.tf`, `variables.tf`, playbooks, `cluster.sh`, etc.). When changing any of those files,
update the matching code block in `README.md`. `k8s-aws-terraform-ansible/README.md` holds
the measured infrastructure spec (versions, sizing, cost) from a real run.
