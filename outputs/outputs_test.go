package outputs

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
)

func TestIsValidFormat(t *testing.T) {
	if IsValidFormat("ne") {
		t.Fatal("'ne' should not be a valid output format")
	}

	if !IsValidFormat("json") {
		t.Fatal("'json' should be a valid output format")
	}
}

func TestOutputers(t *testing.T) {
	list := Outputers()
	assert.NotEmpty(t, list)
}

func TestGetOutputer(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		got, err := GetOutputer("rspecish")
		assert.NoError(t, err)
		assert.NotNil(t, got)
	})
	t.Run("not-valid", func(t *testing.T) {
		got, err := GetOutputer("gibberish")
		assert.Error(t, err)
		assert.Nil(t, got)
	})
}

func TestOutputFormatOptions(t *testing.T) {
	list := FormatOptions()
	assert.NotEmpty(t, list)

	assert.Contains(t, list, foPerfData)
	assert.Contains(t, list, foPretty)
	assert.Contains(t, list, foVerbose)
	assert.Len(t, list, 4)
}

func TestOptionsRegistration(t *testing.T) {
	registeredOutputs := Outputers()
	assert.Contains(t, registeredOutputs, "documentation")
	assert.Contains(t, registeredOutputs, "json")
	assert.Contains(t, registeredOutputs, "junit")
	assert.Contains(t, registeredOutputs, "nagios")
	assert.Contains(t, registeredOutputs, "prometheus")
	assert.Contains(t, registeredOutputs, "rspecish")
	assert.Contains(t, registeredOutputs, "silent")
	assert.Contains(t, registeredOutputs, "structured")
	assert.Contains(t, registeredOutputs, "tap")
}

// TestOutputExitCodes pins what every outputer returns, because serve turns a
// non-zero code into a 503 at /healthz. Prometheus was missing this once (#992)
// and structured was missing it too, so all of them are pinned at once rather
// than one per incident.
func TestOutputExitCodes(t *testing.T) {
	now := time.Now()
	pass := resource.TestResult{
		Result: resource.SUCCESS, ResourceType: "File", ResourceId: "/tmp",
		Property: "exists", StartTime: now, EndTime: now,
	}
	// a skip is not a failure, so it must not move the exit code on its own
	skip := resource.TestResult{
		Result: resource.SKIP, ResourceType: "File", ResourceId: "/skip",
		Property: "exists", Skipped: true, StartTime: now, EndTime: now,
	}
	fail := resource.TestResult{
		Result: resource.FAIL, ResourceType: "File", ResourceId: "/nope",
		Property: "exists", StartTime: now, EndTime: now,
	}

	cases := map[string]struct {
		outputer Outputer
		failing  int
	}{
		"documentation": {Documentation{}, 1},
		"json":          {Json{}, 1},
		"junit":         {JUnit{}, 1},
		"nagios":        {Nagios{}, 2},
		"prometheus":    {Prometheus{}, 1},
		"rspecish":      {Rspecish{}, 1},
		"silent":        {Silent{}, 1},
		"structured":    {Structured{}, 1},
		"tap":           {Tap{}, 1},
	}

	// a new outputer has to make a deliberate choice here rather than default to 0
	assert.Len(t, cases, len(Outputers()))

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// the prometheus outputer accumulates into package-level counters
			defer resetMetrics()

			code, err := tc.outputer.Output(io.Discard, makeResults(pass, skip), util.OutputConfig{})
			assert.NoError(t, err)
			assert.Equal(t, 0, code,
				"a suite with no failures must exit 0")
			code, err = tc.outputer.Output(io.Discard, makeResults(pass, skip, fail), util.OutputConfig{})
			assert.NoError(t, err)
			assert.Equal(t, tc.failing, code,
				"a suite containing a failure must not exit 0")
		})
	}
}

// failingWriter accepts up to failAfter bytes and then fails every write,
// recording the bytes that actually made it through so tests can prove output
// stops growing at the first failure.
type failingWriter struct {
	remaining int
	written   bytes.Buffer
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errors.New("simulated broken writer")
	}
	n := min(len(p), w.remaining)
	w.written.Write(p[:n])
	w.remaining -= n
	if n < len(p) {
		return n, errors.New("simulated broken writer")
	}
	return n, nil
}

// TestOutputWriteErrors drives every built-in outputer against a writer that
// fails, pinning the shared contract: a non-nil error is returned, nothing is
// written after the first failure (machine-readable closing markers included),
// and no panic escapes.
func TestOutputWriteErrors(t *testing.T) {
	now := time.Now()
	results := func() <-chan []resource.TestResult {
		return makeResults(
			resource.TestResult{
				Result: resource.SUCCESS, ResourceType: "File", ResourceId: "/tmp",
				Property: "exists", StartTime: now, EndTime: now,
			},
			resource.TestResult{
				Result: resource.SKIP, ResourceType: "File", ResourceId: "/skip",
				Property: "exists", Skipped: true, StartTime: now, EndTime: now,
			},
			resource.TestResult{
				Result: resource.FAIL, ResourceType: "File", ResourceId: "/nope",
				Property: "exists", StartTime: now, EndTime: now,
			},
		)
	}

	// every registered outputer must take part in the shared handling
	for _, name := range Outputers() {
		t.Run(name, func(t *testing.T) {
			outputer, err := GetOutputer(name)
			require.NoError(t, err)
			if name == "prometheus" {
				defer resetMetrics()
			}

			// silent emits no bytes at all, so a broken writer is unobservable
			// and it must still report a nil write error.
			if name == "silent" {
				code, err := outputer.Output(&failingWriter{}, results(), util.OutputConfig{})
				assert.NoError(t, err)
				assert.Equal(t, 1, code, "a failing suite still exits with the check-failure code")
				return
			}

			// a writer that fails on the first write: not a single byte may
			// reach it and the error must come back.
			dead := &failingWriter{}
			_, err = outputer.Output(dead, results(), util.OutputConfig{})
			require.Error(t, err, "a dead writer must surface as an error")
			assert.Empty(t, dead.written.Bytes(), "nothing must reach a writer that failed immediately")

			// a writer that dies after a few bytes: the partial prefix stays,
			// but nothing more may get through afterwards.
			partial := &failingWriter{remaining: 8}
			_, err = outputer.Output(partial, results(), util.OutputConfig{})
			require.Error(t, err)
			assert.LessOrEqual(t, partial.written.Len(), 8,
				"output must not grow after the first write failure")

			// machine-readable streams must not gain a closing marker after
			// the writer died.
			switch name {
			case "junit":
				assert.NotContains(t, partial.written.String(), "</testsuite>",
					"junit must not append its closing element to a dead writer")
			case "json", "structured":
				assert.NotContains(t, partial.written.String(), "summary-line",
					"%s must not append its body to a dead writer", name)
			case "tap":
				assert.NotContains(t, partial.written.String(), "not ok 3",
					"tap must not append later test lines to a dead writer")
			}
		})
	}
}
