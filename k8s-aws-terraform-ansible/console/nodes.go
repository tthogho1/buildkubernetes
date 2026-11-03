package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// clusterState is what is actually deployed, read from terraform state
// (the source of truth) plus the inventory file (which may be stale).
type clusterState struct {
	Known     bool              // terraform state has been read
	Running   bool              // state has instances
	MasterIP  string            // terraform output master_public_ip
	WorkerIPs []string          // terraform output worker_public_ips, index 0 = worker1
	Inventory map[string]string // inventory.ini name -> ansible_host
}

func stateFromOutputs(outs map[string]string, inv [][2]string) clusterState {
	st := clusterState{Known: true, Inventory: map[string]string{}}
	for _, h := range inv {
		st.Inventory[h[0]] = h[1]
	}
	if ips, ok := outs["worker_public_ips"]; ok {
		if json.Unmarshal([]byte(ips), &st.WorkerIPs) == nil {
			st.Running = true
		}
	}
	st.MasterIP = outs["master_public_ip"]
	if st.MasterIP != "" {
		st.Running = true
	}
	return st
}

// NodeRow is one line of the Nodes panel.
type NodeRow struct{ Name, IP, Status string }

// planNodes compares the deployed cluster with the desired worker count.
// Terraform's count adds and removes the highest indexes, so workerK is
// aws_instance.k8s_worker[K-1] (same mapping cluster.sh scale uses).
func planNodes(st clusterState, desired int) (rows []NodeRow, summary string) {
	if !st.Known {
		return nil, "Reading terraform state…"
	}
	if desired < 1 {
		desired = 0
	}
	if !st.Running {
		rows = append(rows, NodeRow{"master", "—", "created by ./cluster.sh up"})
		for i := 1; i <= desired; i++ {
			rows = append(rows, NodeRow{fmt.Sprintf("worker%d", i), "—", "created by ./cluster.sh up"})
		}
		return rows, fmt.Sprintf("No cluster running. ./cluster.sh up creates 1 master + %d workers.", desired)
	}

	stale := func(name, ip string) string {
		if inv, ok := st.Inventory[name]; ip != "" && (!ok || inv != ip) {
			return "; inventory.ini stale (./cluster.sh inventory)"
		}
		return ""
	}
	rows = append(rows, NodeRow{"master", st.MasterIP, "running" + stale("master", st.MasterIP)})
	have := len(st.WorkerIPs)
	n := have
	if desired > n {
		n = desired
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("worker%d", i)
		switch {
		case i <= have && i <= desired:
			rows = append(rows, NodeRow{name, st.WorkerIPs[i-1], "running" + stale(name, st.WorkerIPs[i-1])})
		case i <= have:
			rows = append(rows, NodeRow{name, st.WorkerIPs[i-1], fmt.Sprintf("WILL BE REMOVED (drained) by ./cluster.sh scale %d", desired)})
		default:
			rows = append(rows, NodeRow{name, "(new)", fmt.Sprintf("will be added by ./cluster.sh scale %d", desired)})
		}
	}
	switch {
	case desired == have:
		summary = fmt.Sprintf("Running: 1 master + %d workers (matches worker_count).", have)
	case desired > have:
		summary = fmt.Sprintf("Running: %d workers → %d after ./cluster.sh scale %d (+%d).", have, desired, desired, desired-have)
	default:
		summary = fmt.Sprintf("Running: %d workers → %d after ./cluster.sh scale %d (−%d, drained first).", have, desired, desired, have-desired)
	}
	return rows, summary
}

func formatNodeRows(rows []NodeRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-8s  %-15s  %s\n", r.Name, r.IP, r.Status)
	}
	return strings.TrimRight(b.String(), "\n")
}
