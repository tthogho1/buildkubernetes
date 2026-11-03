package main

import (
	"fmt"
	"regexp"
	"strings"
)

// Both Ansible data files are edited line-by-line with anchored regexes so that
// everything else in them (comments, host IP lines) is left byte-identical.

func yamlKeyRe(key string) *regexp.Regexp {
	// key: "value"   # comment     (quotes optional)
	return regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(key) + `:[ \t]*)("?)([^"\s#]*)("?)`)
}

func iniKeyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(` + regexp.QuoteMeta(key) + `=)(\S*)`)
}

func getYAML(src, key string) (string, bool) {
	m := yamlKeyRe(key).FindStringSubmatch(src)
	if m == nil {
		return "", false
	}
	return m[3], true
}

func setYAML(src, key, val string) string {
	re := yamlKeyRe(key)
	if !re.MatchString(src) {
		if src != "" && !strings.HasSuffix(src, "\n") {
			src += "\n"
		}
		return src + fmt.Sprintf("%s: %q\n", key, val)
	}
	return re.ReplaceAllStringFunc(src, func(line string) string {
		m := re.FindStringSubmatch(line)
		return m[1] + `"` + val + `"`
	})
}

func getINI(src, key string) (string, bool) {
	m := iniKeyRe(key).FindStringSubmatch(src)
	if m == nil {
		return "", false
	}
	return m[2], true
}

// setINI replaces key=value in the [all:vars] section only; the key must exist.
func setINI(src, key, val string) (string, error) {
	lines := strings.SplitAfter(src, "\n")
	re := iniKeyRe(key)
	section, done := "", false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = t
			continue
		}
		if section == "[all:vars]" && re.MatchString(l) {
			lines[i] = re.ReplaceAllLiteralString(l, key+"="+val)
			done = true
		}
	}
	if !done {
		return "", fmt.Errorf("inventory.ini: %s not found under [all:vars]", key)
	}
	return strings.Join(lines, ""), nil
}

func applyGroupVars(src string, s *Settings) {
	if v, ok := getYAML(src, "k8s_minor"); ok {
		s.K8sMinor = v
	}
	if v, ok := getYAML(src, "calico_version"); ok {
		s.CalicoVersion = v
	}
}

func renderGroupVars(src string, s Settings) string {
	set := func(src, key, val string) string {
		if cur, ok := getYAML(src, key); ok && cur == val {
			return src
		}
		return setYAML(src, key, val)
	}
	src = set(src, "k8s_minor", s.K8sMinor)
	return set(src, "calico_version", s.CalicoVersion)
}

func applyInventory(src string, s *Settings) {
	if v, ok := getINI(src, "ansible_user"); ok {
		s.AnsibleUser = v
	}
	if v, ok := getINI(src, "ansible_ssh_private_key_file"); ok {
		s.SSHPrivateKey = v
	}
}

func renderInventory(src string, s Settings) (string, error) {
	var err error
	for _, kv := range [][2]string{
		{"ansible_user", s.AnsibleUser},
		{"ansible_ssh_private_key_file", s.SSHPrivateKey},
	} {
		if cur, ok := getINI(src, kv[0]); ok && cur == kv[1] {
			continue
		}
		if src, err = setINI(src, kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	return src, nil
}

// Host lines (master/worker ansible_host=...) for the read-only panel.
var hostLineRe = regexp.MustCompile(`(?m)^(\S+)\s+ansible_host=(\S+)`)

func inventoryHosts(src string) [][2]string {
	var out [][2]string
	for _, m := range hostLineRe.FindAllStringSubmatch(src, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}
