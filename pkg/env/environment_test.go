package env

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Preflight serializes the environment record to artifacts/<run-id>/environment.json
// and loads it back on reruns via -env-file. A serialization or deserialization bug
// here corrupts the environment record silently.

// TestEnvironmentRoundTrip verifies that an Environment record with nodes,
// servers, mounts, timing, capabilities, and notes survives serialization and
// deserialization without data loss.
//
// Steps:
//  1. Construct a populated Environment record.
//  2. Write it to disk using WriteTo.
//  3. Load it back using Load.
//  4. Assert all core fields match the original.
func TestEnvironmentRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "environment.json")

	orig := &Environment{
		RunID:             "20260910-120000",
		Timestamp:         time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC),
		Context:           "test-cluster",
		KubernetesVersion: "v1.31.0",
		Platform:          "gke",
		Nodes: []NodeInfo{
			{
				Name:             "node-1",
				KubeletVersion:   "v1.31.0",
				KernelVersion:    "6.1.0",
				OSImage:          "Ubuntu 22.04",
				ContainerRuntime: "containerd://1.7.1",
				Schedulable:      true,
				Labels:           map[string]string{"topology.kubernetes.io/zone": "us-central1-a"},
			},
		},
		StorageClass: "nfs-client",
		CSIDriver:    "nfs.csi.k8s.io",
		Servers: []ServerInfo{
			{
				Namespace: "nfs-system",
				Pod:       "nfs-server-0",
				Node:      "node-1",
				Images:    []string{"registry.k8s.io/nfs:v1"},
			},
		},
		FanOut:     1,
		NFSVersion: "4.1",
		HardMount:  true,
		Timing: Timing{
			LeaseSeconds:  60,
			GraceSeconds:  90,
			Profile:       "default",
			DiscoveredVia: "probe",
		},
		Capabilities: map[string]bool{
			"multiNode": true,
		},
		Notes: []string{"note 1", "note 2"},
	}

	if err := orig.WriteTo(path); err != nil {
		t.Fatalf("WriteTo(%q): %v", path, err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load(%q): %v", path, err)
	}

	if loaded.RunID != orig.RunID {
		t.Errorf("RunID: got %q, want %q", loaded.RunID, orig.RunID)
	}
	if !loaded.Timestamp.Equal(orig.Timestamp) {
		t.Errorf("Timestamp: got %v, want %v", loaded.Timestamp, orig.Timestamp)
	}
	if loaded.Context != orig.Context {
		t.Errorf("Context: got %q, want %q", loaded.Context, orig.Context)
	}
	if loaded.Platform != orig.Platform {
		t.Errorf("Platform: got %q, want %q", loaded.Platform, orig.Platform)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].Name != "node-1" {
		t.Errorf("Nodes mismatch: got %+v, want %+v", loaded.Nodes, orig.Nodes)
	}
	if len(loaded.Servers) != 1 || loaded.Servers[0].Namespace != "nfs-system" {
		t.Errorf("Servers mismatch: got %+v, want %+v", loaded.Servers, orig.Servers)
	}
	if loaded.Timing.Profile != "default" {
		t.Errorf("Timing.Profile: got %q, want default", loaded.Timing.Profile)
	}
	if !loaded.Capabilities["multiNode"] {
		t.Error("Capabilities: multiNode is false or missing")
	}
	if len(loaded.Notes) != 2 || loaded.Notes[0] != "note 1" {
		t.Errorf("Notes mismatch: got %+v, want %+v", loaded.Notes, orig.Notes)
	}
}

// TestEnvironmentWriteCreatesDirectory verifies that Write creates intermediate
// directories when saving environment.json.
//
// Steps:
//  1. Point Write at a nested non-existent directory.
//  2. Assert the directory and file are created without error.
func TestEnvironmentWriteCreatesDirectory(t *testing.T) {
	nestedDir := filepath.Join(t.TempDir(), "artifacts", "run-12345")
	e := &Environment{RunID: "run-12345"}

	path, err := e.Write(nestedDir)
	if err != nil {
		t.Fatalf("Write(%q): %v", nestedDir, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("environment file not created at %q: %v", path, err)
	}
}

// TestEnvironmentCreateOneWinner covers the run-level record's write-once
// primitive. Two invocations of a split run started together on one cluster
// both write the record; if the loser could read the winner's file while it was
// still being written, it would find truncated JSON and stop a legitimate run.
//
// Steps:
//  1. Start several writers at once, each creating the same path with a
//     different context, while readers load the path in a loop.
//  2. Assert exactly one writer succeeded and every other got fs.ErrExist.
//  3. Assert no reader ever saw a record that failed to parse, and that the
//     final record is the winner's.
//  4. Assert no temporary file was left in the directory.
func TestEnvironmentCreateOneWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "environment.json")
	const writers = 8

	stop := make(chan struct{})
	var partial atomic.Int32
	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := Load(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					partial.Add(1)
				}
			}
		}()
	}

	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A large record widens the window in which a partial file would show.
			e := &Environment{Context: fmt.Sprintf("ctx-%d", i), Notes: make([]string, 2000)}
			errs[i] = e.Create(path)
		}()
	}
	wg.Wait()
	close(stop)
	readers.Wait()

	winner := -1
	for i, err := range errs {
		switch {
		case err == nil && winner == -1:
			winner = i
		case err == nil:
			t.Errorf("writers %d and %d both created the record", winner, i)
		case !errors.Is(err, fs.ErrExist):
			t.Errorf("writer %d failed with %v, want fs.ErrExist", i, err)
		}
	}
	if winner == -1 {
		t.Fatal("no writer created the record")
	}
	if n := partial.Load(); n > 0 {
		t.Errorf("readers saw an unparseable record %d times, so a concurrent invocation would stop for nothing", n)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("loading the final record: %v", err)
	}
	if want := fmt.Sprintf("ctx-%d", winner); got.Context != want {
		t.Errorf("the record holds context %q, want the winner's %q", got.Context, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the directory holds %v, want only environment.json", names)
	}
}

// TestEnvironmentLoadNotFound verifies that Load returns an error when the
// file does not exist.
//
// Steps:
//  1. Attempt to Load a non-existent path.
//  2. Assert an error is returned and wraps os.ErrNotExist.
func TestEnvironmentLoadNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	if err == nil {
		t.Fatal("Load of non-existent file succeeded, want error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected os.ErrNotExist, got %v", err)
	}
}

// TestEnvironmentAddNote verifies that AddNote appends formatted messages
// to the environment record.
//
// Steps:
//  1. Call AddNote with formatting arguments.
//  2. Assert the message is appended correctly.
func TestEnvironmentAddNote(t *testing.T) {
	e := &Environment{}
	e.AddNote("server probe on node %s timed out after %ds", "node-1", 5)
	if len(e.Notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(e.Notes))
	}
	want := "server probe on node node-1 timed out after 5s"
	if e.Notes[0] != want {
		t.Errorf("got note %q, want %q", e.Notes[0], want)
	}
}

// TestEnvironmentServerNamespace verifies that ServerNamespace extracts the
// namespace of the first server pod or returns empty when none exist.
//
// Steps:
//  1. Check ServerNamespace on nil Environment.
//  2. Check ServerNamespace on Environment with empty Servers.
//  3. Check ServerNamespace on Environment with populated Servers.
func TestEnvironmentServerNamespace(t *testing.T) {
	var nilEnv *Environment
	if got := nilEnv.ServerNamespace(); got != "" {
		t.Errorf("nilEnv.ServerNamespace() = %q, want empty", got)
	}

	emptyEnv := &Environment{}
	if got := emptyEnv.ServerNamespace(); got != "" {
		t.Errorf("emptyEnv.ServerNamespace() = %q, want empty", got)
	}

	populatedEnv := &Environment{
		Servers: []ServerInfo{
			{Namespace: "custom-nfs", Pod: "pod-0"},
			{Namespace: "other-nfs", Pod: "pod-1"},
		},
	}
	if got := populatedEnv.ServerNamespace(); got != "custom-nfs" {
		t.Errorf("populatedEnv.ServerNamespace() = %q, want custom-nfs", got)
	}
}
