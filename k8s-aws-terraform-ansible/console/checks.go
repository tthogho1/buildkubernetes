package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// run executes a fixed argv (no shell) in dir with AWS_PROFILE set and returns
// combined output plus the exit status line.
func run(ctx context.Context, dir string, name string, args ...string) (string, bool) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AWS_PROFILE="+awsProfile, "TF_IN_AUTOMATION=1", "NO_COLOR=1", "ANSIBLE_NOCOLOR=1")
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	out := "$ " + name + " " + strings.Join(args, " ") + "\n" + buf.String()
	if err != nil {
		return out + "\n[exit: " + err.Error() + "]\n", false
	}
	return out + "\n[ok]\n", true
}

// Check is one validation button. None of them create AWS resources.
type Check struct {
	ID, Label, Note string
	Run             func(ctx context.Context, root string) (string, bool)
}

func ensureTfInit(ctx context.Context, root string) (string, bool) {
	if _, err := os.Stat(filepath.Join(root, "terraform", ".terraform")); err == nil {
		return "", true
	}
	return run(ctx, root, "terraform", "-chdir=terraform", "init", "-input=false")
}

var checks = []Check{
	{"tf-validate", "terraform validate", "offline", func(ctx context.Context, root string) (string, bool) {
		pre, ok := ensureTfInit(ctx, root)
		if !ok {
			return pre, false
		}
		out, ok := run(ctx, root, "terraform", "-chdir=terraform", "validate", "-no-color")
		return pre + out, ok
	}},
	{"tf-plan", "terraform plan", "read-only AWS API calls; creates nothing", func(ctx context.Context, root string) (string, bool) {
		pre, ok := ensureTfInit(ctx, root)
		if !ok {
			return pre, false
		}
		out, ok := run(ctx, root, "terraform", "-chdir=terraform", "plan", "-input=false", "-no-color")
		return pre + out, ok
	}},
	{"ansible-syntax", "ansible-playbook --syntax-check", "offline", func(ctx context.Context, root string) (string, bool) {
		dir := filepath.Join(root, "ansible")
		var all strings.Builder
		okAll := true
		for _, p := range []string{"playbooks/01_deps.yml", "playbooks/02_k8s_init.yml", "playbooks/03_cni.yml"} {
			out, ok := run(ctx, dir, "ansible-playbook", "--syntax-check", p)
			all.WriteString(out + "\n")
			okAll = okAll && ok
		}
		out, ok := run(ctx, dir, "ansible-inventory", "--host", "master")
		all.WriteString(out)
		return all.String(), okAll && ok
	}},
}

// detectMyIP mirrors cluster.sh preflight (curl ifconfig.me).
func detectMyIP(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://ifconfig.me/ip", nil)
	req.Header.Set("User-Agent", "curl/8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	ip := strings.TrimSpace(string(b))
	if net.ParseIP(ip).To4() == nil {
		return ""
	}
	return ip
}

// listKeyPairs returns EC2 key pair names, or nil if the AWS CLI/creds are unavailable.
func listKeyPairs(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "aws", "ec2", "describe-key-pairs", "--region", pinnedRegion,
		"--query", "KeyPairs[].KeyName", "--output", "json")
	cmd.Env = append(os.Environ(), "AWS_PROFILE="+awsProfile)
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var names []string
	_ = json.Unmarshal(b, &names)
	return names
}

// terraformOutputs reads outputs from local state (no AWS calls).
func terraformOutputs(ctx context.Context, root string) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "terraform", "-chdir=terraform", "output", "-json")
	cmd.Dir = root
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var raw map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = strings.Trim(string(v.Value), `"`)
	}
	return out
}
