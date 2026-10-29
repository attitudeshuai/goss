package goss

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
	"github.com/stretchr/testify/require"
)

// TestValidateRejectsNonPositiveMaxConcurrent pins that the concurrency cap
// is validated before anything is scheduled: 0 and negative values must return
// a clear error rather than silently starting zero workers (which deadlocked
// the old feeder).
func TestValidateRejectsNonPositiveMaxConcurrent(t *testing.T) {
	for _, maxConcurrent := range []int{0, -1, -16} {
		cfg, err := util.NewConfig(
			util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
			util.WithMaxConcurrency(maxConcurrent),
		)
		require.NoError(t, err)

		results, err := ValidateResults(context.Background(), cfg)
		require.Error(t, err, "maxConcurrent=%d must be rejected at startup", maxConcurrent)
		require.Contains(t, err.Error(), "max-concurrent")
		require.Nil(t, results, "no result channel must be returned on an invalid cap")
	}
}

// TestServeRejectsNonPositiveMaxConcurrentAtStartup ensures the serve mode
// shares the validation and fails while constructing the handler, i.e. before
// the listener starts accepting probes.
func TestServeRejectsNonPositiveMaxConcurrentAtStartup(t *testing.T) {
	for _, maxConcurrent := range []int{0, -1} {
		cfg, err := util.NewConfig(
			util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
			util.WithMaxConcurrency(maxConcurrent),
		)
		require.NoError(t, err)

		_, err = newHealthHandler(cfg)
		require.Error(t, err)
		require.Contains(t, err.Error(), "max-concurrent")
	}
}

// resultRef is the identity and outcome of a single test result, used to pin
// the normal-path behaviour of the scheduler without depending on channel
// delivery order.
type resultRef struct {
	resourceType string
	resourceID   string
	property     string
	result       int
}

func collectSortedResults(t *testing.T, ch <-chan []resource.TestResult) []resultRef {
	t.Helper()
	var refs []resultRef
	for group := range ch {
		for _, r := range group {
			refs = append(refs, resultRef{
				resourceType: r.ResourceType,
				resourceID:   r.ResourceId,
				property:     r.Property,
				result:       r.Result,
			})
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].resourceType != refs[j].resourceType {
			return refs[i].resourceType < refs[j].resourceType
		}
		if refs[i].resourceID != refs[j].resourceID {
			return refs[i].resourceID < refs[j].resourceID
		}
		return refs[i].property < refs[j].property
	})
	return refs
}

// TestValidateResultsUnchangedOnNormalPath pins the count, identity and
// outcomes of results on an uncancelled run, both against the known
// testdata/passing.goss.yaml spec and across repeated runs (same scheduling
// semantics as before).
func TestValidateResultsUnchangedOnNormalPath(t *testing.T) {
	cfg, err := util.NewConfig(
		util.WithSpecFile(filepath.Join("testdata", "passing.goss.yaml")),
	)
	require.NoError(t, err)

	first, err := ValidateResults(context.Background(), cfg)
	require.NoError(t, err)
	got := collectSortedResults(t, first)

	want := []resultRef{
		{resourceType: "Command", resourceID: "hello world", property: "exit-status", result: resource.SUCCESS},
		{resourceType: "Command", resourceID: "hello world", property: "stdout", result: resource.SUCCESS},
	}
	require.Equal(t, want, got)

	// A second run must produce the exact same result set.
	second, err := ValidateResults(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, want, collectSortedResults(t, second))
}
