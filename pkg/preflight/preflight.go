// Package preflight implements Section 0 of the plan. The suite refuses to run
// unless every check here passes, and every check records what it found so that
// no case has to be told version numbers by a human.
package preflight

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/framework"
)

// Result is what the suite needs before any case runs.
type Result struct {
	Env  *env.Environment
	Caps framework.Capabilities
}

// Run executes the preflight checks in order and returns the environment record.
// It exits at the first hard failure with a specific reason: no test should
// execute after a preflight failure.
func Run(ctx context.Context) (*Result, error) {
	c, err := framework.NewClient()
	if err != nil {
		return nil, fmt.Errorf("connecting to cluster: %w", err)
	}
	e := &env.Environment{RunID: framework.Cfg().RunID, Timestamp: time.Now().UTC(), Platform: framework.Cfg().Platform}
	caps := framework.Capabilities{}

	if e.KubernetesVersion, err = c.ServerVersion(); err != nil {
		return nil, fmt.Errorf("reading Kubernetes version: %w", err)
	}
	if err := describeNodes(ctx, c, e); err != nil {
		return nil, err
	}

	// Check: at least two schedulable workers.
	workers, err := framework.WorkerNodes(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("listing worker nodes: %w", err)
	}
	if len(workers) < 2 {
		return nil, fmt.Errorf("insufficient nodes for cross-node cases: %d schedulable workers", len(workers))
	}
	caps.MultiNode = true

	// Check: a privileged DaemonSet can be scheduled. Its absence is a hard
	// failure, not a silent skip, because it is the only way to see the node.
	agent, err := framework.NodeAgent(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("node-level assertions unavailable (Autopilot?): %w", err)
	}
	caps.NodeAgent = true

	// Check: an RWX-capable StorageClass exists and a PVC on it binds.
	sc, pvcName, fx, closeFx, err := findRWXClass(ctx, c, e, caps)
	if closeFx != nil {
		defer closeFx(context.WithoutCancel(ctx))
	}
	if err != nil {
		return nil, err
	}
	e.StorageClass = sc
	if e.CSIDriver, err = provisionerOf(ctx, c, sc); err != nil {
		return nil, err
	}
	caps.CanExpand, _ = c.SupportsExpansion(ctx, sc)
	caps.CanSnapshot = c.HasAPI(schema.GroupVersionResource{
		Group: "snapshot.storage.k8s.io", Version: "v1", Resource: "volumesnapshots"})

	// Check: two pods on two different nodes both mount it read-write.
	podA, podB, err := mountOnTwoNodes(ctx, fx, pvcName, workers[0], workers[1])
	if err != nil {
		return nil, fmt.Errorf("RWX PVC not simultaneously mountable: %w", err)
	}

	// Check: the mount is NFS and negotiates 4.1. Read from /proc/mounts on the
	// node, which is what the driver actually set, not what was requested.
	mounts, err := collectMounts(ctx, agent, fx, []*corev1.Pod{podA, podB})
	if err != nil {
		return nil, err
	}
	e.Mounts = mounts
	if err := assertNFS41(mounts); err != nil {
		return nil, err
	}
	e.NFSVersion = "4.1"
	e.HardMount = true

	// Everything past here is recorded, not required, except the timing values.
	if e.Servers, err = framework.DescribeServers(ctx, c); err != nil {
		return nil, fmt.Errorf("discovering NFS server pods: %w", err)
	}
	e.FanOut = len(e.Servers)
	caps.SharedServer = e.FanOut > 0 && sharesOneServer(e)
	if e.FanOut == 0 {
		e.AddNote("no NFS server pods discovered: set -server-namespace and -server-selector, " +
			"otherwise every CHAOS case that kills the server will skip")
	}

	e.CSIDriverImages = csiImages(ctx, c, e.CSIDriver)
	e.IndependentlyVersioned = independentlyVersioned(e)
	caps.IndependentVersions = e.IndependentlyVersioned

	e.MountPropagation = mountPropagation(ctx, c, e.CSIDriver)
	e.RecoveryStateBackend = recoveryBackend(ctx, c, e)
	e.IPFamilies = ipFamilies(ctx, c)
	caps.DualStack = len(e.IPFamilies) > 1

	e.DelegationsOn = delegationsEnabled(ctx, c, e)
	caps.DelegationsEnabled = e.DelegationsOn

	e.NodeLoss = nodeLossConfig(ctx, c, e)
	caps.NodeLossConfigured = e.NodeLoss.NotReadyTolerationSet &&
		e.NodeLoss.UnreachableTolerationSet && e.NodeLoss.OutOfServiceTaintUsable

	// Lease and grace must be pinned to one of the two profiles. An unset or
	// off-profile value fails preflight, because every timing assertion depends
	// on them.
	timing, err := framework.DiscoverTiming(ctx, c)
	if err != nil {
		return nil, err
	}
	e.Timing = timing
	if want := framework.Cfg().ProfileName; want != "" && want != timing.Profile {
		return nil, fmt.Errorf("cluster is on the %q lease/grace profile but -profile=%s was requested",
			timing.Profile, want)
	}

	caps.CanNetworkPolicy = c.HasAPI(schema.GroupVersionResource{
		Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"})
	if caps.CanNetworkPolicy {
		e.AddNote("NetworkPolicy API is served; enforcement by the CNI is verified per case, not here")
	}
	caps.CanStopNode = nodeStopConfigured()

	e.Capabilities = caps.AsMap()
	if _, err := e.Write(framework.RunDir()); err != nil {
		return nil, fmt.Errorf("writing environment.json: %w", err)
	}
	return &Result{Env: e, Caps: caps}, nil
}

func describeNodes(ctx context.Context, c *framework.Client, e *env.Environment) error {
	nodes, err := c.Kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		e.Nodes = append(e.Nodes, env.NodeInfo{
			Name:             n.Name,
			KubeletVersion:   n.Status.NodeInfo.KubeletVersion,
			KernelVersion:    n.Status.NodeInfo.KernelVersion,
			OSImage:          n.Status.NodeInfo.OSImage,
			ContainerRuntime: n.Status.NodeInfo.ContainerRuntimeVersion,
			Schedulable:      !n.Spec.Unschedulable,
			Labels:           map[string]string{"topology": n.Labels["topology.kubernetes.io/zone"]},
		})
	}
	return nil
}

// findRWXClass probes StorageClasses until one binds an RWX claim. Nothing in
// the StorageClass API states which access modes its volumes will support, so
// the only honest test is to ask for one.
func findRWXClass(ctx context.Context, c *framework.Client, e *env.Environment, caps framework.Capabilities) (
	sc string, pvcName string, fx *framework.Framework, closeFx func(context.Context), err error) {

	candidates, err := candidateClasses(ctx, c)
	if err != nil {
		return "", "", nil, nil, err
	}
	if len(candidates) == 0 {
		return "", "", nil, nil, fmt.Errorf("no RWX-capable StorageClass found: cluster has no StorageClasses")
	}
	fx, closeFx, err = framework.NewSystem(ctx, c, e, caps, "PREFLIGHT")
	if err != nil {
		return "", "", nil, nil, fmt.Errorf("creating preflight namespace: %w", err)
	}
	var lastErr error
	for _, candidate := range candidates {
		name := "preflight-rwx-" + strings.ToLower(candidate)
		if len(name) > 63 {
			name = name[:63]
		}
		if _, err := fx.CreatePVC(ctx, framework.PVCSpec{Name: name, StorageClass: candidate}); err != nil {
			lastErr = err
			continue
		}
		if _, err := fx.WaitPVCBound(ctx, name, framework.BindTimeout); err != nil {
			lastErr = fmt.Errorf("RWX PVC did not bind on StorageClass %q: %w", candidate, err)
			continue
		}
		return candidate, name, fx, closeFx, nil
	}
	if framework.Cfg().StorageClass != "" {
		return "", "", fx, closeFx, fmt.Errorf("RWX PVC did not bind: %w", lastErr)
	}
	return "", "", fx, closeFx, fmt.Errorf("no RWX-capable StorageClass found: %v", lastErr)
}

func candidateClasses(ctx context.Context, c *framework.Client) ([]string, error) {
	if s := framework.Cfg().StorageClass; s != "" {
		return []string{s}, nil
	}
	classes, err := c.StorageClasses(ctx)
	if err != nil {
		return nil, err
	}
	var def, rest []string
	for i := range classes {
		sc := &classes[i]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true" {
			def = append(def, sc.Name)
			continue
		}
		rest = append(rest, sc.Name)
	}
	return append(def, rest...), nil
}

func provisionerOf(ctx context.Context, c *framework.Client, name string) (string, error) {
	sc, err := c.Kube.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return sc.Provisioner, nil
}

func mountOnTwoNodes(ctx context.Context, fx *framework.Framework, pvc, nodeA, nodeB string) (*corev1.Pod, *corev1.Pod, error) {
	specA := framework.PodSpec{Name: "preflight-a", Node: nodeA, Mounts: []framework.MountSpec{{Claim: pvc, Path: "/mnt/share"}}}
	specB := framework.PodSpec{Name: "preflight-b", Node: nodeB, Mounts: []framework.MountSpec{{Claim: pvc, Path: "/mnt/share"}}}
	podA, err := fx.CreatePod(ctx, specA)
	if err != nil {
		return nil, nil, fmt.Errorf("pod on %s: %w", nodeA, err)
	}
	podB, err := fx.CreatePod(ctx, specB)
	if err != nil {
		return nil, nil, fmt.Errorf("pod on %s: %w", nodeB, err)
	}
	// Read-write on both, simultaneously, verified through the filesystem.
	if _, err := fx.C.MustSh(ctx, fx.Namespace, podA.Name, "main", "echo from-a > /mnt/share/preflight-a.txt && sync"); err != nil {
		return nil, nil, fmt.Errorf("write from %s: %w", podA.Name, err)
	}
	if _, err := fx.C.MustSh(ctx, fx.Namespace, podB.Name, "main", "echo from-b > /mnt/share/preflight-b.txt && sync"); err != nil {
		return nil, nil, fmt.Errorf("write from %s: %w", podB.Name, err)
	}
	if out, err := fx.C.MustSh(ctx, fx.Namespace, podB.Name, "main", "cat /mnt/share/preflight-a.txt"); err != nil || out != "from-a" {
		return nil, nil, fmt.Errorf("pod on %s cannot read what pod on %s wrote: out=%q err=%v", nodeB, nodeA, out, err)
	}
	return podA, podB, nil
}

// collectMounts finds the NFS mounts backing the preflight pods by matching the
// pod UID in the kubelet mount path.
func collectMounts(ctx context.Context, agent *framework.Agent, fx *framework.Framework, pods []*corev1.Pod) ([]env.MountInfo, error) {
	var out []env.MountInfo
	for _, p := range pods {
		lines, err := agent.Mounts(ctx, p.Spec.NodeName)
		if err != nil {
			return nil, fmt.Errorf("reading /proc/mounts on %s: %w", p.Spec.NodeName, err)
		}
		for _, m := range lines {
			if !strings.Contains(m.MountPoint, string(p.UID)) {
				continue
			}
			if !strings.HasPrefix(m.FSType, "nfs") {
				continue
			}
			out = append(out, env.MountInfo{
				Node: p.Spec.NodeName, Device: m.Device, MountPoint: m.MountPoint,
				FSType: m.FSType, Options: m.Options,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("mount is not nfs4 vers=4.1: no NFS mount found on any node for the preflight pods")
	}
	return out, nil
}

func assertNFS41(mounts []env.MountInfo) error {
	for _, m := range mounts {
		line := framework.MountLine{Device: m.Device, MountPoint: m.MountPoint, FSType: m.FSType, Options: m.Options}
		if m.FSType != "nfs4" {
			return fmt.Errorf("mount is not nfs4 vers=4.1: fstype is %q on %s", m.FSType, m.Node)
		}
		vers, ok := line.OptionValue("vers")
		if !ok {
			vers, ok = line.OptionValue("nfsvers")
		}
		if !ok || vers != "4.1" {
			return fmt.Errorf("mount is not nfs4 vers=4.1: options on %s are %q", m.Node, m.Options)
		}
		if line.HasOption("soft") {
			return fmt.Errorf("mount is soft on %s: every chaos assertion in this suite assumes a hard mount that blocks rather than returning EIO", m.Node)
		}
	}
	return nil
}
