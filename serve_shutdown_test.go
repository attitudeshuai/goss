package goss

import (
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goss-org/goss/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowCommandGossfile is a suite whose single check sleeps for a while, so a
// health probe stays in flight long enough to observe shutdown phases.
func slowCommandGossfile(sleep string, timeoutMS int) string {
	return "command:\n" +
		"  goss-shutdown-slow:\n" +
		"    exec: sleep " + sleep + "\n" +
		"    exit-status: 0\n" +
		"    timeout: " + strconv.Itoa(timeoutMS) + "\n"
}

func newShutdownHandler(t *testing.T, specContents string) (*healthHandler, *serveLifecycle) {
	t.Helper()
	dir := t.TempDir()
	specPath := filepath.Join(dir, "goss.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(specContents), 0o644))

	config, err := util.NewConfig(
		util.WithSpecFile(specPath),
		util.WithOutputFormat("json"),
	)
	require.NoError(t, err)

	hh, err := newHealthHandler(config)
	require.NoError(t, err)

	lc := newServeLifecycle()
	hh.lifecycle = lc
	return hh, lc
}

func newShutdownHandlerFromFile(t *testing.T, specPath string) (*healthHandler, *serveLifecycle) {
	t.Helper()
	config, err := util.NewConfig(
		util.WithSpecFile(specPath),
		util.WithOutputFormat("json"),
	)
	require.NoError(t, err)

	hh, err := newHealthHandler(config)
	require.NoError(t, err)

	lc := newServeLifecycle()
	hh.lifecycle = lc
	return hh, lc
}

func serveProbe(t *testing.T, h http.Handler) (int, string) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	result := rr.Result()
	body, _ := io.ReadAll(result.Body)
	return result.StatusCode, string(body)
}

// Once draining has started, new requests must be rejected immediately with an
// explicit 503 instead of queueing, and must never reach a validation run.
func TestServeDrainRejectsNewRequestsWith503(t *testing.T) {
	t.Parallel()
	var logOutput syncBuffer
	log.SetOutput(&logOutput)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	hh, lc := newShutdownHandlerFromFile(t, filepath.Join("testdata", "passing.goss.yaml"))
	h := lc.middleware(http.HandlerFunc(hh.ServeHTTP))

	lc.beginDrain()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	assert.Equal(t, http.StatusServiceUnavailable, rr.Code)
	assert.Equal(t, "text/plain; charset=utf-8", rr.Result().Header.Get("Content-Type"))
	assert.Equal(t, "close", rr.Result().Header.Get("Connection"))
	assert.Equal(t, shutdownUnavailableBody, rr.Body.String())
	assert.Zero(t, lc.inFlight(), "rejected requests do not count as in-flight")

	_, found := hh.cache.Get("res")
	assert.False(t, found, "rejected request must not run or cache a validation")
	assert.NotContains(t, logOutput.String(), "running tests")
}

// Requests already in flight when the signal arrives must run to completion
// during the grace period and return their full, normal response.
func TestServeInFlightRequestCompletesDuringDrain(t *testing.T) {
	t.Parallel()
	hh, lc := newShutdownHandler(t, slowCommandGossfile("0.3", 5000))
	h := lc.middleware(http.HandlerFunc(hh.ServeHTTP))

	type result struct {
		status int
		body   string
	}
	resultCh := make(chan result, 1)
	go func() {
		status, body := serveProbe(t, h)
		resultCh <- result{status, body}
	}()

	require.Eventually(t, func() bool { return lc.inFlight() == 1 }, time.Second, 5*time.Millisecond)

	// Drain starts while the probe is mid-validation.
	lc.beginDrain()

	select {
	case r := <-resultCh:
		assert.Equal(t, http.StatusOK, r.status, "in-flight probe must finish normally during the grace period")
		assert.Contains(t, r.body, `"successful"`)
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not complete after drain started")
	}

	require.Eventually(t, func() bool { return lc.inFlight() == 0 }, time.Second, 5*time.Millisecond)
	_, found := hh.cache.Get("res")
	assert.True(t, found, "the completed validation must be cached")
}

// When validations are cancelled at the end of the grace period, their partial
// results must never be written to the cache, and the client does not receive a
// half-rendered body.
func TestServeCancelledValidationIsNotCached(t *testing.T) {
	t.Parallel()
	hh, lc := newShutdownHandler(t, slowCommandGossfile("10", 30000))
	h := lc.middleware(http.HandlerFunc(hh.ServeHTTP))

	statusCh := make(chan int, 1)
	go func() {
		status, _ := serveProbe(t, h)
		statusCh <- status
	}()

	require.Eventually(t, func() bool { return lc.inFlight() == 1 }, time.Second, 5*time.Millisecond)

	// Grace period exhausted: cancel the running validation outright.
	lc.beginDrain()
	lc.cancel()

	select {
	case status := <-statusCh:
		assert.Equal(t, http.StatusServiceUnavailable, status)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled request did not return promptly")
	}

	require.Eventually(t, func() bool { return lc.inFlight() == 0 }, time.Second, 5*time.Millisecond)
	_, found := hh.cache.Get("res")
	assert.False(t, found, "cancelled validation must not populate the cache")
}

// Cancelling must never overwrite a complete result that was cached before the
// drain; cache hits still serve the previous full response byte-for-byte.
func TestServeCancelledValidationKeepsPreviousCompleteResult(t *testing.T) {
	t.Parallel()
	hh, lc := newShutdownHandlerFromFile(t, filepath.Join("testdata", "passing.goss.yaml"))
	h := http.HandlerFunc(hh.ServeHTTP)

	status, bodyBefore := serveProbe(t, h)
	require.Equal(t, http.StatusOK, status)
	require.NotEmpty(t, bodyBefore)

	lc.beginDrain()
	lc.cancel()

	// Bypass the admission middleware (it would 503): a cache hit inside the
	// handler must still return the previous complete result.
	statusAfter, bodyAfter := serveProbe(t, h)
	assert.Equal(t, http.StatusOK, statusAfter)
	assert.Equal(t, bodyBefore, bodyAfter, "shutdown must not overwrite the last complete cached result")
}

func startDrainTestServer(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

func newProbeClient(t *testing.T) *http.Client {
	t.Helper()
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr}
}

// End-to-end drain: in-flight request finishes within the grace period, drain
// returns nil (clean exit), and the listener is gone afterwards.
func TestServeDrainGracefulOverHTTP(t *testing.T) {
	t.Parallel()
	hh, lc := newShutdownHandler(t, slowCommandGossfile("0.3", 5000))
	srv, baseURL := startDrainTestServer(t, lc.middleware(http.HandlerFunc(hh.ServeHTTP)))

	client := newProbeClient(t)
	respCh := make(chan int, 1)
	go func() {
		resp, err := client.Get(baseURL + "/healthz")
		if err != nil {
			respCh <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		respCh <- resp.StatusCode
	}()

	require.Eventually(t, func() bool { return lc.inFlight() == 1 }, time.Second, 5*time.Millisecond)

	drainErr := lc.drain(srv, 5*time.Second)
	require.NoError(t, drainErr, "all in-flight requests finished within the grace period: clean shutdown")

	select {
	case status := <-respCh:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight HTTP client never got a response")
	}

	_, found := hh.cache.Get("res")
	assert.True(t, found)
	assert.Zero(t, lc.inFlight())

	_, err := net.DialTimeout("tcp", strings.TrimPrefix(baseURL, "http://"), 250*time.Millisecond)
	assert.Error(t, err, "listener must be closed after drain")
}

// End-to-end forced drain: a long-running check exceeds the grace period, is
// cancelled, the process path returns ErrShutdownDeadlineExceeded without
// hanging, and no validation goroutines survive.
func TestServeDrainForcedOverHTTP(t *testing.T) {
	t.Parallel()
	hh, lc := newShutdownHandler(t, slowCommandGossfile("10", 30000))
	srv, baseURL := startDrainTestServer(t, lc.middleware(http.HandlerFunc(hh.ServeHTTP)))

	cl := newProbeClient(t)
	type clientResult struct {
		status int
		err    error
	}
	clientCh := make(chan clientResult, 1)
	go func() {
		resp, err := cl.Get(baseURL + "/healthz")
		if err != nil {
			clientCh <- clientResult{0, err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		clientCh <- clientResult{resp.StatusCode, nil}
	}()

	require.Eventually(t, func() bool { return lc.inFlight() == 1 }, time.Second, 5*time.Millisecond)

	start := time.Now()
	drainErr := lc.drain(srv, 100*time.Millisecond)
	elapsed := time.Since(start)

	require.ErrorIs(t, drainErr, ErrShutdownDeadlineExceeded)
	assert.Less(t, elapsed, 4*time.Second, "forced drain must not hang waiting for the slow check")
	assert.Zero(t, lc.inFlight())

	select {
	case cr := <-clientCh:
		if cr.err == nil {
			assert.NotEqual(t, http.StatusOK, cr.status, "a request unfinished at grace expiry must not be reported as successful")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client request was neither failed nor answered after forced shutdown")
	}

	_, found := hh.cache.Get("res")
	assert.False(t, found, "the killed validation must not have written the cache")

	// The cancelled command check was killed and its reaper goroutine joined,
	// so no validation goroutines are left behind.
	assert.Eventually(t, func() bool {
		return !strings.Contains(goroutineStacks(), "goss/system.runCommand")
	}, 3*time.Second, 10*time.Millisecond)
}

func goroutineStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}
