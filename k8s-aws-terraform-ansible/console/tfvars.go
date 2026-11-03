package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// readTfvarsSource returns the tfvars content, falling back to the .example
// template when terraform.tfvars does not exist yet.
func readTfvarsSource(path, examplePath string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		return b, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return os.ReadFile(examplePath)
}

// parseTfvars evaluates the literal attributes of a tfvars file.
func parseTfvars(src []byte) (map[string]cty.Value, error) {
	f, diags := hclsyntax.ParseConfig(src, "terraform.tfvars", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse tfvars: %s", diags.Error())
	}
	out := map[string]cty.Value{}
	for name, attr := range f.Body.(*hclsyntax.Body).Attributes {
		v, d := attr.Expr.Value(nil)
		if d.HasErrors() {
			return nil, fmt.Errorf("tfvars %s: %s", name, d.Error())
		}
		out[name] = v
	}
	return out, nil
}

// applyTfvars loads the Terraform fields of s from src (defaults for missing keys).
func applyTfvars(src []byte, s *Settings) error {
	vals, err := parseTfvars(src)
	if err != nil {
		return err
	}
	str := func(k string, dst *string) {
		if v, ok := vals[k]; ok && v.Type() == cty.String && v.IsKnown() && !v.IsNull() {
			*dst = v.AsString()
		}
	}
	str("key_name", &s.KeyName)
	str("my_ip_cidr", &s.MyIPCIDR)
	str("master_instance_type", &s.MasterInstanceType)
	str("worker_instance_type", &s.WorkerInstanceType)
	num := func(k string, dst *int) {
		if v, ok := vals[k]; ok && v.Type() == cty.Number && v.IsKnown() && !v.IsNull() {
			n, _ := v.AsBigFloat().Int64()
			*dst = int(n)
		}
	}
	num("root_volume_size", &s.RootVolumeSize)
	num("worker_count", &s.WorkerCount)
	return nil
}

// renderTfvars rewrites src with s's values. Existing values are replaced in
// place at their exact byte range, so comments, alignment and every other line
// stay byte-identical. Optional keys are written only when already present or
// when they differ from the variables.tf default.
func renderTfvars(src []byte, s Settings) ([]byte, error) {
	f, diags := hclsyntax.ParseConfig(src, "terraform.tfvars", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse tfvars: %s", diags.Error())
	}
	attrs := f.Body.(*hclsyntax.Body).Attributes
	type edit struct {
		start, end int
		text       string
	}
	var edits []edit
	var appends []string
	set := func(name string, v cty.Value, required, isDefault bool) {
		lit := string(hclwrite.TokensForValue(v).Bytes())
		if a, ok := attrs[name]; ok {
			if old, d := a.Expr.Value(nil); !d.HasErrors() && old.Type().Equals(v.Type()) && old.Equals(v).True() {
				return
			}
			r := a.Expr.Range()
			edits = append(edits, edit{r.Start.Byte, r.End.Byte, lit})
			return
		}
		if required || !isDefault {
			appends = append(appends, name+" = "+lit+"\n")
		}
	}
	set("key_name", cty.StringVal(s.KeyName), true, false)
	set("my_ip_cidr", cty.StringVal(s.MyIPCIDR), true, false)
	set("master_instance_type", cty.StringVal(s.MasterInstanceType), false, s.MasterInstanceType == defaults.MasterInstanceType)
	set("worker_instance_type", cty.StringVal(s.WorkerInstanceType), false, s.WorkerInstanceType == defaults.WorkerInstanceType)
	set("root_volume_size", cty.NumberIntVal(int64(s.RootVolumeSize)), false, s.RootVolumeSize == defaults.RootVolumeSize)
	set("worker_count", cty.NumberIntVal(int64(s.WorkerCount)), false, s.WorkerCount == defaults.WorkerCount)

	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	out := append([]byte(nil), src...)
	for _, e := range edits {
		out = append(out[:e.start], append([]byte(e.text), out[e.end:]...)...)
	}
	if len(appends) > 0 {
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		for _, a := range appends {
			out = append(out, a...)
		}
	}
	return out, nil
}
