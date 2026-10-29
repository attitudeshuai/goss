package goss

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goss-org/goss/outputs"
	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/system"
	"github.com/goss-org/goss/util"
	"github.com/patrickmn/go-cache"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// cacheMissLogFormat is logged whenever a health probe finds the result cache
// cold and has to run the test suite. It carries a level prefix so that it can
// be muted via --loglevel; without one it was emitted unconditionally and
// flooded the logs of any container being polled by a load balancer.
const cacheMissLogFormat = "[DEBUG] Stale cache[%s], running tests"

const (
	// defaultShutdownGracePeriod bounds how long `goss serve` lets in-flight
	// probes finish after a termination signal before their validations are
	// cancelled. It matches the default Kubernetes termination grace period.
	defaultShutdownGracePeriod = 30 * time.Second
	// forcedShutdownWait bounds the wind-down after validations have been
	// cancelled, guaranteeing the process exits even if a check ignores its
	// context.
	forcedShutdownWait = 5 * time.Second
	// shutdownUnavailableBody is the fixed body returned to requests that
	// arrive once the server has started draining.
	shutdownUnavailableBody = "Service Unavailable: goss is shutting down\n"
)

// ShutdownForceExitCode is the process exit code used when the shutdown grace
// period elapses before all in-flight requests finish. It is deliberately
// non-zero so it can be distinguished from a clean shutdown (exit 0), and it
// mirrors the code used for other goss timeout conditions.
const ShutdownForceExitCode = 3

// ErrShutdownDeadlineExceeded is returned by Serve when the shutdown grace
// period elapsed with in-flight requests still unfinished. The unfinished
// validations were cancelled and their (partial) results were not cached.
var ErrShutdownDeadlineExceeded = errors.New("shutdown grace period elapsed before in-flight requests finished")

func Serve(c *util.Config) error {
	err := setLogLevel(c)
	if err != nil {
		return err
	}
	endpoint := c.Endpoint
	health, err := newHealthHandler(c)
	if err != nil {
		return err
	}
	lifecycle := newServeLifecycle()
	health.lifecycle = lifecycle

	http.Handle(endpoint, health)
	http.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    c.ListenAddress,
		Handler: lifecycle.middleware(http.DefaultServeMux),
	}

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- srv.ListenAndServe()
	}()
	log.Printf("[INFO] Starting to listen on: %s", c.ListenAddress)

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-sigCtx.Done():
	}

	return lifecycle.drain(srv, shutdownGracePeriod(c))
}

func shutdownGracePeriod(c *util.Config) time.Duration {
	if c.ShutdownGracePeriod > 0 {
		return c.ShutdownGracePeriod
	}
	return defaultShutdownGracePeriod
}

// serveLifecycle coordinates graceful shutdown of the serve mode HTTP server.
type serveLifecycle struct {
	mu       sync.Mutex
	draining bool

	// inflight counts requests that passed admission and have not returned yet.
	inflight sync.WaitGroup
	pending  atomic.Int64

	// ctx is cancelled once the grace period is exhausted (or a second
	// termination signal arrives); it cancels every validation that is still
	// running.
	ctx    context.Context
	cancel context.CancelFunc
}

func newServeLifecycle() *serveLifecycle {
	ctx, cancel := context.WithCancel(context.Background())
	return &serveLifecycle{
		ctx:    ctx,
		cancel: cancel,
	}
}

// admit reserves a slot for an incoming request. Once draining has started it
// returns false and the caller must reject the request instead of queueing it.
func (l *serveLifecycle) admit() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.draining {
		return false
	}
	l.inflight.Add(1)
	l.pending.Add(1)
	return true
}

func (l *serveLifecycle) release() {
	l.pending.Add(-1)
	l.inflight.Done()
}

func (l *serveLifecycle) beginDrain() {
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()
}

func (l *serveLifecycle) inFlight() int64 {
	return l.pending.Load()
}

// attach derives a request-scoped context that is also cancelled when the
// lifecycle force-cancels validations at the end of the grace period. The
// returned function releases the cancellation link and must be called when
// the request finishes.
func (l *serveLifecycle) attach(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// middleware rejects every request admitted after draining started with an
// explicit 503 instead of letting it queue behind in-flight work, and tracks
// the number of requests the shutdown must wait for.
func (l *serveLifecycle) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.admit() {
			writeShutdownUnavailable(w)
			return
		}
		defer l.release()
		next.ServeHTTP(w, r)
	})
}

func writeShutdownUnavailable(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Connection", "close")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, shutdownUnavailableBody)
}

// drain stops accepting new connections, lets admitted requests finish within
// the grace period, then cancels and force-closes anything still outstanding.
// It returns nil for a clean shutdown and ErrShutdownDeadlineExceeded when the
// grace period was exhausted.
func (l *serveLifecycle) drain(srv *http.Server, grace time.Duration) error {
	l.beginDrain()
	log.Printf("[INFO] Shutdown initiated: waiting up to %s for %d in-flight request(s)", grace, l.inFlight())

	srv.SetKeepAlivesEnabled(false)

	graceCtx, cancelGrace := context.WithTimeout(context.Background(), grace)
	defer cancelGrace()

	// A second termination signal short-circuits the remainder of the grace
	// period. Only the first signal started the drain, so a fresh channel is
	// needed to observe another one.
	repeatSignal := make(chan os.Signal, 1)
	signal.Notify(repeatSignal, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(repeatSignal)
	go func() {
		select {
		case <-repeatSignal:
			log.Printf("[WARN] Repeated termination signal: ending grace period, %d in-flight request(s)", l.inFlight())
			cancelGrace()
		case <-graceCtx.Done():
		}
	}()

	if err := srv.Shutdown(graceCtx); err == nil {
		log.Printf("[INFO] Shutdown complete: %d in-flight request(s) remaining", l.inFlight())
		return nil
	}

	log.Printf("[WARN] Shutdown grace period elapsed: cancelling %d in-flight request(s)", l.inFlight())
	l.cancel()
	if err := srv.Close(); err != nil {
		log.Printf("[DEBUG] Closing server connections returned: %v", err)
	}

	done := make(chan struct{})
	go func() {
		l.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		log.Printf("[INFO] Shutdown finished: %d in-flight request(s) remaining", l.inFlight())
	case <-time.After(forcedShutdownWait):
		log.Printf("[WARN] Shutdown forced after %s: %d in-flight request(s) remaining", forcedShutdownWait, l.inFlight())
	}
	return ErrShutdownDeadlineExceeded
}

func newHealthHandler(c *util.Config) (*healthHandler, error) {
	// The health endpoint always emits machine-readable output.
	outputs.SetNoColor(true)
	cache := cache.New(c.Cache, 30*time.Second)

	cfg, err := getGossConfig(c.VarsFiles, c.VarsInline, c.Spec)
	if err != nil {
		return nil, err
	}

	output, err := getOutputer(c.NoColor, c.OutputFormat)
	if err != nil {
		return nil, err
	}

	health := &healthHandler{
		c:             c,
		gossConfig:    *cfg,
		sys:           system.New(c.PackageManager),
		outputer:      output,
		cache:         cache,
		gossMu:        &sync.Mutex{},
		maxConcurrent: c.MaxConcurrent,
	}
	return health, nil
}

type res struct {
	body       bytes.Buffer
	statusCode int
}
type healthHandler struct {
	c             *util.Config
	gossConfig    GossConfig
	sys           *system.System
	outputer      outputs.Outputer
	cache         *cache.Cache
	gossMu        *sync.Mutex
	maxConcurrent int
	// lifecycle is nil for handler instances used directly (tests, embedded
	// callers); Serve installs one so requests participate in graceful shutdown.
	lifecycle *serveLifecycle
}

func (h healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	outputFormat, outputer, err := h.negotiateResponseContentType(r)
	if err != nil {
		log.Printf("[DEBUG] Warn: Using process-level output-format. %s", err)
		outputFormat = h.c.OutputFormat
		outputer = h.outputer
	}
	negotiatedContentType := h.responseContentType(outputFormat)

	ctx := r.Context()
	detach := func() {}
	if h.lifecycle != nil {
		ctx, detach = h.lifecycle.attach(ctx)
	}
	defer detach()

	log.Printf("[TRACE] %v: requesting health probe", r.RemoteAddr)
	resp, complete := h.processAndEnsureCached(ctx, negotiatedContentType, outputer)
	if !complete {
		// The validation was cancelled before it finished; never write a
		// partial response and never let a partial result reach the cache.
		log.Printf("[DEBUG] %v: validation cancelled during shutdown", r.RemoteAddr)
		writeShutdownUnavailable(w)
		return
	}
	w.Header().Set("Content-Type", negotiatedContentType)
	w.WriteHeader(resp.statusCode)
	logBody := ""
	if resp.statusCode != http.StatusOK {
		logBody = " - " + resp.body.String()
	}
	resp.body.WriteTo(w)
	log.Printf("[DEBUG] %v: status %d%s", r.RemoteAddr, resp.statusCode, logBody)
}

func (h healthHandler) processAndEnsureCached(ctx context.Context, negotiatedContentType string, outputer outputs.Outputer) (res, bool) {
	var tra [][]resource.TestResult
	cacheKey := "res"
	// Held across the lookup so a miss does not let a second request start its
	// own validate() before the first one has filled the cache.
	h.gossMu.Lock()
	if tmp, found := h.cache.Get(cacheKey); found {
		log.Printf("[TRACE] Returning cached[%s].", cacheKey)
		tra = tmp.([][]resource.TestResult)
	} else {
		log.Printf(cacheMissLogFormat, cacheKey)
		h.sys = system.New(h.c.PackageManager)
		tra = h.validate(ctx)
		if ctx.Err() != nil {
			// The suite was cancelled (shutdown grace exhausted): do not
			// cache partial results and do not overwrite the previous
			// complete entry.
			h.gossMu.Unlock()
			return res{}, false
		}
		h.cache.SetDefault(cacheKey, tra)
	}
	h.gossMu.Unlock()

	trc := testResultArrayToChan(tra)
	return h.output(trc, outputer), true
}

func (h healthHandler) output(trc <-chan []resource.TestResult, outputer outputs.Outputer) res {
	var b bytes.Buffer
	outputConfig := util.OutputConfig{
		FormatOptions: h.c.FormatOptions,
	}
	exitCode := outputer.Output(&b, trc, outputConfig)
	resp := res{
		body: b,
	}
	if exitCode == 0 {
		resp.statusCode = http.StatusOK
	} else {
		resp.statusCode = http.StatusServiceUnavailable
	}
	return resp
}
func (h healthHandler) validate(ctx context.Context) [][]resource.TestResult {
	h.sys = system.New(h.c.PackageManager)
	res := make([][]resource.TestResult, 0)
	tr := validate(ctx, h.sys, h.gossConfig, h.c.DisabledResourceTypes, h.maxConcurrent)
	for i := range tr {
		res = append(res, i)
	}
	return res
}

func testResultArrayToChan(tra [][]resource.TestResult) <-chan []resource.TestResult {
	c := make(chan []resource.TestResult)
	go func(c chan []resource.TestResult) {
		defer close(c)

		for _, i := range tra {
			c <- i
		}
	}(c)

	return c
}

const (
	// https://en.wikipedia.org/wiki/Media_type
	mediaTypePrefix = "application/vnd.goss-"
)

func (h healthHandler) negotiateResponseContentType(r *http.Request) (string, outputs.Outputer, error) {
	acceptHeader := r.Header[http.CanonicalHeaderKey("Accept")]
	var outputer outputs.Outputer
	outputName := ""
	for _, acceptCandidate := range acceptHeader {
		acceptCandidate = strings.TrimSpace(acceptCandidate)
		if strings.HasPrefix(acceptCandidate, mediaTypePrefix) {
			outputName = strings.TrimPrefix(acceptCandidate, mediaTypePrefix)
		} else if strings.EqualFold("application/json", acceptCandidate) || strings.EqualFold("text/json", acceptCandidate) {
			outputName = "json"
		} else {
			outputName = ""
		}
		var err error
		outputer, err = outputs.GetOutputer(outputName)
		if err != nil {
			continue
		}
	}
	if outputer == nil {
		return "", nil, fmt.Errorf("Accept header on request missing or invalid. Accept header: %v", acceptHeader)
	}

	return outputName, outputer, nil
}

func (h healthHandler) responseContentType(outputName string) string {
	if outputName == "json" {
		return "application/json"
	}
	if outputName == "prometheus" {
		return "text/plain; version=0.0.4"
	}

	return fmt.Sprintf("%s%s", mediaTypePrefix, outputName)
}
