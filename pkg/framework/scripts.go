package framework

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"strings"
)

// Scripts the suite runs inside pods are written as shell files under
// scripts/, embedded here, and shipped to the pod as-is. A script that lives
// in a Go format string is unreadable in both languages at once: the shell is
// broken up by verbs, and the Go is a wall of text nobody proofreads. A file
// can be read, run by hand against a directory, and syntax-checked.
//
// Every value a script needs arrives as a positional argument, quoted on the
// way. Nothing is interpolated into the body, so the file on disk is exactly
// the file that runs.
//
//go:embed scripts/*.sh
var scriptFS embed.FS

// scriptBody returns an embedded script. A missing script is a programming
// error rather than a runtime condition, so it panics: the embed directive
// above means the file either shipped in the binary or the build was wrong.
func scriptBody(name string) string {
	b, err := scriptFS.ReadFile("scripts/" + name)
	if err != nil {
		panic(fmt.Sprintf("framework: embedded script %q is missing: %v", name, err))
	}
	return string(b)
}

// scriptEOF terminates the heredoc that carries a script into a pod. It is
// quoted at the heredoc, so the body is passed through with no expansion of
// any kind, and it is distinctive enough that no script here contains it.
const scriptEOF = "NFSV_SCRIPT_EOF"

// runScriptID names a script copy after the run, for the helpers that have no
// case to name it after. The run ID is used as it is when prefix and run ID
// together make a valid script id, so the copy on the node says which run left
// it. Otherwise the run ID is replaced by a digest of it: CheckScriptID caps an
// id at 64 characters, a run ID has no cap of its own, and a run ID that names
// objects and bundles without complaint must not fail a node read before it
// starts. The first OBS-03 runs on #135 failed on exactly that.
func runScriptID(prefix string) string {
	run := strings.ToLower(Cfg().RunID)
	if CheckScriptID(prefix+run) == nil {
		return prefix + run
	}
	sum := sha256.Sum256([]byte(Cfg().RunID))
	return prefix + hex.EncodeToString(sum[:8])
}

// RunScript builds the shell that writes an embedded script into a pod and
// runs it with the given arguments. The id keeps two cases from writing over
// each other's copy, and is validated because it becomes a filename.
//
// The scripts that background themselves do so from inside the script, so this
// one helper serves both the workloads that outlive the exec and the sweeps
// that answer immediately.
func RunScript(name, id string, args ...string) (string, error) {
	if err := CheckScriptID(id); err != nil {
		return "", err
	}
	body := scriptBody(name)
	if strings.Contains(body, scriptEOF) {
		return "", fmt.Errorf("script %s contains the heredoc terminator %s", name, scriptEOF)
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	path := fmt.Sprintf("/tmp/nfsv-%s-%s", id, name)
	return fmt.Sprintf("set -e\ncat > %s <<'%s'\n%s\n%s\nsh %s %s\n",
		shellQuote(path), scriptEOF, strings.TrimRight(body, "\n"), scriptEOF,
		shellQuote(path), strings.Join(quoted, " ")), nil
}
