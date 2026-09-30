package framework

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestDmesgCommandReportsAReadThatNeverHappened runs the node agent's dmesg
// command under a real shell against stand-in dmesg binaries. It exists
// because the failure it guards against has no symptom: the command once ended
// in "|| true", so a node whose dmesg could not run answered with an empty
// success, and the failure bundle recorded it as a complete, quiet kernel log
// (#118).
//
// Steps:
//  1. A dmesg that rejects -T and prints plainly: the fallback must answer,
//     with success.
//  2. A dmesg that fails both ways: the command must exit non-zero and carry
//     the second attempt's message, so Agent.Run returns it as an error.
func TestDmesgCommandReportsAReadThatNeverHappened(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stub     string
		wantOK   bool
		wantText string
	}{
		{
			name: "busybox without -T",
			stub: "#!/bin/sh\n[ \"$1\" = -T ] && { echo 'dmesg: invalid option -- T' >&2; exit 1; }\n" +
				"echo '[    0.000000] Linux version 6.12.94+'\n",
			wantOK: true, wantText: "Linux version",
		},
		{
			name:   "neither form runs",
			stub:   "#!/bin/sh\necho 'dmesg: read kernel buffer failed: Operation not permitted' >&2\nexit 1\n",
			wantOK: false, wantText: "Operation not permitted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "dmesg"), []byte(tc.stub), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", dmesgCommand)
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()
			if gotOK := err == nil; gotOK != tc.wantOK {
				t.Errorf("%q exited ok=%t, want ok=%t; output %q", dmesgCommand, gotOK, tc.wantOK, out)
			}
			if !strings.Contains(string(out), tc.wantText) {
				t.Errorf("%q printed %q, want it to include %q", dmesgCommand, out, tc.wantText)
			}
		})
	}
}
