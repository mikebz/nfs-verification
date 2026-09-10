// Package framework is the Kubernetes-side harness. It runs on the operator's
// workstation, outside the cluster: it creates PVCs, pods and DaemonSets in an
// existing namespace, injects faults, collects artifacts and asserts. It
// creates no namespaces of its own. It is a Kubernetes client, never an NFS
// client: all I/O comes from in-cluster pods.
package framework

import (
	"flag"
	"os"
	"time"
)

// Config is every input the harness takes. Anything discoverable is discovered
// at preflight instead of being declared here.
type Config struct {
	Kubeconfig   string
	Context      string
	StorageClass string
	PVCSize      string

	ToolsImage string

	ServerNamespace string
	ServerSelector  string
	ServerProcess   string

	CSIDriver string

	ArtifactsDir string
	RunID        string

	ProfileName  string
	LeaseSeconds int
	GraceSeconds int

	Platform      string
	GCloudProject string
	GCloudZone    string
	NodePowerCmd  string

	KeepObjects bool
	Verbose     bool

	Delegations      string
	EnvFile          string
	RefreshPreflight bool
	PreflightMaxAge  time.Duration
}

var cfg Config

func init() {
	flag.StringVar(&cfg.Kubeconfig, "kubeconfig", os.Getenv("KUBECONFIG"), "path to kubeconfig; defaults to $KUBECONFIG then ~/.kube/config")
	flag.StringVar(&cfg.Context, "context", "", "kubeconfig context to use")
	flag.StringVar(&cfg.StorageClass, "storage-class", "", "RWX-capable StorageClass; discovered when empty")
	flag.StringVar(&cfg.PVCSize, "pvc-size", "1Gi", "size for test PVCs; small on purpose, since a backing volume that cannot satisfy it fails every case")

	flag.StringVar(&cfg.ToolsImage, "tools-image", "alpine:3.20", "image with dd, sha256sum, flock, stat")

	flag.StringVar(&cfg.ServerNamespace, "server-namespace", "", "namespace of the NFS server pods; discovered when empty")
	flag.StringVar(&cfg.ServerSelector, "server-selector", "", "label selector for NFS server pods; discovered when empty")
	flag.StringVar(&cfg.ServerProcess, "server-process", "", "process name pattern to signal for in-place kill; discovered when empty")

	flag.StringVar(&cfg.CSIDriver, "csi-driver", "", "CSI driver name; taken from the StorageClass when empty")

	flag.StringVar(&cfg.ArtifactsDir, "artifacts-dir", "artifacts", "root directory for run artifacts")
	flag.StringVar(&cfg.RunID, "run-id", "", "run identifier; defaults to a timestamp")

	flag.StringVar(&cfg.ProfileName, "profile", "", "require this lease/grace profile: tuned or default; empty accepts either")
	flag.IntVar(&cfg.LeaseSeconds, "lease-seconds", 0, "lease seconds, used only when discovery cannot read the live value")
	flag.IntVar(&cfg.GraceSeconds, "grace-seconds", 0, "grace seconds, used only when discovery cannot read the live value")

	flag.StringVar(&cfg.Platform, "platform", "auto", "platform for chaos operations: auto, gke, baremetal")
	flag.StringVar(&cfg.GCloudProject, "gcloud-project", "", "GCP project for node stop/start on GKE")
	flag.StringVar(&cfg.GCloudZone, "gcloud-zone", "", "GCP zone for node stop/start on GKE")
	flag.StringVar(&cfg.NodePowerCmd, "node-power-cmd", "", "bare metal power command template, e.g. 'ipmitool -H {{.Node}} power {{.Action}}'")

	flag.BoolVar(&cfg.KeepObjects, "keep-objects", false, "do not delete the objects a case created, for triage")
	flag.BoolVar(&cfg.Verbose, "v-harness", false, "log every harness action")

	flag.StringVar(&cfg.Delegations, "delegations", "auto", "whether delegations are enabled: auto, on, off")
	flag.StringVar(&cfg.EnvFile, "env-file", "", "reuse a specific environment.json instead of rediscovering")
	flag.BoolVar(&cfg.RefreshPreflight, "refresh-preflight", false, "rerun preflight even when a fresh cached result exists for this context")
	flag.DurationVar(&cfg.PreflightMaxAge, "preflight-max-age", 8*time.Hour, "how long a cached preflight result stays usable")

}

// FinalizeFlags applies post-parse defaults. Call once from TestMain after
// flag.Parse.
func FinalizeFlags() error {
	if cfg.RunID == "" {
		cfg.RunID = time.Now().UTC().Format("20060102-150405")
	}
	if cfg.Kubeconfig == "" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := home + "/.kube/config"
			if _, err := os.Stat(candidate); err == nil {
				cfg.Kubeconfig = candidate
			}
		}
	}
	return nil
}

// Cfg returns the parsed configuration.
func Cfg() *Config { return &cfg }
