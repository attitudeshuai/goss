package goss

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
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

// resultCacheKey is the single cache entry holding the latest test results.
const resultCacheKey = "res"

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
	http.Handle(endpoint, health)
	http.Handle("/metrics", promhttp.Handler())
	log.Printf("[INFO] Starting to listen on: %s", c.ListenAddress)
	return http.ListenAndServe(c.ListenAddress, nil)
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
}

func (h healthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	outputFormat, outputer, err := h.negotiateResponseContentType(r)
	if err != nil {
		log.Printf("[DEBUG] Warn: Using process-level output-format. %s", err)
		outputFormat = h.c.OutputFormat
		outputer = h.outputer
	}
	negotiatedContentType := h.responseContentType(outputFormat)

	log.Printf("[TRACE] %v: requesting health probe", r.RemoteAddr)
	resp := h.processAndEnsureCached(r.Context(), negotiatedContentType, outputer)
	w.Header().Set("Content-Type", negotiatedContentType)
	w.WriteHeader(resp.statusCode)
	if _, err := resp.body.WriteTo(w); err != nil {
		// The client went away mid-response. Results produced for a response
		// that never went out must not stay cached; log once and move on, the
		// next probe is handled like any other.
		h.cache.Delete(resultCacheKey)
		log.Printf("[ERROR] %v: failed writing %s health response: %v", r.RemoteAddr, outputFormat, err)
		return
	}
	logBody := ""
	if resp.statusCode != http.StatusOK {
		logBody = " - " + resp.body.String()
	}
	log.Printf("[DEBUG] %v: status %d%s", r.RemoteAddr, resp.statusCode, logBody)
}

func (h healthHandler) processAndEnsureCached(ctx context.Context, negotiatedContentType string, outputer outputs.Outputer) res {
	var tra [][]resource.TestResult
	// Held across the lookup so a miss does not let a second request start its
	// own validate() before the first one has filled the cache.
	h.gossMu.Lock()
	if tmp, found := h.cache.Get(resultCacheKey); found {
		log.Printf("[TRACE] Returning cached[%s].", resultCacheKey)
		tra = tmp.([][]resource.TestResult)
	} else {
		log.Printf(cacheMissLogFormat, resultCacheKey)
		h.sys = system.New(h.c.PackageManager)
		tra = h.validate(ctx)
		h.cache.SetDefault(resultCacheKey, tra)
	}
	h.gossMu.Unlock()

	trc := testResultArrayToChan(tra)
	return h.output(trc, outputer)
}

func (h healthHandler) output(trc <-chan []resource.TestResult, outputer outputs.Outputer) res {
	var b bytes.Buffer
	outputConfig := util.OutputConfig{
		FormatOptions: h.c.FormatOptions,
	}
	// The response is fully rendered into b; a bytes.Buffer never fails to
	// write, so there is no write error to handle before ServeHTTP streams it.
	exitCode, _ := outputer.Output(&b, trc, outputConfig)
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
