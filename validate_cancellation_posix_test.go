//go:build !windows
// +build !windows

package goss

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
	"github.com/stretchr/testify/require"
)

func writeSpec(t *testing.T, contents string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "goss.yaml")
	require.NoError(t, os.WriteFile(f, []byte(contents), 0o644))
	return f
}

// blockingCommandSpec builds n command resources, each marking a file in
// markerDir before sleeping. The marker proves whether (and how many) checks
// actually started; the sleep keeps them in flight until the context is
// cancelled.
func blockingCommandSpec(markerDir string, n int) string {
	var b strings.Builder
	b.WriteString("command:\n")
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("c%d", i)
		fmt.Fprintf(&b, "  %s:\n", id)
		fmt.Fprintf(&b, "    exec: \"touch %s/%s; sleep 30\"\n", markerDir, id)
		b.WriteString("    exit-status: 0\n")
		b.WriteString("    timeout: 60000\n")
	}
	return b.String()
}

func quickCommandSpec(n int) string {
	var b strings.Builder
	b.WriteString("command:\n")
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("q%d", i)
		fmt.Fprintf(&b, "  %s:\n", id)
		b.WriteString("    exec: \"sleep 0.2\"\n")
		b.WriteString("    exit-status: 0\n")
		b.WriteString("    timeout: 10000\n")
	}
	return b.String()
}

func markerCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return len(entries)
}

func requireMarkerCount(t *testing.T, dir string, want int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return markerCount(t, dir) >= want
	}, 5*time.Second, 5*time.Millisecond, "expected %d in-flight markers in %s", want, dir)
}

// TestValidateCancellationDoesNotRunQueuedChecks covers the core guarantee
// with a single worker: once the context is cancelled while one check is in
// flight, the check still queued must never start.
func TestValidateCancellationDoesNotRunQueuedChecks(t *testing.T) {
	markerDir := t.TempDir()
	spec := writeSpec(t, blockingCommandSpec(markerDir, 2))

	cfg, err := util.NewConfig(
		util.WithSpecFile(spec),
		util.WithMaxConcurrency(1),
	)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	results, err := ValidateResults(ctx, cfg)
	require.NoError(t, err)

	// Exactly one check can be in flight with maxConcurrent=1.
	requireMarkerCount(t, markerDir, 1)
	require.Equal(t, 1, markerCount(t, markerDir), "more than one check started under maxConcurrent=1")

	cancel()
	drained := make(chan struct{})
	go func() {
		for range results {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("result channel was not closed after cancellation")
	}

	// Give a wrongly scheduled check plenty of time to leave its marker; none
	// is allowed to have started.
	time.Sleep(250 * time.Millisecond)
	require.Equal(t, 1, markerCount(t, markerDir), "a queued check ran after cancellation")
}

// TestValidateCancellationDrainsInFlight checks that in-flight checks return
// promptly on cancel and that no scheduler goroutine survives the run.
func TestValidateCancellationDrainsInFlight(t *testing.T) {
	markerDir := t.TempDir()
	spec := writeSpec(t, blockingCommandSpec(markerDir, 4))

	cfg, err := util.NewConfig(
		util.WithSpecFile(spec),
		util.WithMaxConcurrency(2),
	)
	require.NoError(t, err)

	baseline := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	results, err := ValidateResults(ctx, cfg)
	require.NoError(t, err)

	// Both workers are busy with a blocking check.
	requireMarkerCount(t, markerDir, 2)
	require.Equal(t, 2, markerCount(t, markerDir), "hard cap of 2 concurrent checks exceeded")

	cancel()
	drained := make(chan struct{})
	go func() {
		for range results {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(3 * time.Second):
		t.Fatal("result channel was not closed after cancellation")
	}

	// All goroutines the scheduler and the killed subprocesses accounted for
	// must be gone.
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= baseline+2
	}, 2*time.Second, 10*time.Millisecond, "goroutines leaked after cancellation: %d (baseline %d)", runtime.NumGoroutine(), baseline)

	// The two queued checks never ran.
	time.Sleep(250 * time.Millisecond)
	require.Equal(t, 2, markerCount(t, markerDir), "a queued check ran after cancellation")
}

// TestValidateHardConcurrencyLimit proves the cap is a real limit by timing
// waves of short checks: six 200ms checks with cap 2 cannot finish faster than
// three sequential waves (~600ms), and every check still runs and succeeds.
func TestValidateHardConcurrencyLimit(t *testing.T) {
	spec := writeSpec(t, quickCommandSpec(6))

	cfg, err := util.NewConfig(
		util.WithSpecFile(spec),
		util.WithMaxConcurrency(2),
	)
	require.NoError(t, err)

	start := time.Now()
	results, err := ValidateResults(context.Background(), cfg)
	require.NoError(t, err)

	groups := 0
	for group := range results {
		for _, r := range group {
			require.Equal(t, resource.SUCCESS, r.Result, "check %s: %s failed", r.ResourceType, r.ResourceId)
		}
		groups++
	}
	elapsed := time.Since(start)

	require.Equal(t, 6, groups, "every queued check must produce its result")
	require.GreaterOrEqual(t, elapsed, 450*time.Millisecond,
		"six 200ms checks under cap 2 need >=3 waves, finished in %s", elapsed)
	require.Less(t, elapsed, 5*time.Second, "validation took unexpectedly long: %s", elapsed)
}

// TestServeCancelledProbeIsNotCached verifies serve mode uses the same
// cancellation semantics: a probe cancelled mid-run must not leave partial
// results in the cache for the next request.
func TestServeCancelledProbeIsNotCached(t *testing.T) {
	markerDir := t.TempDir()
	spec := writeSpec(t, blockingCommandSpec(markerDir, 1))

	cfg, err := util.NewConfig(
		util.WithSpecFile(spec),
		util.WithOutputFormat("json"),
	)
	require.NoError(t, err)

	handler, err := newHealthHandler(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(ctx)
	rr := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(rr, req)
		close(done)
	}()

	requireMarkerCount(t, markerDir, 1)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve handler did not return after the probe was cancelled")
	}

	if _, found := handler.cache.Get("res"); found {
		t.Fatal("partial results from a cancelled probe were cached")
	}
}
