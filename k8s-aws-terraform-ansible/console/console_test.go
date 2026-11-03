package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleTfvars = `# header comment
key_name = "my-key"
my_ip_cidr = "198.51.100.7/32" # Update whenever your global IP changes

# region               = "ap-northeast-1"
# root_volume_size     = 30
`

const sampleInventory = `[masters]
master ansible_host=203.0.113.10

[workers]
worker1 ansible_host=203.0.113.11
worker2 ansible_host=203.0.113.12

[all:vars]
ansible_user=ubuntu
ansible_ssh_private_key_file=~/.ssh/k8s-handson.pem
ansible_python_interpreter=/usr/bin/python3
`

const sampleGroupVars = `# comment
k8s_minor: "v1.31"        # minor
calico_version: "v3.29.1" # tag
`

func loadAll(t *testing.T) Settings {
	t.Helper()
	s := defaults
	if err := applyTfvars([]byte(sampleTfvars), &s); err != nil {
		t.Fatal(err)
	}
	applyGroupVars(sampleGroupVars, &s)
	applyInventory(sampleInventory, &s)
	return s
}

func TestRoundTripUnchanged(t *testing.T) {
	s := loadAll(t)
	if s.KeyName != "my-key" || s.MyIPCIDR != "198.51.100.7/32" || s.RootVolumeSize != 30 {
		t.Fatalf("unexpected load: %+v", s)
	}
	tf, err := renderTfvars([]byte(sampleTfvars), s)
	if err != nil {
		t.Fatal(err)
	}
	if string(tf) != sampleTfvars {
		t.Errorf("tfvars changed:\n%s", unifiedDiff("tfvars", sampleTfvars, string(tf)))
	}
	if gv := renderGroupVars(sampleGroupVars, s); gv != sampleGroupVars {
		t.Errorf("group_vars changed:\n%s", gv)
	}
	inv, err := renderInventory(sampleInventory, s)
	if err != nil || inv != sampleInventory {
		t.Errorf("inventory changed (%v):\n%s", err, inv)
	}
}

func TestTfvarsEditKeepsComments(t *testing.T) {
	s := loadAll(t)
	s.MyIPCIDR = "198.51.100.8/32"
	s.RootVolumeSize = 40
	out, err := renderTfvars([]byte(sampleTfvars), s)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	for _, want := range []string{"# header comment", `"198.51.100.8/32"`, "root_volume_size = 40", "# region"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "master_instance_type =") {
		t.Errorf("default-valued optional key was written:\n%s", got)
	}
	back := defaults
	if err := applyTfvars(out, &back); err != nil || back.RootVolumeSize != 40 || back.MyIPCIDR != s.MyIPCIDR {
		t.Errorf("re-parse: %v %+v", err, back)
	}
}

func TestGroupVarsEdit(t *testing.T) {
	s := loadAll(t)
	s.CalicoVersion = "v3.30.0"
	got := renderGroupVars(sampleGroupVars, s)
	want := strings.Replace(sampleGroupVars, `"v3.29.1"`, `"v3.30.0"`, 1)
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// Missing file: keys are appended.
	if out := renderGroupVars("", s); !strings.Contains(out, `k8s_minor: "v1.31"`) || !strings.Contains(out, `calico_version: "v3.30.0"`) {
		t.Errorf("empty render: %q", out)
	}
}

func TestInventoryEditLeavesHosts(t *testing.T) {
	s := loadAll(t)
	s.AnsibleUser = "admin"
	s.SSHPrivateKey = "~/.ssh/other.pem"
	got, err := renderInventory(sampleInventory, s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "ansible_user=admin\n") || !strings.Contains(got, "ansible_ssh_private_key_file=~/.ssh/other.pem\n") {
		t.Errorf("not edited:\n%s", got)
	}
	if h := inventoryHosts(got); len(h) != 3 || h[0] != [2]string{"master", "203.0.113.10"} {
		t.Errorf("hosts changed: %v", h)
	}
	if _, err := renderInventory("[masters]\nmaster ansible_host=1.2.3.4\n", s); err == nil {
		t.Error("expected error when [all:vars] key is missing")
	}
}

func TestValidate(t *testing.T) {
	s := loadAll(t)
	if e := validate(s); len(e) != 0 {
		t.Fatalf("valid settings rejected: %v", e)
	}
	bad := s
	bad.KeyName = ""
	bad.MyIPCIDR = "0.0.0.0/0"
	bad.MasterInstanceType = "t3.micro"
	bad.RootVolumeSize = 8
	bad.K8sMinor = "1.31"
	bad.CalicoVersion = "v3.29"
	bad.AnsibleUser = "Root User"
	bad.SSHPrivateKey = "a b"
	e := validate(bad)
	for _, k := range []string{"KeyName", "MyIPCIDR", "MasterInstanceType", "RootVolumeSize", "K8sMinor", "CalicoVersion", "AnsibleUser", "SSHPrivateKey"} {
		if e[k] == "" {
			t.Errorf("%s not rejected", k)
		}
	}
	if e["WorkerInstanceType"] != "" {
		t.Error("worker type wrongly rejected")
	}
}

func TestStoreSaveFromExample(t *testing.T) {
	root := t.TempDir()
	for rel, body := range map[string]string{
		tfexampleRel: sampleTfvars,
		groupVarsRel: sampleGroupVars,
		inventoryRel: sampleInventory,
	} {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	st := &Store{Root: root}
	s, _, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	diffs, err := st.Save(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || !strings.Contains(diffs[0], tfvarsRel) {
		t.Fatalf("expected only tfvars to be created, got %v", diffs)
	}
	fi, err := os.Stat(filepath.Join(root, tfvarsRel))
	if err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("tfvars mode: %v %v", fi, err)
	}
	if diffs, _ := st.Save(s); len(diffs) != 0 {
		t.Errorf("second save should be a no-op, got %v", diffs)
	}
}

func TestDiffOrder(t *testing.T) {
	got := unifiedDiff("f", "a\nb\nc", "a\nB\nc")
	if want := "--- f\n+++ f\n-b\n+B\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestFindRoot(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "terraform"), 0o755)
	os.WriteFile(filepath.Join(root, "terraform", "variables.tf"), nil, 0o644)
	if got := findRoot(root, ""); got != root {
		t.Errorf("flag root: got %q", got)
	}
	if got := findRoot("", root); got != root {
		t.Errorf("saved root: got %q", got)
	}
	if got := findRoot(t.TempDir(), ""); got != "" && got == root {
		t.Errorf("unexpected match %q", got)
	}
}

func TestWorkerCount(t *testing.T) {
	s := loadAll(t)
	if s.WorkerCount != 2 {
		t.Fatalf("default worker count: %d", s.WorkerCount)
	}
	s.WorkerCount = 3
	out, err := renderTfvars([]byte(sampleTfvars), s)
	if err != nil || !strings.Contains(string(out), "worker_count = 3\n") {
		t.Fatalf("not written (%v):\n%s", err, out)
	}
	back := defaults
	if applyTfvars(out, &back); back.WorkerCount != 3 {
		t.Errorf("re-parse: %d", back.WorkerCount)
	}
	// Back to the default: the now-present key is updated, not left at 3.
	s.WorkerCount = 2
	out2, _ := renderTfvars(out, s)
	if !strings.Contains(string(out2), "worker_count = 2\n") {
		t.Errorf("not reset:\n%s", out2)
	}
	for _, n := range []int{0, maxWorkers + 1} {
		s.WorkerCount = n
		if validate(s)["WorkerCount"] == "" {
			t.Errorf("worker_count %d accepted", n)
		}
	}
	s.WorkerCount = 3
	if c3, c2 := estimateUSDPerDay(s), estimateUSDPerDay(loadAll(t)); c3 <= c2 {
		t.Errorf("cost did not grow with workers: %v <= %v", c3, c2)
	}
	ws := warnings(s, "", 2)
	if len(ws) == 0 || !strings.Contains(ws[0], "./cluster.sh scale 3") {
		t.Errorf("missing running-cluster warning: %v", ws)
	}
	s.WorkerCount = 10
	s.WorkerInstanceType = "t3.xlarge"
	if ws := strings.Join(warnings(s, "", -1), "\n"); !strings.Contains(ws, "vCPU quota") {
		t.Errorf("missing quota warning: %s", ws)
	}
}
