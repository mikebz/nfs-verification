// Command preflight runs Section 0 of the test plan and writes
// artifacts/<run-id>/environment.json. It exits non-zero with a specific reason
// on any failure. No test should execute after a preflight failure.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/mikebz/nfs-verification/pkg/framework"
	"github.com/mikebz/nfs-verification/pkg/preflight"
)

func main() {
	timeout := flag.Duration("timeout", 3*time.Minute, "preflight budget")
	flag.Parse()
	if err := framework.FinalizeFlags(); err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res, err := preflight.Run(ctx)
	if err != nil {
		fail(err)
	}
	b, _ := json.MarshalIndent(res.Env, "", "  ")
	fmt.Println(string(b))
	fmt.Fprintf(os.Stderr, "\npreflight passed: storageClass=%s csi=%s servers=%d profile=%s(lease=%ds grace=%ds)\n",
		res.Env.StorageClass, res.Env.CSIDriver, res.Env.FanOut,
		res.Env.Timing.Profile, res.Env.Timing.LeaseSeconds, res.Env.Timing.GraceSeconds)
	fmt.Fprintf(os.Stderr, "environment written to %s/environment.json\n", framework.RunDir())
	fmt.Fprintf(os.Stderr, "cached for context %q at %s; test runs reuse it for %s\n",
		res.Env.Context, framework.PreflightCache(res.Env.Context), framework.Cfg().PreflightMaxAge)
	for _, n := range res.Env.Notes {
		fmt.Fprintf(os.Stderr, "note: %s\n", n)
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "PREFLIGHT FAILED: %v\n", err)
	os.Exit(1)
}
