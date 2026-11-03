package main

import (
	"context"
	"fmt"
	"image/color"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// UI holds every widget so tests can drive it headlessly.
type UI struct {
	store *Store
	win   fyne.Window

	keyName      *widget.SelectEntry
	myIP         *widget.Entry
	detectBtn    *widget.Button
	masterType   *widget.Select
	workerType   *widget.Select
	rootVol      *widget.Entry
	workerCount  *widget.Select
	k8sMinor     *widget.Entry
	calico       *widget.Entry
	ansibleUser  *widget.Entry
	sshKey       *widget.Entry
	fieldErr     map[string]*canvas.Text
	cost         *widget.Label
	warnBox      *widget.Label
	status       *widget.Label
	keyHint      *widget.Label
	readOnly     *widget.Form
	nodes        *widget.Label
	nodeSummary  *widget.Label
	nodesHint    *widget.Label
	outputs      *widget.Label
	checkOut     *widget.Entry
	checkBtns    []*widget.Button
	tfvarsBanner *widget.Label

	mu         sync.Mutex
	detectedIP string
	runningWks int // workers in terraform state; -1 = no cluster
	cluster    clusterState
	stateReady chan struct{} // closed when the latest terraform state read has been applied
	running    bool
}

// Instance-type select shows a descriptive label; map it back to the name.
func typeLabel(t InstanceType) string {
	return fmt.Sprintf("%s — %d vCPU / %g GiB — $%.4f/h", t.Name, t.VCPU, t.MemGiB, t.USDHour)
}

func typeLabels() []string {
	var out []string
	for _, t := range sortedInstanceTypes() {
		out = append(out, typeLabel(t))
	}
	return out
}

func typeFromLabel(l string) string { name, _, _ := strings.Cut(l, " "); return name }

func selectType(s *widget.Select, name string) {
	if t, ok := instanceTypes[name]; ok {
		s.SetSelected(typeLabel(t))
	} else {
		s.ClearSelected()
	}
}

func errText() *canvas.Text {
	t := canvas.NewText("", color.NRGBA{R: 0xd0, G: 0x20, B: 0x30, A: 0xff})
	t.TextSize = theme.CaptionTextSize()
	return t
}

func hint(s string) *widget.Label {
	l := widget.NewLabel(s)
	l.Importance = widget.LowImportance
	l.Wrapping = fyne.TextWrapWord
	return l
}

func newUI(st *Store, w fyne.Window) *UI {
	u := &UI{store: st, win: w, fieldErr: map[string]*canvas.Text{}, runningWks: -1}
	for _, k := range []string{"KeyName", "MyIPCIDR", "MasterInstanceType", "WorkerInstanceType", "WorkerCount", "RootVolumeSize", "K8sMinor", "CalicoVersion", "AnsibleUser", "SSHPrivateKey"} {
		u.fieldErr[k] = errText()
	}
	onChange := func(string) { u.refreshDerived() }

	u.keyName = widget.NewSelectEntry(nil)
	u.keyName.SetPlaceHolder("existing EC2 key pair name")
	u.keyHint = hint("Looking up EC2 key pairs…")
	u.myIP = widget.NewEntry()
	u.myIP.SetPlaceHolder("203.0.113.10/32")
	u.myIP.OnChanged = onChange
	u.detectBtn = widget.NewButtonWithIcon("Detect my IP", theme.SearchIcon(), u.detectIP)
	u.masterType = widget.NewSelect(typeLabels(), onChange)
	u.workerType = widget.NewSelect(typeLabels(), onChange)
	var counts []string
	for i := 1; i <= maxWorkers; i++ {
		counts = append(counts, strconv.Itoa(i))
	}
	u.workerCount = widget.NewSelect(counts, onChange)
	u.rootVol = widget.NewEntry()
	u.rootVol.OnChanged = onChange
	u.k8sMinor = widget.NewEntry()
	u.k8sMinor.OnChanged = onChange
	u.calico = widget.NewEntry()
	u.ansibleUser = widget.NewEntry()
	u.sshKey = widget.NewEntry()
	u.sshKey.OnChanged = onChange
	u.cost = widget.NewLabel("")
	u.cost.TextStyle.Bold = true
	u.warnBox = widget.NewLabel("")
	u.warnBox.Wrapping = fyne.TextWrapWord
	u.warnBox.Importance = widget.WarningImportance
	u.status = widget.NewLabel("")
	u.tfvarsBanner = widget.NewLabel("")
	u.tfvarsBanner.Wrapping = fyne.TextWrapWord
	u.tfvarsBanner.Importance = widget.WarningImportance
	u.nodes = widget.NewLabel("")
	u.nodes.TextStyle.Monospace = true
	u.nodeSummary = widget.NewLabel("")
	u.nodeSummary.Wrapping = fyne.TextWrapWord
	u.nodeSummary.Importance = widget.MediumImportance
	u.nodesHint = widget.NewLabel("")
	u.nodesHint.Wrapping = fyne.TextWrapWord
	u.outputs = widget.NewLabel("")
	u.outputs.TextStyle.Monospace = true
	u.checkOut = widget.NewMultiLineEntry()
	u.checkOut.TextStyle.Monospace = true
	u.checkOut.Wrapping = fyne.TextWrapOff
	u.checkOut.SetPlaceHolder("Check output appears here.")
	u.readOnly = widget.NewForm()
	return u
}

func field(w fyne.CanvasObject, err *canvas.Text, h string) fyne.CanvasObject {
	items := []fyne.CanvasObject{w}
	if h != "" {
		items = append(items, hint(h))
	}
	return container.NewVBox(append(items, err)...)
}

func (u *UI) content() fyne.CanvasObject {
	e := u.fieldErr
	tf := widget.NewForm(
		widget.NewFormItem("key_name", field(u.keyName, e["KeyName"], "")),
		widget.NewFormItem("", u.keyHint),
		widget.NewFormItem("my_ip_cidr", field(container.NewBorder(nil, nil, nil, u.detectBtn, u.myIP), e["MyIPCIDR"],
			"Only this CIDR can reach SSH (22), the API server (6443) and NodePorts.")),
		widget.NewFormItem("master_instance_type", field(u.masterType, e["MasterInstanceType"], "")),
		widget.NewFormItem("worker_instance_type", field(u.workerType, e["WorkerInstanceType"], "")),
		widget.NewFormItem("worker_count", field(u.workerCount, e["WorkerCount"], "For a running cluster, apply with ./cluster.sh scale N.")),
		widget.NewFormItem("", u.nodeSummary),
		widget.NewFormItem("root_volume_size", field(u.rootVol, e["RootVolumeSize"], "GB, gp3, per node (20–200).")),
		widget.NewFormItem("estimated cost", u.cost),
	)
	ans := widget.NewForm(
		widget.NewFormItem("k8s_minor", field(u.k8sMinor, e["K8sMinor"], "pkgs.k8s.io minor, e.g. v1.31. Applies to new clusters only.")),
		widget.NewFormItem("calico_version", field(u.calico, e["CalicoVersion"], "Full tag including patch, e.g. v3.29.1.")),
	)
	inv := widget.NewForm(
		widget.NewFormItem("ansible_user", field(u.ansibleUser, e["AnsibleUser"], "")),
		widget.NewFormItem("ansible_ssh_private_key_file", field(u.sshKey, e["SSHPrivateKey"], "Must be the .pem for key_name.")),
	)

	settings := container.NewVScroll(container.NewVBox(
		u.tfvarsBanner,
		widget.NewCard("Terraform", tfvarsRel, tf),
		widget.NewCard("Ansible", groupVarsRel, ans),
		widget.NewCard("Ansible inventory", inventoryRel+" [all:vars]", inv),
	))

	readonly := container.NewVScroll(container.NewVBox(
		widget.NewCard("Nodes", "Deployed (terraform state) vs. worker_count in the form", container.NewVBox(u.nodesHint, u.nodes)),
		widget.NewCard("Pinned settings", "Fixed by the IAM policy, cluster.sh, or the playbooks", u.readOnly),
		widget.NewCard("Terraform outputs", "From local state", u.outputs),
	))

	var btns []fyne.CanvasObject
	for _, c := range checks {
		c := c
		b := widget.NewButton(c.Label, func() { u.runCheck(c) })
		u.checkBtns = append(u.checkBtns, b)
		btns = append(btns, container.NewHBox(b, hint(c.Note)))
	}
	checksTab := container.NewBorder(container.NewVBox(btns...), nil, nil, nil, u.checkOut)

	tabs := container.NewAppTabs(
		container.NewTabItemWithIcon("Settings", theme.DocumentCreateIcon(), settings),
		container.NewTabItemWithIcon("Nodes & read-only", theme.InfoIcon(), readonly),
		container.NewTabItemWithIcon("Checks", theme.ConfirmIcon(), checksTab),
	)

	save := widget.NewButtonWithIcon("Save", theme.DocumentSaveIcon(), func() { u.save() })
	save.Importance = widget.HighImportance
	reload := widget.NewButtonWithIcon("Reload", theme.ViewRefreshIcon(), u.reload)
	bottom := container.NewVBox(
		widget.NewSeparator(),
		u.warnBox,
		container.NewBorder(nil, nil, nil, container.NewHBox(reload, save), u.status),
	)
	top := hint("Edits data files only. Never runs apply, destroy or cluster.sh. Build with ./cluster.sh up, and remember ./cluster.sh down.")
	return container.NewBorder(top, bottom, nil, nil, tabs)
}

// form reads the widgets into Settings.
func (u *UI) form() Settings {
	vol, err := strconv.Atoi(strings.TrimSpace(u.rootVol.Text))
	if err != nil {
		vol = -1
	}
	wc, err := strconv.Atoi(u.workerCount.Selected)
	if err != nil {
		wc = -1
	}
	t := strings.TrimSpace
	return Settings{
		KeyName:            t(u.keyName.Text),
		MyIPCIDR:           t(u.myIP.Text),
		MasterInstanceType: typeFromLabel(u.masterType.Selected),
		WorkerInstanceType: typeFromLabel(u.workerType.Selected),
		RootVolumeSize:     vol,
		WorkerCount:        wc,
		K8sMinor:           t(u.k8sMinor.Text),
		CalicoVersion:      t(u.calico.Text),
		AnsibleUser:        t(u.ansibleUser.Text),
		SSHPrivateKey:      t(u.sshKey.Text),
	}
}

func (u *UI) setForm(s Settings) {
	u.keyName.SetText(s.KeyName)
	u.myIP.SetText(s.MyIPCIDR)
	selectType(u.masterType, s.MasterInstanceType)
	selectType(u.workerType, s.WorkerInstanceType)
	if s.WorkerCount >= 1 && s.WorkerCount <= maxWorkers {
		u.workerCount.SetSelected(strconv.Itoa(s.WorkerCount))
	} else {
		u.workerCount.ClearSelected()
	}
	u.rootVol.SetText(strconv.Itoa(s.RootVolumeSize))
	u.k8sMinor.SetText(s.K8sMinor)
	u.calico.SetText(s.CalicoVersion)
	u.ansibleUser.SetText(s.AnsibleUser)
	u.sshKey.SetText(s.SSHPrivateKey)
}

func (u *UI) showErrors(errs map[string]string) {
	for k, t := range u.fieldErr {
		t.Text = errs[k]
		t.Refresh()
	}
}

// refreshDerived updates cost and warnings as the user types.
func (u *UI) refreshDerived() {
	if u.cost == nil {
		return
	}
	s := u.form()
	if s.RootVolumeSize > 0 && s.WorkerCount > 0 && instanceTypes[s.MasterInstanceType].Name != "" && instanceTypes[s.WorkerInstanceType].Name != "" {
		u.cost.SetText(fmt.Sprintf("≈ $%.2f/day  (1 master + %d workers, EC2 + EBS, approximate)", estimateUSDPerDay(s), s.WorkerCount))
	} else {
		u.cost.SetText("—")
	}
	u.mu.Lock()
	ip, running := u.detectedIP, u.runningWks
	u.mu.Unlock()
	u.renderNodes(s.WorkerCount)
	if w := warnings(s, ip, running); len(w) > 0 {
		u.warnBox.SetText("⚠ " + strings.Join(w, "\n⚠ "))
		u.warnBox.Show()
	} else {
		u.warnBox.SetText("")
		u.warnBox.Hide()
	}
}

func (u *UI) reload() {
	s, src, err := u.store.Load()
	if err != nil {
		u.status.SetText("Load error: " + err.Error())
		dialog.ShowError(err, u.win)
		return
	}
	u.setForm(s)
	u.showErrors(nil)
	if u.store.tfvarsExists() {
		u.tfvarsBanner.Hide()
	} else {
		u.tfvarsBanner.SetText("terraform/terraform.tfvars does not exist yet. Values were loaded from terraform.tfvars.example; Save creates the file.")
		u.tfvarsBanner.Show()
	}
	u.refreshReadOnly(src)
	u.refreshDerived()
	u.status.SetText("Loaded " + time.Now().Format("15:04:05"))
}

func (u *UI) refreshReadOnly(src sources) {
	pinned := [][2]string{
		{"region", pinnedRegion},
		{"pod_network_cidr", pinnedPodCIDR},
		{"vpc_cidr", pinnedVPCCIDR},
		{"ami", pinnedAMI},
		{"aws_profile", awsProfile},
	}
	u.readOnly.Items = nil
	for _, p := range pinned {
		l := widget.NewLabel(p[1])
		l.TextStyle.Monospace = true
		u.readOnly.Append(p[0], l)
	}
	u.readOnly.Refresh()

	inv := inventoryHosts(src.inventory)
	ready := make(chan struct{})
	u.mu.Lock()
	u.cluster = clusterState{} // unknown until terraform output returns
	u.stateReady = ready
	u.mu.Unlock()

	u.outputs.SetText("Reading terraform state…")
	go func() {
		outs := terraformOutputs(context.Background(), u.store.Root)
		text := "No infrastructure (or no state)."
		if len(outs) > 0 {
			keys := make([]string, 0, len(outs))
			for k := range outs {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var b strings.Builder
			for _, k := range keys {
				fmt.Fprintf(&b, "%-18s %s\n", k, outs[k])
			}
			text = strings.TrimRight(b.String(), "\n")
		}
		cs := stateFromOutputs(outs, inv)
		running := -1
		if cs.Running {
			running = len(cs.WorkerIPs)
		}
		fyne.DoAndWait(func() {
			u.mu.Lock()
			u.runningWks = running
			u.cluster = cs
			u.mu.Unlock()
			u.outputs.SetText(text)
			u.refreshDerived()
		})
		close(ready)
	}()
}

// renderNodes redraws the Nodes panel and the summary under worker_count.
func (u *UI) renderNodes(desired int) {
	u.mu.Lock()
	cs := u.cluster
	u.mu.Unlock()
	rows, summary := planNodes(cs, desired)
	u.nodeSummary.SetText(summary)
	u.nodes.SetText(formatNodeRows(rows))
	if cs.Known && !cs.Running {
		u.nodesHint.SetText("Not deployed. IPs appear here after ./cluster.sh up (inventory.ini is refreshed then).")
	} else {
		u.nodesHint.SetText("IPs come from terraform state, not inventory.ini. Changing worker_count here only previews the change; ./cluster.sh scale N applies it.")
	}
}

// lookupsAsync fills the key-pair list and the detected IP without blocking startup.
func (u *UI) lookupsAsync() {
	var wg sync.WaitGroup
	var pairs []string
	var ip string
	wg.Add(2)
	go func() { defer wg.Done(); pairs = listKeyPairs(context.Background()) }()
	go func() { defer wg.Done(); ip = detectMyIP(context.Background()) }()
	wg.Wait()
	u.mu.Lock()
	u.detectedIP = ip
	u.mu.Unlock()
	fyne.Do(func() {
		if len(pairs) > 0 {
			u.keyName.SetOptions(pairs)
			u.keyHint.SetText("EC2 key pairs in " + pinnedRegion + ": " + strings.Join(pairs, ", "))
		} else {
			u.keyHint.SetText("Could not list key pairs (AWS profile " + awsProfile + " unavailable); type the name.")
		}
		u.refreshDerived()
	})
}

func (u *UI) detectIP() {
	u.detectBtn.Disable()
	go func() {
		ip := detectMyIP(context.Background())
		fyne.Do(func() {
			u.detectBtn.Enable()
			if ip == "" {
				dialog.ShowInformation("Detect my IP", "Could not detect your global IP.", u.win)
				return
			}
			u.mu.Lock()
			u.detectedIP = ip
			u.mu.Unlock()
			u.myIP.SetText(ip + "/32")
		})
	}()
}

// save validates, writes changed files, and reports the diff. It returns the
// diffs so tests can assert on them.
func (u *UI) save() []string {
	s := u.form()
	errs := validate(s)
	u.showErrors(errs)
	if len(errs) > 0 {
		u.status.SetText(fmt.Sprintf("Not saved: %d field(s) need fixing.", len(errs)))
		return nil
	}
	diffs, err := u.store.Save(s)
	if err != nil {
		u.status.SetText("Save failed: " + err.Error())
		dialog.ShowError(err, u.win)
		return diffs
	}
	if len(diffs) == 0 {
		u.status.SetText("No changes; nothing written.")
		return nil
	}
	u.status.SetText(fmt.Sprintf("Saved %d file(s) at %s.", len(diffs), time.Now().Format("15:04:05")))
	u.tfvarsBanner.Hide()
	if u.win != nil {
		d := widget.NewLabel(strings.Join(diffs, "\n"))
		d.TextStyle.Monospace = true
		sc := container.NewScroll(d)
		sc.SetMinSize(fyne.NewSize(600, 300))
		dialog.ShowCustom("Saved — changes", "OK", sc, u.win)
	}
	return diffs
}

func (u *UI) runCheck(c Check) {
	u.mu.Lock()
	if u.running {
		u.mu.Unlock()
		return
	}
	u.running = true
	u.mu.Unlock()
	for _, b := range u.checkBtns {
		b.Disable()
	}
	u.checkOut.SetText("Running " + c.Label + " …")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		out, ok := c.Run(ctx, u.store.Root)
		head := "✓ OK\n\n"
		if !ok {
			head = "✗ FAILED\n\n"
		}
		fyne.Do(func() {
			u.checkOut.SetText(head + out)
			for _, b := range u.checkBtns {
				b.Enable()
			}
			u.mu.Lock()
			u.running = false
			u.mu.Unlock()
		})
	}()
}

// waitState blocks until the most recent terraform state read has been applied.
func (u *UI) waitState() {
	u.mu.Lock()
	ch := u.stateReady
	u.mu.Unlock()
	if ch != nil {
		<-ch
	}
}
