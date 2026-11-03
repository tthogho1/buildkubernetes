#!/usr/bin/env bash
#
# Single entry point for the k8s hands-on cluster.
#
#   ./cluster.sh up        Create EC2 + build the Kubernetes cluster (~7 min)
#   ./cluster.sh down      Destroy everything
#   ./cluster.sh status    Show cluster state
#   ./cluster.sh ssh       SSH into the master node
#   ./cluster.sh inventory Refresh ansible/inventory.ini from terraform output
#   ./cluster.sh scale N   Change the number of workers (drains removed nodes first)
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
  local m workers tmp
  m=$($TF output -raw master_public_ip)
  workers=$($TF output -json worker_public_ips \
        | python3 -c 'import json,sys; print(" ".join(json.load(sys.stdin)))')
  [ -n "$m" ] && [ -n "$workers" ] || die "could not read terraform outputs"

  # Rewrite the master line and rebuild [workers] as worker1..workerN.
  # awk (not sed -i) so it behaves the same with macOS (BSD) and Linux (GNU)
  # tools. Every other line, including [all:vars], is kept as-is.
  tmp=$(mktemp "ansible/inventory.ini.XXXXXX")
  awk -v m="$m" -v ws="$workers" '
    /^\[/ { in_workers = ($0 == "[workers]") }
    /^master ansible_host=/ { print "master ansible_host=" m; next }
    in_workers && /^worker[0-9]+ ansible_host=/ { next }
    { print }
    $0 == "[workers]" {
      n = split(ws, w, " ")
      for (i = 1; i <= n; i++) print "worker" i " ansible_host=" w[i]
    }
  ' ansible/inventory.ini > "$tmp"
  cat "$tmp" > ansible/inventory.ini   # keep the original file's mode
  rm -f "$tmp"
  grn "inventory updated: master=$m workers=$workers"
}

# Set (or append) `key = value` in terraform.tfvars; portable like refresh_inventory.
set_tfvar() {
  local k="$1" v="$2" f="terraform/terraform.tfvars" tmp
  [ -f "$f" ] || die "$f not found (cp terraform/terraform.tfvars.example $f)"
  tmp=$(mktemp "$f.XXXXXX")
  awk -v k="$k" -v v="$v" '
    $0 ~ "^" k "[ \t]*=" { print k " = " v; done = 1; next }
    { print }
    END { if (!done) print k " = " v }
  ' "$f" > "$tmp"
  cat "$tmp" > "$f"
  rm -f "$tmp"
}

worker_count_now() {
  $TF output -json worker_public_ips 2>/dev/null \
    | python3 -c 'import json,sys; print(len(json.load(sys.stdin)))'
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
    local n
    n=$(sed -n 's/^worker_count *= *\([0-9]*\).*/\1/p' terraform/terraform.tfvars 2>/dev/null)
    bold "This creates 1 master + ${n:-2} workers in $REGION (~\$1.5/day per t3.medium node)."
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

cmd_scale() {
  local want="${1:-}" yes="${2:-}" have i node
  [[ "$want" =~ ^[0-9]+$ ]] && [ "$want" -ge 1 ] && [ "$want" -le 10 ] \
    || die "usage: ./cluster.sh scale N [-y]   (1 <= N <= 10)"
  preflight

  if [ "$($TF state list 2>/dev/null | wc -l | tr -d ' ')" = "0" ]; then
    set_tfvar worker_count "$want"
    grn "No cluster running. Saved worker_count = $want; run ./cluster.sh up"
    return 0
  fi
  have=$(worker_count_now)
  if [ "$have" = "$want" ]; then
    echo "Already $have workers; nothing to do."
    set_tfvar worker_count "$want"
    return 0
  fi

  # Plan first and refuse anything beyond adding/removing workers, e.g. a master
  # replacement because a newer Ubuntu AMI was published or other edited settings.
  bold "[1/4] terraform plan (worker_count $have -> $want)"
  $TF init -input=false >/dev/null
  $TF plan -input=false -out=tfplan -var "worker_count=$want" >/dev/null
  if ! $TF show -json tfplan | python3 -c '
import json, sys
bad = []
for rc in json.load(sys.stdin).get("resource_changes", []):
    a = rc["change"]["actions"]
    if a in (["no-op"], ["read"]):
        continue
    if rc["address"].startswith("aws_instance.k8s_worker[") and a in (["create"], ["delete"]):
        continue
    bad.append(rc["address"] + " (" + "/".join(a) + ")")
if bad:
    print("Plan would also change:\n  " + "\n  ".join(bad))
    sys.exit(1)
'; then
    rm -f terraform/tfplan
    die "refusing to scale; run 'terraform -chdir=terraform plan' to see why"
  fi

  if [ "$yes" != "-y" ]; then
    if [ "$want" -lt "$have" ]; then
      bold "This drains and DESTROYS worker$((want + 1))..worker$have."
    else
      bold "This creates $((want - have)) more worker(s) (each bills while running)."
    fi
    read -r -p "Continue? [y/N] " a
    [[ "$a" =~ ^[Yy]$ ]] || { rm -f terraform/tfplan; echo "aborted"; exit 0; }
  fi

  # Terraform's count removes the highest indexes, i.e. inventory worker(want+1)..worker(have).
  if [ "$want" -lt "$have" ]; then
    bold "[2/4] drain and delete nodes"
    for i in $(seq $((want + 1)) "$have"); do
      node=$(cd ansible && ansible "worker$i" -m command -a hostname -o 2>/dev/null \
             | sed -n 's/.*(stdout) //p')
      [ -n "$node" ] || die "cannot reach worker$i to read its node name; fix or remove it manually"
      (cd ansible && ansible master -b -m command -a \
        "kubectl --kubeconfig /etc/kubernetes/admin.conf drain $node --ignore-daemonsets --delete-emptydir-data --timeout=300s") \
        || die "drain of $node failed; nothing destroyed"
      (cd ansible && ansible master -b -m command -a \
        "kubectl --kubeconfig /etc/kubernetes/admin.conf delete node $node")
      grn "removed node $node (worker$i)"
    done
  else
    bold "[2/4] (scaling up: nothing to drain)"
  fi

  bold "[3/4] terraform apply"
  set_tfvar worker_count "$want"   # keep later `up` runs from reverting the change
  $TF apply -input=false tfplan
  rm -f terraform/tfplan
  refresh_inventory

  if [ "$want" -gt "$have" ]; then
    local new
    new=$(seq -f 'worker%g' $((have + 1)) "$want" | paste -sd: -)
    wait_for_ssh
    bold "[4/4] install and join $new"
    (cd ansible && ansible-playbook playbooks/01_deps.yml --limit "$new")
    (cd ansible && ansible-playbook playbooks/02_k8s_init.yml --limit "masters:$new")
    (cd ansible && ansible-playbook playbooks/03_cni.yml)
  else
    bold "[4/4] done"
  fi
  cmd_status
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
  scale)     shift; cmd_scale "${1:-}" "${2:-}" ;;
  *) sed -n '3,13p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
