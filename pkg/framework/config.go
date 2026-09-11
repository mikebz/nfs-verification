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
	"path/filepath"
	"regexp"
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
	FioImage   string

	ServerNamespace string
	ServerSelector  string
	ServerProcess   string

	CSIDriver string

	ArtifactsDir string
	RunID        string

	ProfileName  string
	LeaseSeconds int
	GraceSeconds int

	// GraceEnterPattern and GraceExitPattern name the wording a server uses to
	// announce grace, for a server the built-in rule does not cover. Compiled
	// once in FinalizeFlags: a pattern that does not compile is a startup
	// failure, not a run that quietly observes nothing.
	GraceEnterPattern string
	GraceExitPattern  string
	graceEnterRE      *regexp.Regexp
	graceExitRE       *regexp.Regexp

	Platform      string
	GCloudProject string
	GCloudZone    string
	NodePowerCmd  string

	KeepObjects bool
	Verbose     bool

	Delegations      string
	RootSquash       string
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
	// No default. A suite that pulls an image nobody named is a supply chain
	// the operator did not agree to, and the one case that needs fio costs an
	// hour, so skipping it costs nothing on the days nobody has an image.
	flag.StringVar(&cfg.FioImage, "fio-image", "", "image carrying fio, for the mixed soak (DATA-14); the case is skipped when empty")

	flag.StringVar(&cfg.ServerNamespace, "server-namespace", "", "namespace of the NFS server pods; discovered when empty")
	flag.StringVar(&cfg.ServerSelector, "server-selector", "", "label selector for NFS server pods; discovered when empty")
	flag.StringVar(&cfg.ServerProcess, "server-process", "", "process name pattern to signal for in-place kill; discovered when empty")

	flag.StringVar(&cfg.CSIDriver, "csi-driver", "", "CSI driver name; taken from the StorageClass when empty")

	flag.StringVar(&cfg.ArtifactsDir, "artifacts-dir", "artifacts", "root directory for run artifacts")
	flag.StringVar(&cfg.RunID, "run-id", "", "run identifier; defaults to a timestamp")

	flag.StringVar(&cfg.ProfileName, "profile", "", "require this lease/grace profile: tuned or default; empty accepts either")
	flag.IntVar(&cfg.LeaseSeconds, "lease-seconds", 0, "lease seconds, used only when discovery cannot read the live value")
	flag.IntVar(&cfg.GraceSeconds, "grace-seconds", 0, "grace seconds, used only when discovery cannot read the live value")
	flag.StringVar(&cfg.GraceEnterPattern, "grace-enter-pattern", "", "regexp matching the server log line announcing grace entry; the built-in wording rule is used when empty")
	flag.StringVar(&cfg.GraceExitPattern, "grace-exit-pattern", "", "regexp matching the server log line announcing grace exit; set it with -grace-enter-pattern or not at all")

	flag.StringVar(&cfg.Platform, "platform", "auto", "platform for chaos operations: auto, gke, baremetal")
	flag.StringVar(&cfg.GCloudProject, "gcloud-project", "", "GCP project for node stop/start on GKE")
	flag.StringVar(&cfg.GCloudZone, "gcloud-zone", "", "GCP zone for node stop/start on GKE")
	flag.StringVar(&cfg.NodePowerCmd, "node-power-cmd", "", "bare metal power command template, e.g. 'ipmitool -H {{.Node}} power {{.Action}}'")

	flag.BoolVar(&cfg.KeepObjects, "keep-objects", false, "do not delete the objects a case created, for triage")
	flag.BoolVar(&cfg.Verbose, "v-harness", false, "log every harness action")

	flag.StringVar(&cfg.Delegations, "delegations", "auto", "whether delegations are enabled: auto, on, off")
	flag.StringVar(&cfg.RootSquash, "root-squash", "auto", "what the export is configured to do with root: on, off, or auto to record what it does without asserting it")
	flag.StringVar(&cfg.EnvFile, "env-file", "", "reuse a specific environment.json instead of rediscovering")
	flag.BoolVar(&cfg.RefreshPreflight, "refresh-preflight", false, "rerun preflight even when a fresh cached result exists for this context")
	flag.DurationVar(&cfg.PreflightMaxAge, "preflight-max-age", 8*time.Hour, "how long a cached preflight result stays usable")

}

// FinalizeFlags applies post-parse defaults. Call once from TestMain after
// flag.Parse.
func FinalizeFlags() error {
	// go test runs with the working directory set to the test package, while
	// cmd/preflight runs from wherever the operator is. A relative artifacts
	// path would therefore name two different directories, and the preflight
	// cache would never be found. Anchor it to the repository root.
	if !filepath.IsAbs(cfg.ArtifactsDir) {
		if root, err := repoRoot(); err == nil {
			cfg.ArtifactsDir = filepath.Join(root, cfg.ArtifactsDir)
		} else if abs, err := filepath.Abs(cfg.ArtifactsDir); err == nil {
			cfg.ArtifactsDir = abs
		}
	}
	if err := compileGracePatterns(); err != nil {
		return err
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

// compileGracePatterns turns the grace wording flags into matchers. The two are
// set together or not at all: a run that stated only how its server announces
// entry would observe every failover entering grace and never leaving it, which
// is the re-entry loop symptom the OBS cases exist to report honestly.
func compileGracePatterns() error {
	cfg.graceEnterRE, cfg.graceExitRE = nil, nil
	if (cfg.GraceEnterPattern == "") != (cfg.GraceExitPattern == "") {
		return fmt.Errorf("-grace-enter-pattern and -grace-exit-pattern are set together or not at all: " +
			"stating one wording and leaving the other to a rule that does not cover this server " +
			"would report every failover as a grace re-entry loop")
	}
	if cfg.GraceEnterPattern == "" {
		return nil
	}
	enter, err := regexp.Compile(cfg.GraceEnterPattern)
	if err != nil {
		return fmt.Errorf("-grace-enter-pattern %q does not compile: %w", cfg.GraceEnterPattern, err)
	}
	exit, err := regexp.Compile(cfg.GraceExitPattern)
	if err != nil {
		return fmt.Errorf("-grace-exit-pattern %q does not compile: %w", cfg.GraceExitPattern, err)
	}
	cfg.graceEnterRE, cfg.graceExitRE = enter, exit
	return nil
}

// repoRoot walks up from the working directory looking for the module file.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// Cfg returns the parsed configuration.
func Cfg() *Config { return &cfg }
