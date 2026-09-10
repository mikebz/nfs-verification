package framework

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// agentDaemonSet is the one privileged component in the suite. Its absence is a
// preflight failure rather than a silent skip: without it there are no
// node-level assertions at all. It lives in the configured namespace like
// everything else the suite creates.
const agentDaemonSet = "nfs-verification-node-agent"

// Agent runs commands in the host namespaces of a node, which is how the suite
// reads /proc/mounts, reads dmesg and signals processes.
type Agent struct {
	c    *Client
	pods map[string]string // node name -> agent pod name
}

var (
	agentOnce sync.Once
	agentVal  *Agent
	agentErr  error
)

// NodeAgent returns the shared agent, creating the DaemonSet on first use.
func NodeAgent(ctx context.Context, c *Client) (*Agent, error) {
	agentOnce.Do(func() {
		agentVal, agentErr = installAgent(ctx, c)
	})
	if agentErr != nil {
		return nil, agentErr
	}
	// Pods move; refresh the mapping on every call rather than caching a node
	// list that goes stale the moment a chaos case stops a node.
	if err := agentVal.refresh(ctx); err != nil {
		return nil, err
	}
	return agentVal, nil
}

func installAgent(ctx context.Context, c *Client) (*Agent, error) {
	ns := Namespace
	var ds appsv1.DaemonSet
	if err := render("node-agent-daemonset.yaml", map[string]string{
		"Name":      agentDaemonSet,
		"Namespace": ns,
		"Image":     Cfg().ToolsImage,
	}, &ds); err != nil {
		return nil, err
	}
	if _, err := c.Kube.AppsV1().DaemonSets(ns).Create(ctx, &ds, metav1.CreateOptions{}); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("creating the node agent DaemonSet in %s: %w. "+
			"The agent is privileged by design; if Pod Security admission rejected it, either label the "+
			"namespace pod-security.kubernetes.io/enforce=privileged or point -namespace at one that allows it",
			ns, err)
	}

	a := &Agent{c: c, pods: map[string]string{}}
	err := Poll(ctx, PollInterval, PodReadyTimeout, func(ctx context.Context) (bool, error) {
		cur, err := c.Kube.AppsV1().DaemonSets(ns).Get(ctx, agentDaemonSet, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if cur.Status.DesiredNumberScheduled == 0 {
			return false, fmt.Errorf("node agent DaemonSet scheduled on no nodes")
		}
		if cur.Status.NumberReady < cur.Status.DesiredNumberScheduled {
			return false, fmt.Errorf("node agent ready on %d/%d nodes",
				cur.Status.NumberReady, cur.Status.DesiredNumberScheduled)
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("node-level assertions unavailable (Autopilot?): %w", err)
	}
	return a, a.refresh(ctx)
}

func (a *Agent) refresh(ctx context.Context) error {
	pods, err := a.c.Kube.CoreV1().Pods(Namespace).List(ctx, ListOptions("app="+agentDaemonSet))
	if err != nil {
		return err
	}
	fresh := map[string]string{}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodRunning && p.Spec.NodeName != "" {
			fresh[p.Spec.NodeName] = p.Name
		}
	}
	a.pods = fresh
	return nil
}

// Nodes lists the nodes where an agent pod is currently running.
func (a *Agent) Nodes(ctx context.Context) ([]string, error) {
	if err := a.refresh(ctx); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(a.pods))
	for n := range a.pods {
		out = append(out, n)
	}
	return out, nil
}

// Run executes a shell snippet in the host namespaces of a node.
func (a *Agent) Run(ctx context.Context, node, script string) (string, error) {
	pod, ok := a.pods[node]
	if !ok {
		if err := a.refresh(ctx); err != nil {
			return "", err
		}
		if pod, ok = a.pods[node]; !ok {
			return "", fmt.Errorf("no node agent running on %s", node)
		}
	}
	// nsenter into PID 1 puts the command in the host mount, network, IPC, UTS
	// and PID namespaces, which is what makes /proc/mounts and signals real.
	wrapped := fmt.Sprintf("nsenter -t 1 -m -u -i -n -p -- sh -c %s", shellQuote(script))
	r := a.c.Sh(ctx, Namespace, pod, "agent", wrapped)
	if r.Err != nil {
		return r.Combined(), fmt.Errorf("node %s: %w: %s", node, r.Err, r.Combined())
	}
	return r.Stdout, nil
}

// ReadFile reads a file from the node's root filesystem.
func (a *Agent) ReadFile(ctx context.Context, node, path string) (string, error) {
	return a.Run(ctx, node, "cat "+shellQuote(path))
}

// Dmesg reads the kernel ring buffer, where NFS client state actually shows up.
func (a *Agent) Dmesg(ctx context.Context, node string) (string, error) {
	return a.Run(ctx, node, "dmesg -T 2>/dev/null || dmesg 2>/dev/null || true")
}

// Mounts parses /proc/mounts on a node.
func (a *Agent) Mounts(ctx context.Context, node string) ([]MountLine, error) {
	out, err := a.ReadFile(ctx, node, "/proc/mounts")
	if err != nil {
		return nil, err
	}
	return ParseMounts(out), nil
}

// KillProcess sends a signal to every process on a node whose command line
// matches pattern, and reports how many it hit.
func (a *Agent) KillProcess(ctx context.Context, node, pattern, signal string) (int, error) {
	script := fmt.Sprintf(
		`pids=$(ps -eo pid=,args= | grep -F %s | grep -v grep | awk '{print $1}'); `+
			`n=0; for p in $pids; do kill -%s "$p" 2>/dev/null && n=$((n+1)); done; echo "$n"`,
		shellQuote(pattern), signal)
	out, err := a.Run(ctx, node, script)
	if err != nil {
		return 0, err
	}
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(out), "%d", &n)
	return n, nil
}

// WaitProcessAbsent waits until no process matching pattern remains on a node.
func (a *Agent) WaitProcessAbsent(ctx context.Context, node, pattern string, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		out, err := a.Run(ctx, node, fmt.Sprintf(
			`ps -eo pid=,args= | grep -F %s | grep -v grep | wc -l`, shellQuote(pattern)))
		if err != nil {
			return false, err
		}
		return strings.TrimSpace(out) == "0", nil
	})
}

// MountLine is one parsed line of /proc/mounts.
type MountLine struct {
	Device     string
	MountPoint string
	FSType     string
	Options    string
}

// ParseMounts parses the contents of /proc/mounts.
func ParseMounts(contents string) []MountLine {
	var out []MountLine
	for _, line := range strings.Split(contents, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		out = append(out, MountLine{Device: fields[0], MountPoint: fields[1], FSType: fields[2], Options: fields[3]})
	}
	return out
}

// HasOption reports whether an option is present in a mount option list.
func (m MountLine) HasOption(opt string) bool {
	for _, o := range strings.Split(m.Options, ",") {
		if o == opt {
			return true
		}
	}
	return false
}

// OptionValue returns the value of a key=value mount option.
func (m MountLine) OptionValue(key string) (string, bool) {
	for _, o := range strings.Split(m.Options, ",") {
		if k, v, ok := strings.Cut(o, "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func isAlreadyExists(err error) bool { return apierrors.IsAlreadyExists(err) }
