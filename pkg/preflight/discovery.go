package preflight

import (
	"context"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/mikebz/nfs-verification/pkg/env"
	"github.com/mikebz/nfs-verification/pkg/framework"
)

// sharesOneServer reports whether more than one export is served by one pod.
// Only then does the noisy-neighbour case (SCALE-06) mean anything.
func sharesOneServer(e *env.Environment) bool {
	for _, s := range e.Servers {
		if len(s.Exports) > 1 {
			return true
		}
	}
	// A single server pod for the whole cluster is the extreme case of sharing.
	return e.FanOut == 1
}

// csiImages finds the images of the pods implementing the CSI driver, so that
// version skew is read rather than declared.
func csiImages(ctx context.Context, c *framework.Client, driver string) []string {
	if driver == "" {
		return nil
	}
	pods, err := c.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	token := driverToken(driver)
	seen := map[string]bool{}
	var out []string
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.Contains(strings.ToLower(p.Name), token) && !mentionsDriver(p, driver) {
			continue
		}
		for _, ct := range p.Spec.Containers {
			if seen[ct.Image] {
				continue
			}
			seen[ct.Image] = true
			out = append(out, ct.Image)
		}
	}
	return out
}

// driverToken reduces "nfs.csi.k8s.io" to "nfs", which is what pod names carry.
func driverToken(driver string) string {
	parts := strings.Split(driver, ".")
	if len(parts) == 0 {
		return driver
	}
	return strings.ToLower(parts[0])
}

func mentionsDriver(p *corev1.Pod, driver string) bool {
	for _, ct := range p.Spec.Containers {
		for _, a := range ct.Args {
			if strings.Contains(a, driver) {
				return true
			}
		}
		for _, e := range ct.Env {
			if strings.Contains(e.Value, driver) {
				return true
			}
		}
	}
	return false
}

// independentlyVersioned reports whether the server and the CSI driver come
// from different release trains. It gates the SKEW cases, and it is a heuristic
// on image references, so the answer is recorded next to the images it used.
func independentlyVersioned(e *env.Environment) bool {
	serverRepos := map[string]string{}
	for _, s := range e.Servers {
		for _, img := range s.Images {
			repo, tag := splitImage(img)
			serverRepos[repo] = tag
		}
	}
	if len(serverRepos) == 0 || len(e.CSIDriverImages) == 0 {
		return false
	}
	for _, img := range e.CSIDriverImages {
		repo, tag := splitImage(img)
		if serverTag, ok := serverRepos[repo]; ok && serverTag == tag {
			// Same repository and tag: one release train.
			return false
		}
	}
	return true
}

func splitImage(image string) (repo, tag string) {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i], image[i+1:]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, "latest"
}

// mountPropagation records the propagation mode on the CSI node plugin. Not
// gated: if it were wrong nothing would mount, and the simultaneous-mount check
// already catches that.
func mountPropagation(ctx context.Context, c *framework.Client, driver string) string {
	if driver == "" {
		return ""
	}
	pods, err := c.Kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	token := driverToken(driver)
	for i := range pods.Items {
		p := &pods.Items[i]
		if !strings.Contains(strings.ToLower(p.Name), token) {
			continue
		}
		for _, ct := range p.Spec.Containers {
			for _, vm := range ct.VolumeMounts {
				if vm.MountPropagation != nil {
					return string(*vm.MountPropagation)
				}
			}
		}
	}
	return ""
}

// recoveryBackend records whether the server's recovery state lives on
// persistent storage or on ephemeral pod storage. Recorded for triage only:
// lock survival is measured empirically by CHAOS-02 and CHAOS-06, never
// inferred from this value. It exists so that a reclaim failure is diagnosed in
// one minute instead of one day.
func recoveryBackend(ctx context.Context, c *framework.Client, e *env.Environment) string {
	pods, err := framework.ServerPods(ctx, c)
	if err != nil || len(pods) == 0 {
		return "unknown"
	}
	var kinds []string
	for _, v := range pods[0].Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			kinds = append(kinds, "pvc:"+v.PersistentVolumeClaim.ClaimName)
		case v.EmptyDir != nil:
			kinds = append(kinds, "emptyDir:"+v.Name)
		case v.HostPath != nil:
			kinds = append(kinds, "hostPath:"+v.HostPath.Path)
		}
	}
	if len(kinds) == 0 {
		return "container filesystem (ephemeral)"
	}
	return strings.Join(kinds, ",")
}

// ipFamilies reads the families in use from the kubernetes Service, which is
// the one Service every cluster has.
func ipFamilies(ctx context.Context, c *framework.Client) []string {
	svc, err := c.Kube.CoreV1().Services("default").Get(ctx, "kubernetes", metav1.GetOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, f := range svc.Spec.IPFamilies {
		out = append(out, string(f))
	}
	return out
}

var delegationOn = regexp.MustCompile(`(?i)delegations?\s*[=: ]\s*"?(true|on|yes|enabled|1)\b`)
var delegationOff = regexp.MustCompile(`(?i)delegations?\s*[=: ]\s*"?(false|off|no|disabled|0)\b`)

// delegationsEnabled gates CHAOS-18. Disabled is a common default, so an
// undiscoverable answer is treated as disabled and the case skips rather than
// failing on a feature that is not turned on.
func delegationsEnabled(ctx context.Context, c *framework.Client, e *env.Environment) bool {
	switch strings.ToLower(framework.Cfg().Delegations) {
	case "on", "true", "enabled":
		return true
	case "off", "false", "disabled":
		return false
	}
	blob := serverConfigBlob(ctx, c)
	if delegationOff.MatchString(blob) {
		return false
	}
	if delegationOn.MatchString(blob) {
		return true
	}
	e.AddNote("delegation configuration not discoverable; CHAOS-18 will skip. Pass -delegations=on to force it")
	return false
}

// serverConfigBlob concatenates everything the server pod spec and its mounted
// ConfigMaps say, for regex discovery of settings the suite must not assume.
func serverConfigBlob(ctx context.Context, c *framework.Client) string {
	pods, err := framework.ServerPods(ctx, c)
	if err != nil {
		return ""
	}
	var sb strings.Builder
	for i := range pods {
		p := &pods[i]
		for _, ct := range p.Spec.Containers {
			sb.WriteString(strings.Join(ct.Args, "\n"))
			sb.WriteString(strings.Join(ct.Command, "\n"))
			for _, e := range ct.Env {
				sb.WriteString(e.Name + "=" + e.Value + "\n")
			}
		}
		for _, v := range p.Spec.Volumes {
			if v.ConfigMap == nil {
				continue
			}
			cm, err := c.Kube.CoreV1().ConfigMaps(p.Namespace).Get(ctx, v.ConfigMap.Name, metav1.GetOptions{})
			if err != nil {
				continue
			}
			for _, val := range cm.Data {
				sb.WriteString(val + "\n")
			}
		}
	}
	return sb.String()
}

// nodeLossConfig records the two platform defaults that stand between the 90s
// ungraceful node loss SLO and the roughly 11 minute worst case: the pod's own
// tolerationSeconds, and whether the out-of-service taint path is available.
func nodeLossConfig(ctx context.Context, c *framework.Client, e *env.Environment) env.NodeLossConfig {
	cfg := env.NodeLossConfig{}
	// The out-of-service taint went GA in 1.28; below that the force-detach
	// path this suite relies on is not the supported one.
	cfg.OutOfServiceTaintUsable = atLeast128(e.KubernetesVersion)
	pods, err := framework.ServerPods(ctx, c)
	if err != nil || len(pods) == 0 {
		return cfg
	}
	for _, t := range pods[0].Spec.Tolerations {
		switch t.Key {
		case corev1.TaintNodeNotReady:
			cfg.NotReadyTolerationSet = t.TolerationSeconds != nil
			if t.TolerationSeconds != nil {
				cfg.ServerTolerationSeconds = t.TolerationSeconds
			}
		case corev1.TaintNodeUnreachable:
			cfg.UnreachableTolerationSet = t.TolerationSeconds != nil
		}
	}
	if !cfg.NotReadyTolerationSet || !cfg.UnreachableTolerationSet {
		e.AddNote("server workload does not set tolerationSeconds on the not-ready and unreachable taints; " +
			"Kubernetes adds 300s by default, so CHAOS-03 is blocked on cluster configuration rather than failed")
	}
	return cfg
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)`)

func atLeast128(gitVersion string) bool {
	m := versionRE.FindStringSubmatch(gitVersion)
	if m == nil {
		return false
	}
	major, minor := atoi(m[1]), atoi(m[2])
	return major > 1 || (major == 1 && minor >= 28)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// nodeStopConfigured reports whether a platform implementation for hard node
// stop is available. Capability, never platform name.
func nodeStopConfigured() bool {
	if framework.Cfg().NodePowerCmd != "" {
		return true
	}
	return framework.Cfg().GCloudProject != "" && framework.Cfg().GCloudZone != ""
}
