package framework

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecResult is the outcome of a command run inside a pod.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Err      error
}

// Combined returns stdout and stderr joined, for error messages.
func (r ExecResult) Combined() string {
	return strings.TrimSpace(r.Stdout + "\n" + r.Stderr)
}

// Exec runs argv in a pod container and captures its output. It never fails the
// test itself: callers decide whether a non-zero exit is a defect or the
// expected result, which matters because several cases assert on failure.
func (c *Client) Exec(ctx context.Context, ns, pod, container string, argv ...string) ExecResult {
	return c.exec(ctx, ns, pod, container, nil, argv...)
}

// ExecStdin runs argv with stdin attached, which is how a file reaches a pod
// without an image to carry it: the exec stream is binary-clean, so a binary
// goes in as bytes with no encoding step and no tool in the container beyond
// the shell redirect that receives it.
func (c *Client) ExecStdin(ctx context.Context, ns, pod, container string, stdin io.Reader, argv ...string) ExecResult {
	if stdin == nil {
		return ExecResult{Err: fmt.Errorf("ExecStdin called with no reader; use Exec"), ExitCode: -1}
	}
	return c.exec(ctx, ns, pod, container, stdin, argv...)
}

func (c *Client) exec(ctx context.Context, ns, pod, container string, stdin io.Reader, argv ...string) ExecResult {
	req := c.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdin:     stdin != nil,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.Rest, "POST", req.URL())
	if err != nil {
		return ExecResult{Err: fmt.Errorf("creating executor: %w", err), ExitCode: -1}
	}
	var stdout, stderr bytes.Buffer
	opts := remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}
	if stdin != nil {
		opts.Stdin = stdin
	}
	err = exec.StreamWithContext(ctx, opts)
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err != nil {
		res.Err = err
		res.ExitCode = exitCodeOf(err)
		return res
	}
	return res
}

// Sh runs a shell snippet in a pod, which is how nearly every case drives I/O.
func (c *Client) Sh(ctx context.Context, ns, pod, container, script string) ExecResult {
	return c.Exec(ctx, ns, pod, container, "sh", "-c", script)
}

// MustSh runs a shell snippet and returns stdout, or an error carrying the full
// output so a failure report says what the pod actually printed.
func (c *Client) MustSh(ctx context.Context, ns, pod, container, script string) (string, error) {
	r := c.Sh(ctx, ns, pod, container, script)
	if r.Err != nil {
		return "", fmt.Errorf("exec in %s/%s: %w: %s", ns, pod, r.Err, r.Combined())
	}
	return strings.TrimSpace(r.Stdout), nil
}

// exitCodeOf extracts the command's exit status from a remotecommand error.
func exitCodeOf(err error) int {
	type coder interface{ ExitStatus() int }
	if c, ok := err.(coder); ok {
		return c.ExitStatus()
	}
	// CodeExitError from remotecommand carries the code in its message.
	var e interface{ Error() string }
	e = err
	if strings.Contains(e.Error(), "command terminated with exit code ") {
		var code int
		if _, scanErr := fmt.Sscanf(e.Error()[strings.LastIndex(e.Error(), " ")+1:], "%d", &code); scanErr == nil {
			return code
		}
	}
	return -1
}

// scriptID matches an identifier that is safe to build a path and a filename
// from: no separators, no shell metacharacters, no leading dot.
var scriptID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// CheckScriptID rejects an identifier a helper would interpolate into a path
// inside a pod. Quoting alone is not enough: a quoted "../../etc/x" is still a
// path escape, and the helpers here build filenames under /tmp from these.
//
// Cases pass literals, so this never fires in practice. It exists because the
// safety of these helpers should not rest on who happens to call them today.
func CheckScriptID(id string) error {
	if !scriptID.MatchString(id) {
		return fmt.Errorf("%q is not usable as a script id: it must start with a letter or digit and hold "+
			"only letters, digits, dot, dash and underscore, since it becomes a filename inside the pod", id)
	}
	return nil
}

// rfc1123Subdomain matches a Kubernetes object name: lowercase alphanumerics,
// dashes and dots, starting and ending alphanumeric.
var rfc1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// CheckObjectName rejects a name Kubernetes will not accept, at the point the
// name becomes an object rather than at the point the API says no.
//
// It exists because the alternative is finding out on a cluster. A case that
// names a pod something RFC 1123 forbids fails at creation, minutes into a run,
// with an API message that names the rendered object rather than the logical
// name the case actually wrote. Rendering a name is cheap and testable, so this
// belongs in a unit test's reach.
func CheckObjectName(kind, name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%s name is empty", kind)
	case len(name) > 253:
		return fmt.Errorf("%s name %q is %d characters, over the 253 Kubernetes allows; "+
			"shorten -run-id, which is carried whole so that two runs cannot collide", kind, name, len(name))
	case !rfc1123Subdomain.MatchString(name):
		return fmt.Errorf("%s name %q is not a valid Kubernetes name: it must hold only lowercase letters, "+
			"digits, dashes and dots, and start and end with a letter or digit. Names are lowercased for you; "+
			"anything else has to change in the case that chose the name", kind, name)
	}
	return nil
}
