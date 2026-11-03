// Command console is a desktop (Fyne) UI for editing the hands-on cluster's
// Terraform and Ansible settings. It edits data files only:
//
//	terraform/terraform.tfvars, ansible/group_vars/all.yml, ansible/inventory.ini [all:vars]
//
// and can run offline/read-only checks. It never runs apply, destroy or cluster.sh.
package main

import (
	"errors"
	"flag"
	"os"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// version is set at build time: -ldflags "-X main.version=v1.2.3".
var version = "dev"

const prefRoot = "repoRoot"

// isRepoRoot reports whether dir looks like k8s-aws-terraform-ansible/.
func isRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "terraform", "variables.tf"))
	return err == nil
}

// findRoot tries, in order: -root flag, the last folder used, the current
// directory and its parent (running from console/), and the folder next to
// the executable. Returns "" if none match.
func findRoot(flagRoot, saved string) string {
	var cands []string
	if flagRoot != "" {
		cands = append(cands, flagRoot)
	}
	if saved != "" {
		cands = append(cands, saved)
	}
	if wd, err := os.Getwd(); err == nil {
		cands = append(cands, wd, filepath.Dir(wd), filepath.Join(wd, "k8s-aws-terraform-ansible"))
	}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands, d, filepath.Dir(d))
	}
	for _, c := range cands {
		if abs, err := filepath.Abs(c); err == nil && isRepoRoot(abs) {
			return abs
		}
	}
	return ""
}

func main() {
	rootFlag := flag.String("root", "", "path to k8s-aws-terraform-ansible/ (default: auto-detect, else ask)")
	flag.Parse()

	a := app.NewWithID("io.github.tthogho1.k8s-console")
	w := a.NewWindow("k8s hands-on console " + version)
	w.Resize(fyne.NewSize(900, 700))

	open := func(root string) {
		a.Preferences().SetString(prefRoot, root)
		w.SetTitle("k8s hands-on console " + version + " — " + root)
		ui := newUI(&Store{Root: root}, w)
		w.SetContent(ui.content())
		ui.reload()
		go ui.lookupsAsync()
	}

	if root := findRoot(*rootFlag, a.Preferences().String(prefRoot)); root != "" {
		open(root)
	} else {
		var pick func()
		pick = func() {
			d := dialog.NewFolderOpen(func(u fyne.ListableURI, err error) {
				if err != nil || u == nil {
					return
				}
				if !isRepoRoot(u.Path()) {
					dialog.ShowError(errors.New(u.Path()+" is not k8s-aws-terraform-ansible/ (no terraform/variables.tf)"), w)
					return
				}
				open(u.Path())
			}, w)
			d.Resize(fyne.NewSize(800, 600))
			d.Show()
		}
		w.SetContent(container.NewCenter(container.NewVBox(
			widget.NewLabel("Select your k8s-aws-terraform-ansible folder to edit."),
			widget.NewButton("Choose folder…", pick),
		)))
		w.Show()
		pick()
	}
	w.ShowAndRun()
}
