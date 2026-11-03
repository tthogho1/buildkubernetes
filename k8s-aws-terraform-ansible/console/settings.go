package main

// Settings is every value the console can edit. Each field maps to exactly one
// key in one data file; the console never touches .tf or playbook source.
type Settings struct {
	// terraform/terraform.tfvars
	KeyName            string
	MyIPCIDR           string
	MasterInstanceType string
	WorkerInstanceType string
	RootVolumeSize     int
	WorkerCount        int

	// ansible/group_vars/all.yml
	K8sMinor      string
	CalicoVersion string

	// ansible/inventory.ini [all:vars]
	AnsibleUser   string
	SSHPrivateKey string
}

// Defaults mirror terraform/variables.tf and ansible/group_vars/all.yml.
var defaults = Settings{
	MasterInstanceType: "t3.medium",
	WorkerInstanceType: "t3.medium",
	RootVolumeSize:     30,
	WorkerCount:        2,
	K8sMinor:           "v1.31",
	CalicoVersion:      "v3.29.1",
	AnsibleUser:        "ubuntu",
	SSHPrivateKey:      "~/.ssh/k8s-handson.pem",
}

// Values that are pinned elsewhere in the repo and shown read-only.
const (
	pinnedRegion  = "ap-northeast-1" // iam/k8s-handson-policy.json, cluster.sh REGION
	maxWorkers    = 10               // same limit as ./cluster.sh scale
	pinnedPodCIDR = "192.168.0.0/16" // 02_k8s_init.yml, Calico default
	pinnedVPCCIDR = "10.0.0.0/16"    // main.tf
	pinnedAMI     = "Ubuntu 22.04 LTS (Canonical)"
	clusterShKey  = "~/.ssh/k8s-handson.pem" // KEY in cluster.sh
	awsProfile    = "k8s-handson"
)
