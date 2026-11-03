package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// InstanceType: kubeadm preflight needs >= 2 vCPU and ~1700 MiB RAM.
// Prices are approximate on-demand Linux $/hour in ap-northeast-1.
type InstanceType struct {
	Name    string
	VCPU    int
	MemGiB  float64
	USDHour float64
}

var instanceTypes = map[string]InstanceType{
	"t3.small":   {"t3.small", 2, 2, 0.0272},
	"t3.medium":  {"t3.medium", 2, 4, 0.0544},
	"t3.large":   {"t3.large", 2, 8, 0.1088},
	"t3.xlarge":  {"t3.xlarge", 4, 16, 0.2176},
	"t3a.small":  {"t3a.small", 2, 2, 0.0245},
	"t3a.medium": {"t3a.medium", 2, 4, 0.049},
	"t3a.large":  {"t3a.large", 2, 8, 0.0979},
	"t3a.xlarge": {"t3a.xlarge", 4, 16, 0.1958},
	"m5.large":   {"m5.large", 2, 8, 0.124},
	"m5.xlarge":  {"m5.xlarge", 4, 16, 0.248},
	"c5.large":   {"c5.large", 2, 4, 0.107},
	"c5.xlarge":  {"c5.xlarge", 4, 8, 0.214},
}

func sortedInstanceTypes() []InstanceType {
	out := make([]InstanceType, 0, len(instanceTypes))
	for _, t := range instanceTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].USDHour < out[j].USDHour })
	return out
}

const gp3USDPerGBMonth = 0.096

// estimateUSDPerDay is EC2 + gp3 for 1 master + s.WorkerCount workers.
func estimateUSDPerDay(s Settings) float64 {
	m, w := instanceTypes[s.MasterInstanceType], instanceTypes[s.WorkerInstanceType]
	ec2 := (m.USDHour + float64(s.WorkerCount)*w.USDHour) * 24
	ebs := float64((1+s.WorkerCount)*s.RootVolumeSize) * gp3USDPerGBMonth / 30
	return ec2 + ebs
}

var (
	k8sMinorRe = regexp.MustCompile(`^v1\.\d{2}$`)
	semverRe   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	keyNameRe  = regexp.MustCompile(`^[\x20-\x7E]{1,255}$`)
	userRe     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	pathRe     = regexp.MustCompile(`^[^\s"'\\]+$`)
)

// validate returns field-name → error message. Empty map means valid.
func validate(s Settings) map[string]string {
	errs := map[string]string{}
	if !keyNameRe.MatchString(s.KeyName) || strings.ContainsAny(s.KeyName, `"\`) {
		errs["KeyName"] = "required: name of an existing EC2 key pair"
	}
	if ip, n, err := net.ParseCIDR(s.MyIPCIDR); err != nil || ip.To4() == nil {
		errs["MyIPCIDR"] = "must be an IPv4 CIDR such as 203.0.113.10/32"
	} else if ones, _ := n.Mask.Size(); ones < 16 {
		errs["MyIPCIDR"] = "prefix shorter than /16 would expose SSH and the API server too widely"
	}
	if _, ok := instanceTypes[s.MasterInstanceType]; !ok {
		errs["MasterInstanceType"] = "choose a type from the list (>= 2 vCPU, >= 2 GiB)"
	}
	if _, ok := instanceTypes[s.WorkerInstanceType]; !ok {
		errs["WorkerInstanceType"] = "choose a type from the list (>= 2 vCPU, >= 2 GiB)"
	}
	if s.RootVolumeSize < 20 || s.RootVolumeSize > 200 {
		errs["RootVolumeSize"] = "must be 20-200 GB (8 GB fills up pulling images)"
	}
	if s.WorkerCount < 1 || s.WorkerCount > maxWorkers {
		errs["WorkerCount"] = fmt.Sprintf("must be 1-%d", maxWorkers)
	}
	if !k8sMinorRe.MatchString(s.K8sMinor) {
		errs["K8sMinor"] = "must look like v1.31"
	}
	if !semverRe.MatchString(s.CalicoVersion) {
		errs["CalicoVersion"] = "must be a full tag such as v3.29.1"
	}
	if !userRe.MatchString(s.AnsibleUser) {
		errs["AnsibleUser"] = "must be a valid Linux user name (ubuntu for the Ubuntu AMI)"
	}
	if !pathRe.MatchString(s.SSHPrivateKey) {
		errs["SSHPrivateKey"] = "must be a path without spaces or quotes"
	}
	return errs
}

// vCPU total above which we warn about the EC2 On-Demand Standard vCPU quota
// (new accounts can have a low limit).
const vcpuQuotaHint = 16

// warnings are non-blocking hints shown next to the form. runningWorkers is the
// worker count of the cluster in terraform state, or -1 if none is running.
func warnings(s Settings, detectedIP string, runningWorkers int) []string {
	var w []string
	if runningWorkers >= 0 && s.WorkerCount != runningWorkers {
		w = append(w, fmt.Sprintf("A cluster is running with %d workers. Saving only updates terraform.tfvars; apply it with ./cluster.sh scale %d (it drains removed nodes; ./cluster.sh up would not).", runningWorkers, s.WorkerCount))
	}
	m, mok := instanceTypes[s.MasterInstanceType]
	wt, wok := instanceTypes[s.WorkerInstanceType]
	if mok && wok {
		if total := m.VCPU + s.WorkerCount*wt.VCPU; total > vcpuQuotaHint {
			w = append(w, fmt.Sprintf("%d vCPUs in total; check your EC2 On-Demand Standard vCPU quota in %s.", total, pinnedRegion))
		}
	}
	if detectedIP != "" && s.MyIPCIDR != "" && s.MyIPCIDR != detectedIP+"/32" {
		w = append(w, fmt.Sprintf("my_ip_cidr (%s) does not match your current global IP (%s/32); SSH will be blocked.", s.MyIPCIDR, detectedIP))
	}
	if p := expandHome(s.SSHPrivateKey); p != "" {
		if _, err := os.Stat(p); err != nil {
			w = append(w, fmt.Sprintf("SSH key file %s does not exist on this machine.", s.SSHPrivateKey))
		}
	}
	if expandHome(s.SSHPrivateKey) != expandHome(clusterShKey) {
		w = append(w, fmt.Sprintf("cluster.sh uses KEY=%s for preflight and ./cluster.sh ssh; it does not read the inventory.", clusterShKey))
	}
	if s.K8sMinor != defaults.K8sMinor {
		w = append(w, "Changing k8s_minor only affects new clusters: packages are held and the apt keyring has a creates: guard.")
	}
	if t, ok := instanceTypes[s.MasterInstanceType]; ok && t.MemGiB < 4 {
		w = append(w, "A 2 GiB master passes kubeadm preflight but is tight for the control plane.")
	}
	return w
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}
