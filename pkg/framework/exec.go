package framework

import (
	"bytes"
	"context"
	"fmt"
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
	req := c.Kube.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.Rest, "POST", req.URL())
	if err != nil {
		return ExecResult{Err: fmt.Errorf("creating executor: %w", err), ExitCode: -1}
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr})
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
