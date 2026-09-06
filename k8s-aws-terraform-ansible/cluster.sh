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
