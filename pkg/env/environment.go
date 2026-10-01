// Package env holds the environment record that preflight discovers and every
// failure report carries. Discovery over declaration: a plan that requires
// humans to type version numbers correctly will be wrong within one sprint.
package env

import (
	"encoding/json"
	"errors"
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
	// Process is the process observed holding the listening NFS socket in this
	// pod. It is what an in-place kill is aimed at, and it is discovered once
	// here rather than by each case that injects one: the name is a property of
	// the image, so a case that re-derived it would be paying an exec into the
	// server for an answer that cannot have changed. What does change on every
	// restart is the pid, so no pid is recorded; a case that needs to know
	// whether a particular process died observes that for itself. See F-026.
	Process string `json:"process,omitempty"`
	// ProcessNote is why Process is empty, when it is.
	ProcessNote string `json:"processNote,omitempty"`
	// Capabilities is the server's capability state, read here once so that
	// SEC-09 evaluates a record rather than reading the cluster itself: naming
	// the server process needs the node agent (F-027), and a case should not.
	// Like Process it is a property of the image, the pod spec and the
	// platform, none of which a case changes.
	Capabilities *ServerCapabilities `json:"capabilities,omitempty"`
	// CapabilitiesNote is why Capabilities is empty, when it is.
	CapabilitiesNote string `json:"capabilitiesNote,omitempty"`
	// CapabilitiesBlocked says the note is a condition of this cluster that
	// another would not have, such as a server pod that is not running or a
	// PID 1 that is not the container's own, rather than the harness failing to
	// read what was there. SEC-09 reports the first blocked and the second
	// failed, as it did when it read the sets itself; an error's type does not
	// survive the record, and the note's text is not something to match on.
	// A server process that could not be named at all counts as blocked, as it
	// did for SEC-09 before, and as ProcessNote does for the cases that kill
	// it: DiscoverServerProcess does not yet type its errors, so the harness
	// failures among them are not told apart here.
	CapabilitiesBlocked bool `json:"capabilitiesBlocked,omitempty"`
}

// ServerCapabilities is what a server pod's container declares and what two of
// its processes hold, read at one moment so the three can be compared.
type ServerCapabilities struct {
	// PodUID ties the record to the pod instance it was read from, so a case
	// can tell a record that no longer describes the live pod
	// (framework.StaleCapabilityRecord).
	PodUID string `json:"podUID"`
	// Container is the pod container the server process was found in.
	Container string `json:"container"`
	// DeclaredAdd, DeclaredDrop and Privileged are that container's spec.
	DeclaredAdd  []string `json:"declaredAdd,omitempty"`
	DeclaredDrop []string `json:"declaredDrop,omitempty"`
	Privileged   bool     `json:"privileged"`
	// Server is the process holding the listening NFS socket.
	Server CapMasks `json:"server"`
	// Init is the container's PID 1, the process the runtime started, whose
	// set is what the platform delivered. On a supervised server it is not the
	// server (F-026, #98).
	Init CapMasks `json:"init"`
}

// CapMasks is one process's capability masks, in the 16-digit hex
// /proc/<pid>/status prints, so a record can be checked against a status file
// by eye.
type CapMasks struct {
	// Name is the process's comm.
	Name        string `json:"name"`
	Inheritable string `json:"inheritable"`
	Permitted   string `json:"permitted"`
	Effective   string `json:"effective"`
	Bounding    string `json:"bounding"`
	Ambient     string `json:"ambient"`
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
	// RunID is the run the record belongs to. In a run's directory that is
	// the run itself; in the preflight cache it is the run that discovered it.
	RunID string `json:"runId"`
	// Timestamp is when discovery ran, which on a reused result predates the
	// run whose directory holds the record.
	Timestamp time.Time `json:"timestamp"`

	Context           string     `json:"context"`
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
	path := filepath.Join(dir, "environment.json")
	return path, e.WriteTo(path)
}

// WriteTo serializes the environment to an exact path.
func (e *Environment) WriteTo(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := e.encode()
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// Create serializes the environment to path only if nothing is there yet, and
// never exposes a partial record. The bytes go to a temporary file in the same
// directory first, and a hard link publishes it under path: the link fails if
// path exists, so of two writers racing for it exactly one wins, and a reader
// sees either no record or the whole of one. The loser gets an error wrapping
// fs.ErrExist and the winner's file is untouched. A write that fails before the
// link leaves nothing at path. The one error that can follow publication is
// failing to remove the temporary file: the whole record is then at path and
// the error is still returned, naming the stray file, rather than discarded.
//
// O_EXCL alone reserves the name before the bytes are in it, and a split run
// started in parallel on one cluster would read the half-written record and
// stop for no reason.
func (e *Environment) Create(path string) error {
	b, err := e.encode()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".environment-*.json")
	if err != nil {
		return err
	}
	_, writeErr := tmp.Write(b)
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return errors.Join(os.Link(tmp.Name(), path), os.Remove(tmp.Name()))
}

// encode is the one serialization both writers use.
func (e *Environment) encode() ([]byte, error) {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
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
