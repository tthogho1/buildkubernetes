package main

import (
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
)

func running2() clusterState {
	return stateFromOutputs(
		map[string]string{
			"master_public_ip":  "198.51.100.1",
			"worker_public_ips": `["198.51.100.11","198.51.100.12"]`,
		},
		[][2]string{{"master", "198.51.100.1"}, {"worker1", "198.51.100.11"}, {"worker2", "203.0.113.12"}},
	)
}

func TestPlanNodes(t *testing.T) {
	if _, s := planNodes(clusterState{}, 2); !strings.Contains(s, "Reading") {
		t.Errorf("unknown state: %q", s)
	}

	none := stateFromOutputs(map[string]string{}, nil)
	rows, s := planNodes(none, 3)
	if len(rows) != 4 || !strings.Contains(s, "No cluster running") || rows[3].Name != "worker3" {
		t.Errorf("no cluster: %v %q", rows, s)
	}

	st := running2()
	rows, s = planNodes(st, 2)
	if len(rows) != 3 || !strings.Contains(s, "matches") {
		t.Errorf("same count: %v %q", rows, s)
	}
	if !strings.Contains(rows[2].Status, "stale") || strings.Contains(rows[1].Status, "stale") {
		t.Errorf("stale detection: %+v", rows)
	}

	rows, s = planNodes(st, 4)
	if len(rows) != 5 || !strings.Contains(rows[3].Status, "will be added") || !strings.Contains(s, "+2") {
		t.Errorf("scale up: %v %q", rows, s)
	}

	rows, s = planNodes(st, 1)
	if len(rows) != 3 || !strings.Contains(rows[2].Status, "WILL BE REMOVED") || rows[2].Name != "worker2" ||
		rows[1].Status != "running" || !strings.Contains(s, "−1") {
		t.Errorf("scale down: %v %q", rows, s)
	}
}

func TestNodesPanelFollowsForm(t *testing.T) {
	a := test.NewApp()
	defer a.Quit()
	w := a.NewWindow("test")
	u := newUI(&Store{Root: tempRepo(t)}, w)
	w.SetContent(u.content())
	u.reload()
	u.waitState() // so reload's background read can't overwrite the fake state

	u.mu.Lock()
	u.cluster = running2()
	u.runningWks = 2
	u.mu.Unlock()
	u.workerCount.SetSelected("3") // OnChanged -> refreshDerived -> renderNodes
	if !strings.Contains(u.nodes.Text, "worker3") || !strings.Contains(u.nodeSummary.Text, "+1") {
		t.Fatalf("panel did not follow form:\n%s\n%s", u.nodes.Text, u.nodeSummary.Text)
	}
	u.workerCount.SetSelected("1")
	if !strings.Contains(u.nodes.Text, "WILL BE REMOVED") {
		t.Fatalf("scale-down not shown:\n%s", u.nodes.Text)
	}

	if dir := os.Getenv("UI_SNAPSHOT_DIR"); dir != "" {
		u.workerCount.SetSelected("3")
		tabs := w.Content().(*fyne.Container).Objects[0].(*container.AppTabs)
		tabs.SelectIndex(1)
		w.Resize(fyne.NewSize(900, 700))
		f, _ := os.Create(filepath.Join(dir, "nodes.png"))
		defer f.Close()
		png.Encode(f, w.Canvas().Capture())
	}
}
