package framework

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/slo"
)

// nfsPort is the only thing about the server the suite is willing to assume.
const nfsPort int32 = 2049

// ServerPods finds the NFS server pods. Selector and namespace flags win; with
// neither set the suite looks for pods exposing the NFS port or named for a
// known userspace server. No case depends on which implementation answers.
func ServerPods(ctx context.Context, c *Client) ([]corev1.Pod, error) {
	ns := Cfg().ServerNamespace
	sel := Cfg().ServerSelector
	if sel != "" {
		list, err := c.Kube.CoreV1().Pods(ns).List(ctx, ListOptions(sel))
		if err != nil {
			return nil, err
		}
		return runningOnly(list.Items), nil
	}
	list, err := c.Kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var found []corev1.Pod
	for i := range list.Items {
		if looksLikeNFSServer(&list.Items[i]) {
			found = append(found, list.Items[i])
		}
	}
	return runningOnly(found), nil
}

func runningOnly(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning || pods[i].Status.Phase == corev1.PodPending {
			out = append(out, pods[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var serverNameHint = regexp.MustCompile(`(?i)(nfs|ganesha|nfsd)`)

func looksLikeNFSServer(p *corev1.Pod) bool {
	for _, c := range p.Spec.Containers {
		for _, port := range c.Ports {
			if port.ContainerPort == nfsPort {
				return true
			}
		}
		if serverNameHint.MatchString(c.Image) && !strings.Contains(strings.ToLower(c.Image), "csi") {
			return true
		}
	}
	return serverNameHint.MatchString(p.Name) && !strings.Contains(strings.ToLower(p.Name), "csi")
}

// DescribeServers builds the environment record for the server fan-out.
func DescribeServers(ctx context.Context, c *Client) ([]env.ServerInfo, error) {
	pods, err := ServerPods(ctx, c)
	if err != nil {
		return nil, err
	}
	var out []env.ServerInfo
	for i := range pods {
		p := &pods[i]
		info := env.ServerInfo{Namespace: p.Namespace, Pod: p.Name, Node: p.Spec.NodeName}
		for _, ct := range p.Spec.Containers {
			info.Images = append(info.Images, ct.Image)
		}
		info.Exports = exportsOf(p)
		out = append(out, info)
	}
	return out, nil
}

// exportsOf reports the claims a server pod is backed by, which is the
// export-to-PVC mapping preflight records.
func exportsOf(p *corev1.Pod) []string {
	var out []string
	for _, v := range p.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out = append(out, v.PersistentVolumeClaim.ClaimName)
		}
	}
	return out
}

// WaitServersReady waits until at least one server pod is Running and Ready.
func WaitServersReady(ctx context.Context, c *Client, timeout time.Duration) error {
	return Poll(ctx, PollInterval, timeout, func(ctx context.Context) (bool, error) {
		pods, err := ServerPods(ctx, c)
		if err != nil {
			return false, err
		}
		for i := range pods {
			if podReady(&pods[i]) {
				return true, nil
			}
		}
		return false, fmt.Errorf("no server pod ready (%d known)", len(pods))
	})
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, cs := range p.Status.ContainerStatuses {
		if !cs.Ready {
			return false
		}
	}
	return len(p.Status.ContainerStatuses) > 0
}

// ServerRestartCount sums container restarts across server pods. A chaos case
// that expects no restart asserts on this rather than on log scraping.
func ServerRestartCount(ctx context.Context, c *Client) (int32, error) {
	pods, err := ServerPods(ctx, c)
	if err != nil {
		return 0, err
	}
	var total int32
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			total += cs.RestartCount
		}
	}
	return total, nil
}

var (
	leasePattern = regexp.MustCompile(`(?i)(lease[_-]?(?:lifetime|time|seconds|period)?)\s*[=: ]\s*"?(\d+)`)
	gracePattern = regexp.MustCompile(`(?i)(grace[_-]?(?:period|time|seconds)?)\s*[=: ]\s*"?(\d+)`)
)

// DiscoverTiming reads the live lease and grace values. Sources are tried in
// order of trustworthiness: explicit flags, container environment, container
// args, then any ConfigMap the server pod mounts. Every timing assertion in the
// suite depends on these, so an undiscoverable value is a preflight failure,
// not a default.
func DiscoverTiming(ctx context.Context, c *Client) (env.Timing, error) {
	if Cfg().LeaseSeconds > 0 && Cfg().GraceSeconds > 0 {
		return finishTiming(Cfg().LeaseSeconds, Cfg().GraceSeconds, "flags")
	}
	pods, err := ServerPods(ctx, c)
	if err != nil {
		return env.Timing{}, err
	}
	for i := range pods {
		p := &pods[i]
		var blob strings.Builder
		for _, ct := range p.Spec.Containers {
			for _, e := range ct.Env {
				fmt.Fprintf(&blob, "%s=%s\n", e.Name, e.Value)
			}
			blob.WriteString(strings.Join(ct.Args, "\n") + "\n")
			blob.WriteString(strings.Join(ct.Command, "\n") + "\n")
		}
		if lease, grace, ok := scanTiming(blob.String()); ok {
			return finishTiming(lease, grace, "server pod spec "+p.Name)
		}
		for _, v := range p.Spec.Volumes {
			if v.ConfigMap == nil {
				continue
			}
			cm, err := c.Kube.CoreV1().ConfigMaps(p.Namespace).Get(ctx, v.ConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			var cmBlob strings.Builder
			for _, val := range cm.Data {
				cmBlob.WriteString(val + "\n")
			}
			if lease, grace, ok := scanTiming(cmBlob.String()); ok {
				return finishTiming(lease, grace, "configmap "+cm.Namespace+"/"+cm.Name)
			}
		}
	}
	return env.Timing{}, fmt.Errorf("lease and grace values are unset or undiscoverable: " +
		"every timing assertion depends on them; set them on the server or pass -lease-seconds and -grace-seconds")
}

func scanTiming(blob string) (lease, grace int, ok bool) {
	lm := leasePattern.FindStringSubmatch(blob)
	gm := gracePattern.FindStringSubmatch(blob)
	if lm == nil || gm == nil {
		return 0, 0, false
	}
	l, err1 := strconv.Atoi(lm[2])
	g, err2 := strconv.Atoi(gm[2])
	if err1 != nil || err2 != nil || l == 0 || g == 0 {
		return 0, 0, false
	}
	return l, g, true
}

func finishTiming(lease, grace int, via string) (env.Timing, error) {
	t := env.Timing{LeaseSeconds: lease, GraceSeconds: grace, DiscoveredVia: via}
	p, ok := slo.Match(time.Duration(lease)*time.Second, time.Duration(grace)*time.Second)
	if !ok {
		return t, fmt.Errorf("lease=%ds grace=%ds matches neither the tuned (%s/%s) nor the default (%s/%s) profile; "+
			"a third value silently invalidates every timing assertion",
			lease, grace, slo.Tuned.Lease, slo.Tuned.Grace, slo.Default.Lease, slo.Default.Grace)
	}
	t.Profile = p.Name
	return t, nil
}

// Profile returns the pinned lease/grace profile the run is measuring against.
func Profile() (slo.Profile, error) {
	e := SuiteEnv()
	if e == nil {
		return slo.Profile{}, fmt.Errorf("environment not discovered")
	}
	p, ok := slo.ByName(e.Timing.Profile)
	if !ok {
		return slo.Profile{}, fmt.Errorf("environment carries unknown profile %q", e.Timing.Profile)
	}
	return p, nil
}
