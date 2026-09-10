// Package framework is the Kubernetes-side harness. It runs on the operator's
// workstation, outside the cluster: it creates PVCs, pods and DaemonSets in an
// existing namespace, injects faults, collects artifacts and asserts. It
// creates no namespaces of its own. It is a Kubernetes client, never an NFS
// client: all I/O comes from in-cluster pods.
package framework

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

// Gate selects which cases run. Cases declare their gate and skip otherwise.
type Gate string

const (
	GatePresubmit Gate = "presubmit"
	GateNightly   Gate = "nightly"
	GateSoak      Gate = "soak"
	GateManual    Gate = "manual"
	GateAll       Gate = "all"
)

// rank orders gates so that a higher gate includes the lower ones.
var gateRank = map[Gate]int{GatePresubmit: 1, GateNightly: 2, GateSoak: 3, GateManual: 4}

// Includes reports whether the selected gate runs cases declared at want.
func (g Gate) Includes(want Gate) bool {
	if g == GateAll {
		return true
	}
	if g == GateSoak {
		// Soak is a distinct budget: it runs only W cases.
		return want == GateSoak
	}
	if g == GateManual {
		return want == GateManual
	}
	return gateRank[g] >= gateRank[want] && want != GateManual && want != GateSoak
}

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

	Gate         Gate
	ArtifactsDir string
	RunID        string

	ProfileName  string
	LeaseSeconds int
	GraceSeconds int

	Platform      string
	GCloudProject string
	GCloudZone    string
	NodePowerCmd  string

	Namespace   string
	KeepObjects bool
	Verbose     bool

	Delegations string
	EnvFile     string
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

	gate := flag.String("gate", string(GatePresubmit), "which cases to run: presubmit, nightly, soak, manual, all")
	flag.StringVar(&cfg.ArtifactsDir, "artifacts-dir", "artifacts", "root directory for run artifacts")
	flag.StringVar(&cfg.RunID, "run-id", "", "run identifier; defaults to a timestamp")

	flag.StringVar(&cfg.ProfileName, "profile", "", "require this lease/grace profile: tuned or default; empty accepts either")
	flag.IntVar(&cfg.LeaseSeconds, "lease-seconds", 0, "lease seconds, used only when discovery cannot read the live value")
	flag.IntVar(&cfg.GraceSeconds, "grace-seconds", 0, "grace seconds, used only when discovery cannot read the live value")

	flag.StringVar(&cfg.Platform, "platform", "auto", "platform for chaos operations: auto, gke, baremetal")
	flag.StringVar(&cfg.GCloudProject, "gcloud-project", "", "GCP project for node stop/start on GKE")
	flag.StringVar(&cfg.GCloudZone, "gcloud-zone", "", "GCP zone for node stop/start on GKE")
	flag.StringVar(&cfg.NodePowerCmd, "node-power-cmd", "", "bare metal power command template, e.g. 'ipmitool -H {{.Node}} power {{.Action}}'")

	flag.StringVar(&cfg.Namespace, "namespace", "default", "namespace for the test workloads; the suite creates no namespaces of its own")
	flag.BoolVar(&cfg.KeepObjects, "keep-objects", false, "do not delete the objects a case created, for triage")
	flag.BoolVar(&cfg.Verbose, "v-harness", false, "log every harness action")

	flag.StringVar(&cfg.Delegations, "delegations", "auto", "whether delegations are enabled: auto, on, off")
	flag.StringVar(&cfg.EnvFile, "env-file", "", "reuse a previously written environment.json instead of rediscovering")

	// Gate is parsed lazily because flag.Parse happens in TestMain.
	gateHolder = gate
}

var gateHolder *string

// FinalizeFlags applies post-parse defaults. Call once from TestMain after
// flag.Parse.
func FinalizeFlags() error {
	if gateHolder != nil {
		cfg.Gate = Gate(strings.ToLower(*gateHolder))
	}
	switch cfg.Gate {
	case GatePresubmit, GateNightly, GateSoak, GateManual, GateAll:
	default:
		return fmt.Errorf("unknown -gate %q", cfg.Gate)
	}
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
