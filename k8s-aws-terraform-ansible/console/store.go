package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Store reads and writes the three data files under the repo root.
type Store struct {
	Root string
	mu   sync.Mutex
}

func (st *Store) p(rel string) string { return filepath.Join(st.Root, rel) }

const (
	tfvarsRel    = "terraform/terraform.tfvars"
	tfexampleRel = "terraform/terraform.tfvars.example"
	groupVarsRel = "ansible/group_vars/all.yml"
	inventoryRel = "ansible/inventory.ini"
)

type sources struct{ tfvars, groupVars, inventory string }

func (st *Store) readSources() (sources, error) {
	tf, err := readTfvarsSource(st.p(tfvarsRel), st.p(tfexampleRel))
	if err != nil {
		return sources{}, err
	}
	gv, err := os.ReadFile(st.p(groupVarsRel))
	if err != nil && !os.IsNotExist(err) {
		return sources{}, err
	}
	inv, err := os.ReadFile(st.p(inventoryRel))
	if err != nil {
		return sources{}, err
	}
	return sources{string(tf), string(gv), string(inv)}, nil
}

func (st *Store) Load() (Settings, sources, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	src, err := st.readSources()
	if err != nil {
		return Settings{}, src, err
	}
	s := defaults
	if err := applyTfvars([]byte(src.tfvars), &s); err != nil {
		return s, src, err
	}
	applyGroupVars(src.groupVars, &s)
	applyInventory(src.inventory, &s)
	return s, src, nil
}

// Save writes only the files whose content changed and returns their diffs.
// The caller must have validated s.
func (st *Store) Save(s Settings) ([]string, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	src, err := st.readSources()
	if err != nil {
		return nil, err
	}
	tfOut, err := renderTfvars([]byte(src.tfvars), s)
	if err != nil {
		return nil, err
	}
	invOut, err := renderInventory(src.inventory, s)
	if err != nil {
		return nil, err
	}
	gvOut := renderGroupVars(src.groupVars, s)

	// tfvars may be new (rendered from .example); compare against what is on disk.
	tfDisk, _ := os.ReadFile(st.p(tfvarsRel))
	var diffs []string
	for _, f := range []struct {
		rel, old, new string
		mode          os.FileMode
	}{
		{tfvarsRel, string(tfDisk), string(tfOut), 0o600},
		{groupVarsRel, src.groupVars, gvOut, 0o644},
		{inventoryRel, src.inventory, invOut, 0o644},
	} {
		if f.old == f.new {
			continue
		}
		if err := writeAtomic(st.p(f.rel), []byte(f.new), f.mode); err != nil {
			return diffs, fmt.Errorf("write %s: %w", f.rel, err)
		}
		diffs = append(diffs, unifiedDiff(f.rel, f.old, f.new))
	}
	return diffs, nil
}

// writeAtomic writes via a temp file in the same directory, keeping an existing file's mode.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (st *Store) tfvarsExists() bool {
	_, err := os.Stat(st.p(tfvarsRel))
	return err == nil
}
