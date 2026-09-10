// Package env holds the environment record that preflight discovers and every
// failure report carries. Discovery over declaration: a plan that requires
// humans to type version numbers correctly will be wrong within one sprint.
package env

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// NodeInfo is what preflight records per node.
type NodeInfo struct {
	Name             string            `json:"name"`
	KubeletVersion   string            `json:"kubeletVersion"`
	KernelVersion    string            `json:"kernelVersion"`
	OSImage          string            `json:"osImage"`
	ContainerRuntime string            `json:"containerRuntime"`
	Schedulable      bool              `json:"schedulable"`
	Labels           map[string]string `json:"labels,omitempty"`
}

// ServerInfo describes one NFS server pod.
type ServerInfo struct {
	Namespace string   `json:"namespace"`
	Pod       string   `json:"pod"`
	Node      string   `json:"node"`
	Images    []string `json:"images"`
	// Version as the server reports it, when it exposes one. Empty is normal.
	ReportedVersion string `json:"reportedVersion,omitempty"`
	// Exports is the export-to-PVC mapping this pod serves, when discoverable.
	Exports []string `json:"exports,omitempty"`
}

// MountInfo is one line of /proc/mounts on a node, for an NFS mount the suite
// created. Options are what the driver actually set, not what was requested.
type MountInfo struct {
	Node       string `json:"node"`
	Device     string `json:"device"`
	MountPoint string `json:"mountPoint"`
	FSType     string `json:"fsType"`
	Options    string `json:"options"`
}

// Timing is the lease and grace configuration in force, plus the profile it
// matched. An unmatched profile is a preflight failure.
type Timing struct {
	LeaseSeconds  int    `json:"leaseSeconds"`
	GraceSeconds  int    `json:"graceSeconds"`
	Profile       string `json:"profile"`
	DiscoveredVia string `json:"discoveredVia"`
}

// NodeLossConfig records the two cluster-level settings that stand between the
// 90s ungraceful-node-loss SLO and the ~11 minute worst case on stock defaults.
// Absence blocks CHAOS-03, it does not fail it.
type NodeLossConfig struct {
	ServerTolerationSeconds  *int64 `json:"serverTolerationSeconds"`
	OutOfServiceTaintUsable  bool   `json:"outOfServiceTaintUsable"`
	NotReadyTolerationSet    bool   `json:"notReadyTolerationSet"`
	UnreachableTolerationSet bool   `json:"unreachableTolerationSet"`
}

// Environment is the full record written to artifacts/<run-id>/environment.json.
type Environment struct {
	RunID     string    `json:"runId"`
	Timestamp time.Time `json:"timestamp"`

	KubernetesVersion string     `json:"kubernetesVersion"`
	Platform          string     `json:"platform"`
	Nodes             []NodeInfo `json:"nodes"`

	StorageClass    string   `json:"storageClass"`
	CSIDriver       string   `json:"csiDriver"`
	CSIDriverImages []string `json:"csiDriverImages,omitempty"`

	Servers []ServerInfo `json:"servers"`
	// FanOut is the number of server pods serving the RWX exports.
	FanOut int `json:"fanOut"`
	// IndependentlyVersioned reports whether server and CSI driver come from
	// different release trains. Gates the SKEW cases.
	IndependentlyVersioned bool `json:"independentlyVersioned"`

	Mounts           []MountInfo `json:"mounts"`
	NFSVersion       string      `json:"nfsVersion"`
	HardMount        bool        `json:"hardMount"`
	MountPropagation string      `json:"mountPropagation,omitempty"`

	Timing        Timing         `json:"timing"`
	NodeLoss      NodeLossConfig `json:"nodeLoss"`
	DelegationsOn bool           `json:"delegationsEnabled"`
	// RecoveryStateBackend is recorded for triage only. Lock survival is
	// measured empirically by CHAOS-02 and CHAOS-06, never inferred from this.
	RecoveryStateBackend string `json:"recoveryStateBackend"`

	IPFamilies []string `json:"ipFamilies"`

	Capabilities map[string]bool `json:"capabilities"`

	// Notes carries anything discovery could not determine, verbatim, so a
	// failure report says what was unknown instead of implying it was checked.
	Notes []string `json:"notes,omitempty"`
}

// AddNote records a discovery gap, so that a failure report says what was
// unknown instead of implying it was checked.
func (e *Environment) AddNote(format string, args ...any) {
	e.Notes = append(e.Notes, fmt.Sprintf(format, args...))
}

// Write serializes the environment into dir/environment.json.
func (e *Environment) Write(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "environment.json")
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Load reads an environment record back, for reruns that skip discovery.
func Load(path string) (*Environment, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var e Environment
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// ServerNamespace returns the namespace the NFS server pods live in, or "" when
// discovery never found them.
func (e *Environment) ServerNamespace() string {
	if e == nil || len(e.Servers) == 0 {
		return ""
	}
	return e.Servers[0].Namespace
}
