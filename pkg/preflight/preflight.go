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
	storagev1 "k8s.io/api/storage/v1"
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

	// Checks: an RWX-capable StorageClass exists, a PVC on it binds, and two
	// pods on two different nodes both mount it read-write. These are one probe
	// rather than three, because a class that binds on first consumer only
	// binds once the pods exist.
	fx, closeFx := framework.NewSystem(c, e, caps, "PREFLIGHT")
	defer closeFx(context.WithoutCancel(ctx))

	sc, podA, podB, err := findRWXClass(ctx, c, fx, workers[0], workers[1])
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

// classProbeTimeout bounds one candidate StorageClass. It is the bind timeout
// from the plan plus room for two pods to pull an image and start.
const classProbeTimeout = framework.BindTimeout + 90*time.Second

// findRWXClass probes StorageClasses until one gives an RWX volume that two
// pods on two nodes can mount at once. Nothing in the StorageClass API states
// which access modes its volumes will support, so the only honest test is to
// ask for one and use it.
func findRWXClass(ctx context.Context, c *framework.Client, fx *framework.Framework, nodeA, nodeB string) (
	sc string, podA, podB *corev1.Pod, err error) {

	candidates, err := candidateClasses(ctx, c)
	if err != nil {
		return "", nil, nil, err
	}
	if len(candidates) == 0 {
		return "", nil, nil, fmt.Errorf("no RWX-capable StorageClass found: cluster has no StorageClasses")
	}
	var lastErr error
	for _, candidate := range candidates {
		// Bound each candidate: a class that will never work should cost one
		// bind timeout, not a full pod-ready timeout, or probing three classes
		// eats the whole preflight budget.
		probeCtx, cancel := context.WithTimeout(ctx, classProbeTimeout)
		podA, podB, err = probeClass(probeCtx, fx, candidate, nodeA, nodeB)
		cancel()
		if err == nil {
			return candidate, podA, podB, nil
		}
		lastErr = fmt.Errorf("StorageClass %q: %w", candidate, err)
	}
	if framework.Cfg().StorageClass != "" {
		return "", nil, nil, lastErr
	}
	return "", nil, nil, fmt.Errorf("no RWX-capable StorageClass found: %w", lastErr)
}

// probeClass asks one class for an RWX volume and mounts it from two nodes.
func probeClass(ctx context.Context, fx *framework.Framework, class, nodeA, nodeB string) (*corev1.Pod, *corev1.Pod, error) {
	name := "rwx-" + strings.ToLower(class)
	pvc, err := fx.CreatePVC(ctx, framework.PVCSpec{Name: name, StorageClass: class})
	if err != nil {
		return nil, nil, fmt.Errorf("creating an RWX claim: %w", err)
	}
	mode, err := fx.BindingMode(ctx, class)
	if err != nil {
		return nil, nil, err
	}
	// An immediate class must bind on its own; a first-consumer class binds
	// only once a pod referencing it is scheduled, so the pods come first and
	// the bind check follows them.
	if mode != storagev1.VolumeBindingWaitForFirstConsumer {
		if _, err := fx.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
			return nil, nil, fmt.Errorf("RWX PVC did not bind: %w", err)
		}
	}

	podA, podB, mountErr := mountOnTwoNodes(ctx, fx, pvc.Name, nodeA, nodeB)
	if mountErr != nil {
		// Distinguish the two failures the plan names: a claim that never
		// bound, and a bound claim that two nodes cannot mount at once.
		if _, err := fx.WaitPVCBound(ctx, pvc.Name, framework.PollInterval); err != nil {
			return nil, nil, fmt.Errorf("RWX PVC did not bind: %w", err)
		}
		return nil, nil, fmt.Errorf("RWX PVC not simultaneously mountable: %w", mountErr)
	}
	if _, err := fx.WaitPVCBound(ctx, pvc.Name, framework.BindTimeout); err != nil {
		return nil, nil, fmt.Errorf("RWX PVC did not bind: %w", err)
	}
	return podA, podB, nil
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
	specA := framework.PodSpec{Name: "a", Node: nodeA, Mounts: []framework.MountSpec{{Claim: pvc, Path: "/mnt/share"}}}
	specB := framework.PodSpec{Name: "b", Node: nodeB, Mounts: []framework.MountSpec{{Claim: pvc, Path: "/mnt/share"}}}
	podA, err := fx.CreatePod(ctx, specA)
	if err != nil {
		return nil, nil, fmt.Errorf("pod on %s: %w", nodeA, err)
	}
	podB, err := fx.CreatePod(ctx, specB)
	if err != nil {
		return nil, nil, fmt.Errorf("pod on %s: %w", nodeB, err)
	}
	if podA.Spec.NodeName == podB.Spec.NodeName {
		return nil, nil, fmt.Errorf("both preflight pods landed on %s, so nothing was proved about "+
			"simultaneous mounting from two nodes", podA.Spec.NodeName)
	}
	// Read-write on both, simultaneously, verified through the filesystem.
	if _, err := fx.C.MustSh(ctx, framework.Namespace, podA.Name, "main", "echo from-a > /mnt/share/preflight-a.txt && sync"); err != nil {
		return nil, nil, fmt.Errorf("write from %s: %w", podA.Name, err)
	}
	if _, err := fx.C.MustSh(ctx, framework.Namespace, podB.Name, "main", "echo from-b > /mnt/share/preflight-b.txt && sync"); err != nil {
		return nil, nil, fmt.Errorf("write from %s: %w", podB.Name, err)
	}
	if out, err := fx.C.MustSh(ctx, framework.Namespace, podB.Name, "main", "cat /mnt/share/preflight-a.txt"); err != nil || out != "from-a" {
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
