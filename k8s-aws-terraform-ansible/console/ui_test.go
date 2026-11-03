package main

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

func tempRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range map[string]string{
		tfvarsRel:    sampleTfvars,
		groupVarsRel: sampleGroupVars,
		inventoryRel: sampleInventory,
	} {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return root
}

func TestUISaveFlow(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	root := tempRepo(t)
	w := a.NewWindow("test")
	u := newUI(&Store{Root: root}, w)
	w.SetContent(u.content())
	w.Resize(fyne.NewSize(900, 700))
	u.reload()
	u.waitState()

	if u.keyName.Text != "my-key" || u.rootVol.Text != "30" || typeFromLabel(u.masterType.Selected) != "t3.medium" {
		t.Fatalf("form not loaded: key=%q vol=%q master=%q", u.keyName.Text, u.rootVol.Text, u.masterType.Selected)
	}
	if !strings.Contains(u.cost.Text, "$") {
		t.Errorf("cost not shown: %q", u.cost.Text)
	}

	// Unchanged save is a no-op.
	if d := u.save(); len(d) != 0 {
		t.Errorf("no-op save wrote: %v", d)
	}

	// Invalid input is rejected and nothing is written.
	u.rootVol.SetText("300")
	if d := u.save(); d != nil || u.fieldErr["RootVolumeSize"].Text == "" {
		t.Errorf("invalid volume accepted: %v", d)
	}

	// Valid edits across two files.
	u.rootVol.SetText("40")
	selectType(u.workerType, "t3.large")
	u.workerCount.SetSelected("3")
	u.calico.SetText("v3.30.0")
	diffs := u.save()
	if len(diffs) != 2 {
		t.Fatalf("expected 2 changed files, got %d: %v", len(diffs), diffs)
	}
	tf, _ := os.ReadFile(filepath.Join(root, tfvarsRel))
	gv, _ := os.ReadFile(filepath.Join(root, groupVarsRel))
	if !strings.Contains(string(tf), "root_volume_size = 40") || !strings.Contains(string(tf), `worker_instance_type = "t3.large"`) || !strings.Contains(string(tf), "worker_count = 3") || !strings.Contains(string(gv), `"v3.30.0"`) {
		t.Errorf("files not updated:\n%s\n%s", tf, gv)
	}

	if dir := os.Getenv("UI_SNAPSHOT_DIR"); dir != "" {
		u.reload()
		u.waitState()
		for o := w.Canvas().Overlays(); o.Top() != nil; {
			o.Remove(o.Top())
		}
		w.Resize(fyne.NewSize(900, 1250))
		f, err := os.Create(filepath.Join(dir, "console.png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		png.Encode(f, w.Canvas().Capture())
	}
}
