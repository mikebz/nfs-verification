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
	// Containers is the pod's containers, for a case that has to look inside
	// one. It is not where the process name comes from: that is preflight's
	// answer alone, see ResolveProcess.
	Containers []corev1.Container
}

// ServerTarget picks the NFS server pod to injure. With fan-out greater than
// one it takes the first, deterministically, so a rerun hits the same pod.
//
// A pod that is already being deleted, or that has not started, is never
// chosen. Discovery keeps both, on purpose: a terminating pod still belongs in
// the artifact bundle, and a server stuck Pending is worth reporting. Injuring
// one is different, because it measures something this case did not cause, and
// the result would be attributed to the fault anyway.
//
// This is the same confusion as F-013 in docs/findings.md, one level up. There
// it was a terminating pod answering yes to "is a server ready"; here it is a
// terminating pod answering yes to "is there a server to injure".
func ServerTarget(ctx context.Context, f *framework.Framework) (Target, error) {
	pods, err := framework.ServerPods(ctx, f.C)
	if err != nil {
		return Target{}, fmt.Errorf("discovering NFS server pods: %w", err)
	}
	p, err := firstInjurable(pods)
	if err != nil {
		return Target{}, err
	}
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

// firstInjurable returns the first server pod that a fault may be aimed at.
//
// Discovery is sorted by name and deliberately keeps pods this function must
// not pick, so with fan-out greater than one the pod sorted first can be unfit
// while a healthy pod sits behind it. Taking that one injures nothing, and the
// case credits the result to a fault it believes it caused.
//
// Empty-handed is reported with the reason for every pod rejected, because the
// answers call for different actions: no pods at all is discovery pointed at
// the wrong place, and pods that are all unfit is something outside this case
// removing or restarting them.
func firstInjurable(pods []corev1.Pod) (*corev1.Pod, error) {
	if len(pods) == 0 {
		return nil, fmt.Errorf("no NFS server pods found: pass -server-namespace and -server-selector, " +
			"otherwise there is nothing for a chaos case to injure")
	}
	for i := range pods {
		if unfitToInjure(&pods[i]) == "" {
			return &pods[i], nil
		}
	}
	why := make([]string, 0, len(pods))
	for i := range pods {
		why = append(why, pods[i].Name+" "+unfitToInjure(&pods[i]))
	}
	return nil, fmt.Errorf("no NFS server pod found that can be injured (%s), so a fault aimed at one "+
		"would measure something this case did not cause. Either something outside the suite is removing "+
		"them, or a previous fault has not finished", strings.Join(why, ", "))
}

// unfitToInjure says why a fault must not be aimed at this pod, or "" if one
// may be. One predicate serves both the choice and the error that explains it,
// so the two cannot drift apart and start disagreeing about which pods count.
//
// Two states are refused:
//
//   - Being deleted. A gracefully deleted pod keeps phase Running for its whole
//     grace period, so the phase check below does not catch it. Injuring one
//     measures the tail of a deletion somebody else started.
//   - Any phase other than Running. Such a pod has no running container, so
//     there is no process to signal, and deleting it removes a replica that was
//     not serving while the pod that is serving carries on untouched.
//
// In practice the second means Pending, because discovery only ever returns
// Running and Pending. It is written as the general rule anyway: the cost of
// refusing a Succeeded or Failed pod is nothing, and a phase filter that has to
// agree with another package's filter to stay correct is one edit away from not
// being correct. Discovery keeps those pods on purpose, because a server that
// cannot start is worth reporting; that is still not a thing a fault can be
// aimed at.
//
// Readiness is deliberately not required, which is the narrower rule than it
// may look. A Running server that has gone not-ready is still serving, still
// exec-able, and is frequently the exact pod a repeated-failover case means to
// hit. Refusing it would turn "the server is sick" into "the harness found no
// target", reporting a symptom of the system under test as a harness error.
func unfitToInjure(p *corev1.Pod) string {
	if p.DeletionTimestamp != nil {
		return "is being deleted"
	}
	if p.Status.Phase != corev1.PodRunning {
		return "is " + string(p.Status.Phase)
	}
	return ""
}

// Process is the process a fault is aimed at: the pattern that will be matched
// on the node, where that pattern came from, and the evidence for it.
//
// The source travels with the pattern because the three sources are not equally
// good. What was observed holding the socket is a fact about the running
// server; a container's declared command is a proxy for it that is wrong on any
// server that is supervised rather than being PID 1. A fault record that does
// not say which one it used cannot be audited afterwards.
type Process struct {
	// Pattern is matched against every command line on the node.
	Pattern string
	// Source names where the pattern came from, for the fault record.
	Source string
	// Evidence is what backs it, empty for a pattern nobody had to derive.
	Evidence string
}

// ResolveProcess names the process a fault will signal. Preflight is the only
// source. It observed which process holds the listening NFS socket in each
// server pod and recorded it, and a case reads that record.
//
// There is deliberately nothing to fall back to. The two candidates a harness
// could invent are both wrong often enough to be dangerous: the container's
// declared command is the supervisor on a supervised server, so killing it
// stops the pod rather than the NFS server and every recovery measured
// afterwards describes a container restart (F-026 in docs/findings.md); and a
// name an operator types is a name nobody checked against the running server,
// signalled node-wide. A guess that kills the wrong process does not fail the
// case, it produces a confident measurement of the wrong event.
//
// So when preflight could not name it, the case reports blocked and says to run
// preflight again. Preflight is a precondition for the suite, not an optional
// step: a run where it cannot read the server pod has a problem no fallback
// here would fix.
//
// Nothing is probed at fault time. The name is a property of the image and
// cannot have changed since preflight read it, so a case that re-derived it
// would pay an exec into the server for an answer it already has. The pid is
// the opposite, it changes on every restart, and that is why none is recorded:
// a case that needs one observes it live.
func ResolveProcess(f *framework.Framework, t Target) (Process, error) {
	recorded, why := recordedProcess(f, t)
	return chooseProcess(recorded, why, t)
}

// recordedProcess returns what preflight recorded for the target pod, and why
// not when it recorded nothing. The pod is matched by name rather than taken
// first, because with fan-out greater than one the record holds several and a
// fault is aimed at one of them.
func recordedProcess(f *framework.Framework, t Target) (string, string) {
	if f == nil || f.Env == nil {
		return "", "no environment record is loaded"
	}
	for _, s := range f.Env.Servers {
		if s.Namespace == t.Namespace && s.Pod == t.Pod {
			if s.Process != "" {
				return s.Process, ""
			}
			return "", s.ProcessNote
		}
	}
	return "", fmt.Sprintf("preflight recorded no server pod %s/%s; it was discovered after preflight ran, "+
		"so re-run it with -refresh-preflight", t.Namespace, t.Pod)
}

// chooseProcess judges the recorded name, split out so the decision can be
// tested without a cluster.
func chooseProcess(recorded, whyNotRecorded string, t Target) (Process, error) {
	// A recorded name is still checked before it is signalled. Preflight names
	// whatever holds the socket, and on some image that could be a wrapper;
	// what the node would receive is a SIGKILL to every process of that name,
	// so this guard is the only thing standing between a chaos case and taking
	// a node out of service. There is no NFS server it rejects.
	switch {
	case recorded == "":
		why := whyNotRecorded
		if why == "" {
			why = "preflight recorded no process for this pod"
		}
		return Process{}, fmt.Errorf("preflight did not name the process serving NFS in pod %s: %s. "+
			"Re-run preflight with -refresh-preflight", t.Pod, why)
	case !usableAsPattern(recorded):
		return Process{}, fmt.Errorf("preflight recorded %q as serving NFS in pod %s, which is too generic to "+
			"signal on: killing every process of that name on a node takes the node out of service", recorded, t.Pod)
	}
	return Process{
		Pattern:  recorded,
		Source:   "observed by preflight to be serving NFS",
		Evidence: "recorded in environment.json as the process holding the listening socket on port 2049",
	}, nil
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
//
// The fault record names the source of the pattern as well as the pattern, so
// that a kill aimed by observation and one aimed by an operator's flag can be
// told apart months later without rerunning anything.
func KillServerProcess(ctx context.Context, f *framework.Framework, t Target, signal string) (int, error) {
	p, err := ResolveProcess(f, t)
	if err != nil {
		return 0, err
	}
	pattern := p.Pattern
	agent, err := framework.NodeAgent(ctx, f.C)
	if err != nil {
		return 0, fmt.Errorf("node agent unavailable, so the server process cannot be signalled: %w", err)
	}
	procs, err := agent.ListProcesses(ctx, t.Node, pattern)
	if err != nil {
		return 0, fmt.Errorf("listing processes matching %q on %s: %w", pattern, t.Node, err)
	}
	if len(procs) == 0 {
		return 0, fmt.Errorf("no process matching %q on %s (server pod %s), named by %s; the server preflight "+
			"observed is gone or runs under another name now, so re-run preflight with -refresh-preflight",
			pattern, t.Node, t.Pod, p.Source)
	}
	killed, err := agent.KillProcess(ctx, t.Node, pattern, signal)
	if err != nil {
		return 0, fmt.Errorf("signalling %q on %s: %w", pattern, t.Node, err)
	}
	detail := fmt.Sprintf("pattern %q, from %s, hit %d of %d matching processes: %s",
		pattern, p.Source, killed, len(procs), strings.Join(procs, "; "))
	if p.Evidence != "" {
		detail += ". " + p.Evidence
	}
	f.RecordFault(framework.FaultEvent{
		At: time.Now().UTC().Format(time.RFC3339), Action: "kill-" + strings.ToLower(signal),
		Target: t.Node + "/" + t.Pod,
		Detail: detail,
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
