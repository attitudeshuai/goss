package goss

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goss-org/goss/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServeWithNoContentNegotiation(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		outputFormat        string
		specFile            string
		expectedHTTPStatus  int
		expectedContentType string
	}{
		"passing-json": {
			outputFormat:        "json",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/json",
		},
		"failing-json": {
			outputFormat:        "json",
			specFile:            filepath.Join("testdata", "failing.goss.yaml"),
			expectedHTTPStatus:  http.StatusServiceUnavailable,
			expectedContentType: "application/json",
		},
		"failing-default-output": {
			outputFormat:        "rspecish",
			specFile:            filepath.Join("testdata", "failing.goss.yaml"),
			expectedHTTPStatus:  http.StatusServiceUnavailable,
			expectedContentType: "",
		},
	}
	for testName := range tests {
		tc := tests[testName]
		t.Run(testName, func(t *testing.T) {
			var logOutput syncBuffer
			log.SetOutput(&logOutput)

			config, err := util.NewConfig(
				util.WithSpecFile(tc.specFile),
				util.WithOutputFormat(tc.outputFormat),
			)
			require.NoError(t, err)

			hh, err := newHealthHandler(config)
			require.NoError(t, err)

			req := makeRequest(t, config, nil)
			rr := httptest.NewRecorder()

			handler := http.HandlerFunc(hh.ServeHTTP)

			handler.ServeHTTP(rr, req)

			t.Logf("testName %q log output:\n%s", testName, logOutput.String())
			assert.Equal(t, tc.expectedHTTPStatus, rr.Code)
			if tc.expectedContentType != "" {
				assert.Equal(t, tc.expectedContentType, rr.Result().Header.Get("Content-Type"))
			}
		})
	}
}

func TestServeNegotiatingContent(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		acceptHeader        []string
		outputFormat        string
		specFile            string
		expectedHTTPStatus  int
		expectedContentType string
	}{
		"accept {blank} returns process-level format-option": {
			acceptHeader: []string{
				"",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/vnd.goss-structured",
		},
		"accept application/json": {
			acceptHeader: []string{
				"application/json",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/json",
		},
		"accept text/json translates to application/json": {
			acceptHeader: []string{
				"text/json",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/json",
		},
		"when accept is application/vnd.goss-json, return more widely known application/json": {
			acceptHeader: []string{
				"application/vnd.goss-json",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/json",
		},
		"accept prometheus": {
			acceptHeader: []string{
				"text/plain; version=0.0.4",
			},
			outputFormat:        "prometheus",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "text/plain; version=0.0.4",
		},
		"accept header contains vendor-specific output format different from process-level": {
			acceptHeader: []string{
				"application/vnd.goss-rspecish",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/vnd.goss-rspecish",
		},
		"accept header contains nonsense": {
			acceptHeader: []string{
				"application/vnd.goss-nonexistent",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/vnd.goss-structured",
		},
		"accept header contains nonsense then valid": {
			acceptHeader: []string{
				"application/vnd.goss-nonexistent",
				"application/json",
			},
			outputFormat:        "structured",
			specFile:            filepath.Join("testdata", "passing.goss.yaml"),
			expectedHTTPStatus:  http.StatusOK,
			expectedContentType: "application/json",
		},
	}
	for testName := range tests {
		tc := tests[testName]
		t.Run(testName, func(t *testing.T) {
			var logOutput syncBuffer
			log.SetOutput(&logOutput)

			config, err := util.NewConfig(
				util.WithSpecFile(tc.specFile),
				util.WithOutputFormat(tc.outputFormat),
			)
			require.NoError(t, err)

			hh, err := newHealthHandler(config)
			require.NoError(t, err)

			req := makeRequest(t, config, map[string][]string{
				"accept": tc.acceptHeader,
			})
			rr := httptest.NewRecorder()

			handler := http.HandlerFunc(hh.ServeHTTP)

			handler.ServeHTTP(rr, req)

			t.Logf("testName %q log output:\n%s", testName, logOutput.String())
			assert.Equal(t, tc.expectedHTTPStatus, rr.Code)
			if tc.expectedContentType != "" {
				assert.Equal(t, tc.expectedContentType, rr.Result().Header.Get("Content-Type"))
			}
		})
	}
}

func TestServeCacheWithNoContentNegotiation(t *testing.T) {
	var logOutput syncBuffer
	log.SetOutput(&logOutput)
	const cache = time.Duration(time.Millisecond * 100)
	config, err := util.NewConfig(
		util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
		util.WithCache(cache),
	)
	require.NoError(t, err)

	hh, err := newHealthHandler(config)
	require.NoError(t, err)

	req := makeRequest(t, config, nil)
	rr := httptest.NewRecorder()

	handler := http.HandlerFunc(hh.ServeHTTP)

	t.Run("fresh cache", func(t *testing.T) {
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.Contains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})

	t.Run("immediately re-request, cache should be warm", func(t *testing.T) {
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.NotContains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})

	t.Run("allow cache to expire, cache should be cold", func(t *testing.T) {
		time.Sleep(cache + 5*time.Millisecond)
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.Contains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})
}

func TestServeCacheNegotiatingContent(t *testing.T) {
	var logOutput syncBuffer
	log.SetOutput(&logOutput)
	const cache = time.Duration(time.Millisecond * 100)
	config, err := util.NewConfig(
		util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
		util.WithCache(cache),
		util.WithOutputFormat("structured"),
	)
	require.NoError(t, err)

	hh, err := newHealthHandler(config)
	require.NoError(t, err)

	rr := httptest.NewRecorder()

	handler := http.HandlerFunc(hh.ServeHTTP)

	t.Run("fresh cache", func(t *testing.T) {
		req := makeRequest(t, config, map[string][]string{
			"accept": {"application/json"},
		})
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.Contains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})

	t.Run("immediately re-request, cache should be warm", func(t *testing.T) {
		req := makeRequest(t, config, map[string][]string{
			"accept": {"application/json"},
		})
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.NotContains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})

	t.Run("immediately re-request but different accept header, cache should be warm", func(t *testing.T) {
		req := makeRequest(t, config, map[string][]string{
			"accept": {"application/vnd.goss-rspecish"},
		})
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.NotContains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})

	t.Run("allow cache to expire, cache should be cold", func(t *testing.T) {
		time.Sleep(cache + 5*time.Millisecond)
		req := makeRequest(t, config, map[string][]string{
			"accept": {"application/json"},
		})
		handler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Result().StatusCode)
		assert.Contains(t, logOutput.String(), "Stale cache")
		t.Log(logOutput.String())
		logOutput.Reset()
	})
}

func makeRequest(t *testing.T, config *util.Config, headers map[string][]string) *http.Request {
	req, err := http.NewRequest("GET", config.Endpoint, nil)
	require.NoError(t, err)
	for header, vals := range headers {
		for _, v := range vals {
			req.Header.Add(header, v)
		}
	}
	return req
}

// failingResponseWriter stands in for a connection that dropped before the
// body was fully sent: Header/WriteHeader behave normally, every Write fails.
type failingResponseWriter struct {
	header http.Header
	status int
}

func (f *failingResponseWriter) Header() http.Header {
	if f.header == nil {
		f.header = http.Header{}
	}
	return f.header
}

func (f *failingResponseWriter) WriteHeader(statusCode int) {
	f.status = statusCode
}

func (f *failingResponseWriter) Write([]byte) (int, error) {
	return 0, assert.AnError
}

// TestServeBrokenResponseWrite covers a client that disappears while the
// health response is being sent: the failure is logged exactly once and names
// the format, the freshly computed results are not left in the cache, and the
// handler keeps serving subsequent probes normally.
func TestServeBrokenResponseWrite(t *testing.T) {
	var logOutput syncBuffer
	log.SetOutput(&logOutput)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	config, err := util.NewConfig(
		util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
		util.WithOutputFormat("json"),
	)
	require.NoError(t, err)

	hh, err := newHealthHandler(config)
	require.NoError(t, err)

	handler := http.HandlerFunc(hh.ServeHTTP)

	// first probe: the connection cannot accept the body
	handler.ServeHTTP(&failingResponseWriter{}, makeRequest(t, config, nil))

	logs := logOutput.String()
	assert.Equal(t, 1, strings.Count(logs, "failed writing"),
		"a broken response write must be logged exactly once, got:\n%s", logs)
	assert.Contains(t, logs, "failed writing json health response",
		"the log line must name the active format")

	// results from the failed response must not have been cached
	_, cached := hh.cache.Get(resultCacheKey)
	assert.False(t, cached, "results of a failed response must not be cached")

	// second probe from a healthy client must be served normally and trigger a
	// fresh validation run rather than reuse anything from the failed request
	logOutput.Reset()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, makeRequest(t, config, nil))
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "summary")
	assert.Contains(t, logOutput.String(), "running tests",
		"the probe after a broken response must recompute instead of reading a cache entry")
}
