// Package chaos holds the fault operations the CHAOS cases inject. Each one is
// expressed as a Kubernetes operation or a signal delivered through the node
// agent, never as a platform API, so that the same case runs on GKE and on bare
// metal. Node power operations, which genuinely differ per platform, arrive
// with the cases that need them.
//
// Every operation here records what it did on the fixture's fault timeline, and
// every one refuses to act on a target it cannot identify. A case that measures
// a recovery from a fault that was never injected passes for the wrong reason,
// which is worse than a case that does not run.
package chaos

import (
	"context"
	"fmt"
	"path"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/mikebz/nfs-verification/pkg/framework"
)

// Target is the server pod a fault is aimed at, resolved live rather than read
// from the environment record: a cached preflight names the pod that was there
// an hour ago, and the chaos cases move pods around.
type Target struct {
	Namespace string
	Pod       string
	UID       types.UID
	Node      string
	// Controller is the owning workload, empty when the pod is unmanaged.
	Controller string
	// Containers holds the container commands, which is where the process name
	// comes from when nobody passed one.
	Containers []corev1.Container
}

// ServerTarget picks the NFS server pod to injure. With fan-out greater than
// one it takes the first, deterministically, so a rerun hits the same pod.
func ServerTarget(ctx context.Context, f *framework.Framework) (Target, error) {
	pods, err := framework.ServerPods(ctx, f.C)
	if err != nil {
		return Target{}, fmt.Errorf("discovering NFS server pods: %w", err)
	}
	if len(pods) == 0 {
		return Target{}, fmt.Errorf("no NFS server pods found: pass -server-namespace and -server-selector, " +
			"otherwise there is nothing for a chaos case to injure")
	}
	p := &pods[0]
	t := Target{Namespace: p.Namespace, Pod: p.Name, UID: p.UID, Node: p.Spec.NodeName, Containers: p.Spec.Containers}
	for _, ref := range p.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			t.Controller = ref.Kind + "/" + ref.Name
		}
	}
	if t.Node == "" {
		return t, fmt.Errorf("server pod %s/%s is not scheduled to a node", t.Namespace, t.Pod)
	}
	return t, nil
}

// ProcessPattern is the fixed string that identifies the server process on the
// node. The flag wins; otherwise it comes from the container's own command,
// which is the only place the cluster states it.
func ProcessPattern(t Target) (string, error) {
	// The flag is checked exactly as a derived name is. An operator who passes
	// -server-process=sh means a server called sh, but what the node gets is a
	// SIGKILL to every shell on it, and the guard below is the only thing
	// standing between a chaos case and taking the node out of service. There
	// is no NFS server this rejects, so nothing is lost by refusing.
	if p := framework.Cfg().ServerProcess; p != "" {
		if !usableAsPattern(p) {
			return "", fmt.Errorf("-server-process=%q is too generic to signal on: it would match processes "+
				"that have nothing to do with NFS, and killing those on a node takes the node out of service. "+
				"Pass the name the server process actually runs under", p)
		}
		return p, nil
	}
	for _, c := range t.Containers {
		if len(c.Command) == 0 {
			continue
		}
		if name := path.Base(c.Command[0]); usableAsPattern(name) {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot tell which process serves NFS in pod %s: its containers declare no command, "+
		"so the process name lives in the image entrypoint where the cluster cannot see it; pass -server-process",
		t.Pod)
}

// usableAsPattern rejects names too generic to signal on. Killing everything
// matching "sh" on a node takes the node out, and a case that does that is a
// worse outage than the one it was written to measure.
func usableAsPattern(name string) bool {
	switch name {
	case "", "sh", "bash", "dash", "env", "sleep", "tini", "dumb-init", "entrypoint.sh", "start.sh":
		return false
	}
	return len(name) >= 4
}

// KillServerProcess sends a signal to the server process on its node and
// reports how many processes it hit. It looks before it signals and returns an
// error when nothing matches, because measuring a recovery from a fault that
// never happened is the failure mode this whole package has to avoid.
func KillServerProcess(ctx context.Context, f *framework.Framework, t Target, signal string) (int, error) {
	pattern, err := ProcessPattern(t)
	if err != nil {
		return 0, err
	}
	agent, err := framework.NodeAgent(ctx, f.C)
	if err != nil {
		return 0, fmt.Errorf("node agent unavailable, so the server process cannot be signalled: %w", err)
	}
	procs, err := agent.ListProcesses(ctx, t.Node, pattern)
	if err != nil {
		return 0, fmt.Errorf("listing processes matching %q on %s: %w", pattern, t.Node, err)
	}
	if len(procs) == 0 {
		return 0, fmt.Errorf("no process matching %q on %s (server pod %s); pass -server-process with the name "+
			"the server actually runs under", pattern, t.Node, t.Pod)
	}
	killed, err := agent.KillProcess(ctx, t.Node, pattern, signal)
	if err != nil {
		return 0, fmt.Errorf("signalling %q on %s: %w", pattern, t.Node, err)
	}
	f.RecordFault(framework.FaultEvent{
		At: time.Now().UTC().Format(time.RFC3339), Action: "kill-" + strings.ToLower(signal),
		Target: t.Node + "/" + t.Pod,
		Detail: fmt.Sprintf("pattern %q hit %d of %d matching processes: %s",
			pattern, killed, len(procs), strings.Join(procs, "; ")),
	})
	if killed == 0 {
		return 0, fmt.Errorf("matched %d processes for %q on %s but signalled none", len(procs), pattern, t.Node)
	}
	return killed, nil
}

// DeleteServerPod deletes the server pod, which is the fault an operator
// injects by accident every day. It refuses on a pod no controller owns: such a
// pod does not come back, and the case would be measuring a permanent outage
// against a recovery SLO.
func DeleteServerPod(ctx context.Context, f *framework.Framework, t Target) error {
	if t.Controller == "" {
		return fmt.Errorf("server pod %s/%s has no controller, so deleting it would not bring it back; "+
			"this case needs a server managed by a Deployment, StatefulSet or DaemonSet", t.Namespace, t.Pod)
	}
	// Graceful. The abrupt path is CHAOS-01's signal, and a force delete here
	// would remove the pod from the API before the server stopped, which makes
	// the recovery measurement start at the wrong moment.
	if err := f.C.Kube.CoreV1().Pods(t.Namespace).Delete(ctx, t.Pod, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting server pod %s/%s: %w", t.Namespace, t.Pod, err)
	}
	f.RecordFault(framework.FaultEvent{
		At: time.Now().UTC().Format(time.RFC3339), Action: "delete-pod",
		Target: t.Node + "/" + t.Pod, Detail: "owned by " + t.Controller,
	})
	return nil
}

// WaitServerBack waits for the fan-out to hold a ready server pod again. It is
// a diagnostic, not the assertion: what the plan measures is time to first
// successful I/O from a client, and a server that reports Ready before it
// serves would otherwise be recorded as a recovery.
func WaitServerBack(ctx context.Context, f *framework.Framework, timeout time.Duration) error {
	return framework.WaitServersReady(ctx, f.C, timeout)
}

// WaitServerGone waits for the targeted server pod instance to disappear from the API.
// For controllers like StatefulSets or DaemonSets where the replacement pod shares the same
// name, a changed UID confirms the targeted instance was deleted and replaced.
func WaitServerGone(ctx context.Context, f *framework.Framework, t Target, timeout time.Duration) error {
	return framework.Poll(ctx, framework.PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pod, err := f.C.Kube.CoreV1().Pods(t.Namespace).Get(ctx, t.Pod, metav1.GetOptions{})
		if err != nil {
			return framework.IgnoreNotFound(err) == nil, nil
		}
		if pod.UID != t.UID {
			return true, nil
		}
		if pod.DeletionTimestamp != nil {
			return false, fmt.Errorf("server pod %s/%s still terminating", t.Namespace, t.Pod)
		}
		return false, fmt.Errorf("server pod %s/%s still present", t.Namespace, t.Pod)
	})
}

// WaitServerReplaced waits for a replacement server pod instance (with a different UID or name)
// to become Running and Ready after a fault.
func WaitServerReplaced(ctx context.Context, f *framework.Framework, old Target, timeout time.Duration) error {
	return framework.Poll(ctx, framework.PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pods, err := framework.ServerPods(ctx, f.C)
		if err != nil {
			return false, err
		}
		for i := range pods {
			p := &pods[i]
			if (p.UID != old.UID || p.Name != old.Pod) && framework.PodReady(p) {
				return true, nil
			}
		}
		return false, fmt.Errorf("no replacement server pod is ready yet")
	})
}
