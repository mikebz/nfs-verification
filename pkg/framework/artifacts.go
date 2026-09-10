package framework

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeInspectTimeout bounds one node's share of the artifact bundle.
const nodeInspectTimeout = 10 * time.Second

// CollectArtifacts writes the triage bundle for a failed case: the environment
// record, pod logs, Kubernetes Events, server logs, /proc/mounts and dmesg from
// every involved node, and the fault timeline. A failure filed without this
// bundle will be closed as unreproducible.
func (f *Framework) CollectArtifacts(ctx context.Context) error {
	dir := CaseDir(f.CaseID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var errs []string

	if f.Env != nil {
		if _, err := f.Env.Write(dir); err != nil {
			errs = append(errs, fmt.Sprintf("environment.json: %v", err))
		}
	}
	if err := f.dumpTimeline(dir); err != nil {
		errs = append(errs, fmt.Sprintf("timeline: %v", err))
	}
	if err := f.dumpPods(ctx, dir, Namespace, f.Selector(), "client"); err != nil {
		errs = append(errs, fmt.Sprintf("client pods: %v", err))
	}
	if ns := f.Env.ServerNamespace(); ns != "" {
		if err := f.dumpPods(ctx, dir, ns, Cfg().ServerSelector, "server"); err != nil {
			errs = append(errs, fmt.Sprintf("server pods: %v", err))
		}
	}
	if err := f.dumpEvents(ctx, dir); err != nil {
		errs = append(errs, fmt.Sprintf("events: %v", err))
	}
	if f.Caps.NodeAgent {
		if err := f.dumpNodeState(ctx, dir); err != nil {
			errs = append(errs, fmt.Sprintf("node state: %v", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("artifact collection had gaps: %s", strings.Join(errs, "; "))
	}
	f.T.Logf("artifacts written to %s", dir)
	return nil
}

func (f *Framework) dumpPods(ctx context.Context, dir, ns, selector, kind string) error {
	pods, err := f.C.Kube.CoreV1().Pods(ns).List(ctx, ListOptions(selector))
	if err != nil {
		return err
	}
	sub := filepath.Join(dir, kind+"-pods")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if b, err := json.MarshalIndent(p, "", "  "); err == nil {
			_ = os.WriteFile(filepath.Join(sub, p.Name+".json"), b, 0o644)
		}
		for _, c := range p.Spec.Containers {
			f.writeLogs(ctx, sub, ns, p.Name, c.Name, false)
			// Previous-container logs are where a crash actually shows up.
			f.writeLogs(ctx, sub, ns, p.Name, c.Name, true)
		}
	}
	return nil
}

func (f *Framework) writeLogs(ctx context.Context, dir, ns, pod, container string, previous bool) {
	suffix := ".log"
	if previous {
		suffix = ".previous.log"
	}
	req := f.C.Kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous})
	rc, err := req.Stream(ctx)
	if err != nil {
		return
	}
	defer rc.Close()
	out, err := os.Create(filepath.Join(dir, pod+"-"+container+suffix))
	if err != nil {
		return
	}
	defer out.Close()
	_, _ = io.Copy(out, rc)
}

func (f *Framework) dumpEvents(ctx context.Context, dir string) error {
	namespaces := []string{Namespace}
	if ns := f.Env.ServerNamespace(); ns != "" && ns != Namespace {
		namespaces = append(namespaces, ns)
	}
	for _, ns := range namespaces {
		evs, err := f.C.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return err
		}
		var sb strings.Builder
		for _, e := range evs.Items {
			fmt.Fprintf(&sb, "%s\t%s\t%s\t%s/%s\t%s\n",
				e.LastTimestamp.Time.Format("15:04:05"), e.Type, e.Reason,
				e.InvolvedObject.Kind, e.InvolvedObject.Name, e.Message)
		}
		if err := os.WriteFile(filepath.Join(dir, "events-"+ns+".txt"), []byte(sb.String()), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *Framework) dumpNodeState(ctx context.Context, dir string) error {
	agent, err := NodeAgent(ctx, f.C)
	if err != nil {
		return err
	}
	nodes, err := agent.Nodes(ctx)
	if err != nil {
		return err
	}
	for _, n := range nodes {
		// One node at a time, each on its own short clock. A node with a wedged
		// mount blocks on cat /proc/mounts, and the whole point of the bundle is
		// that it still contains the other nodes when that happens.
		nodeCtx, cancel := context.WithTimeout(ctx, nodeInspectTimeout)
		if mounts, err := agent.ReadFile(nodeCtx, n, "/proc/mounts"); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "proc-mounts-"+n+".txt"), []byte(mounts), 0o644)
		} else {
			_ = os.WriteFile(filepath.Join(dir, "proc-mounts-"+n+".txt"),
				[]byte(fmt.Sprintf("unreadable within %s: %v\n", nodeInspectTimeout, err)), 0o644)
		}
		if dmesg, err := agent.Dmesg(nodeCtx, n); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "dmesg-"+n+".txt"), []byte(dmesg), 0o644)
		}
		cancel()
	}
	return nil
}

// FaultEvent is one entry in the injected-fault timeline.
type FaultEvent struct {
	At     string `json:"at"`
	Action string `json:"action"`
	Target string `json:"target"`
	Detail string `json:"detail,omitempty"`
}

// RecordFault appends to the case timeline. Chaos helpers call it so that the
// bundle says exactly what was done and when.
func (f *Framework) RecordFault(e FaultEvent) {
	f.faults = append(f.faults, e)
	if f.T != nil {
		f.T.Logf("fault: %s %s %s", e.Action, e.Target, e.Detail)
	}
}

func (f *Framework) dumpTimeline(dir string) error {
	b, err := json.MarshalIndent(f.faults, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "fault-timeline.json"), b, 0o644)
}
