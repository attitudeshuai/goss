package goss

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/onsi/gomega/format"

	"github.com/goss-org/goss/outputs"
	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/system"
	"github.com/goss-org/goss/util"
)

func getGossConfig(varsFiles []string, varsInline string, specFile string) (cfg *GossConfig, err error) {
	// handle stdin
	var fh *os.File
	var path, source string
	var gossConfig GossConfig

	tf, err := NewTemplateFilter(varsFiles, varsInline)
	if err != nil {
		return nil, err
	}
	setTemplateFilter(tf)

	if specFile == "-" {
		source = "STDIN"
		fh = os.Stdin
		data, err := io.ReadAll(fh)
		if err != nil {
			return nil, err
		}
		format, err := getStoreFormatFromData(data)
		if err != nil {
			return nil, err
		}
		setStoreFormat(format)

		gossConfig, err = ReadJSONData(data, true)
		if err != nil {
			return nil, err
		}
	} else {
		source = specFile
		path = filepath.Dir(specFile)
		format, err := getStoreFormatFromFileName(specFile)
		if err != nil {
			return nil, err
		}
		setStoreFormat(format)

		gossConfig, err = ReadJSON(specFile)
		if err != nil {
			return nil, err
		}
	}

	gossConfig, err = mergeJSONData(gossConfig, 0, path)
	if err != nil {
		return nil, err
	}

	if len(gossConfig.Resources()) == 0 {
		return nil, fmt.Errorf("found 0 tests, source: %v", source)
	}

	return &gossConfig, nil
}

func getOutputer(c *bool, format string) (outputs.Outputer, error) {
	if c != nil && *c {
		outputs.SetNoColor(true)
	}
	if c != nil && !*c {
		outputs.SetNoColor(false)
	}

	return outputs.GetOutputer(format)
}

// ValidateResults performs validation and provides programmatic access to validation results
// no retries or outputs are supported
func ValidateResults(ctx context.Context, c *util.Config) (results <-chan []resource.TestResult, err error) {
	gossConfig, err := getGossConfig(c.VarsFiles, c.VarsInline, c.Spec)
	if err != nil {
		return nil, err
	}

	sys := system.New(c.PackageManager)

	return validate(ctx, sys, *gossConfig, c.DisabledResourceTypes, c.MaxConcurrent)
}

// Validate performs validation, writes formatted output to stdout by default
// and supports retries and more, this is the full featured Validate used
// by the typical CLI invocation and will produce output to StdOut.  Use
// ValidateResults for programmatic access
func Validate(ctx context.Context, c *util.Config) (code int, err error) {
	err = setLogLevel(c)
	if err != nil {
		return 1, err
	}
	gossConfig, err := getGossConfig(c.VarsFiles, c.VarsInline, c.Spec)
	if err != nil {
		return 78, err
	}
	return ValidateConfig(ctx, c, gossConfig)
}

func ValidateConfig(ctx context.Context, c *util.Config, gossConfig *GossConfig) (code int, err error) {
	// Needed for contains-elements
	// Maybe we don't use this and use custom
	// contain_element_matcher is needed because it's single entry to avoid
	// transform message
	format.UseStringerRepresentation = true
	outputConfig := util.OutputConfig{
		FormatOptions: c.FormatOptions,
	}

	sys := system.New(c.PackageManager)
	outputer, err := getOutputer(c.NoColor, c.OutputFormat)
	if err != nil {
		return 1, err
	}

	var ofh io.Writer
	ofh = os.Stdout
	if c.OutputWriter != nil {
		ofh = c.OutputWriter
	}

	sleep := c.Sleep
	retryTimeout := c.RetryTimeout
	i := 1
	startTime := time.Now()
	for {
		out, err := validate(ctx, sys, *gossConfig, c.DisabledResourceTypes, c.MaxConcurrent)
		if err != nil {
			// Configuration error before any check ran, mirrors the exit code
			// used when the gossfile itself cannot be loaded.
			return 78, err
		}
		exitCode := outputer.Output(ofh, out, outputConfig)
		if retryTimeout == 0 || exitCode == 0 {
			return exitCode, nil
		}
		// The run was cancelled: do not enter the retry sleep, return promptly.
		if err := ctx.Err(); err != nil {
			return exitCode, err
		}
		elapsed := time.Since(startTime)
		if elapsed+sleep > retryTimeout {
			return 3, fmt.Errorf("timeout of %s reached before tests entered a passing state", retryTimeout)
		}
		color.Red("Retrying in %s (elapsed/timeout time: %.3fs/%s)\n\n\n", sleep, elapsed.Seconds(), retryTimeout)
		// Reset cache
		sys = system.New(c.PackageManager)
		select {
		case <-time.After(sleep):
		case <-ctx.Done():
			return exitCode, ctx.Err()
		}
		i++
		fmt.Printf("Attempt #%d:\n", i)
	}
}

// validateMaxConcurrency checks the caller supplied concurrency cap before any
// goroutine is started. maxConcurrent is a hard limit, so 0 or negative values
// are rejected explicitly instead of silently spawning zero workers (which
// deadlocks the feeder) or falling back to another number.
func validateMaxConcurrency(maxConcurrent int) error {
	if maxConcurrent <= 0 {
		return fmt.Errorf("max-concurrent must be a positive integer, got %d", maxConcurrent)
	}
	return nil
}

func validate(ctx context.Context, sys *system.System, gossConfig GossConfig, skipList []string, maxConcurrent int) (<-chan []resource.TestResult, error) {
	if err := validateMaxConcurrency(maxConcurrent); err != nil {
		return nil, err
	}

	out := make(chan []resource.TestResult)
	in := make(chan resource.Resource)

	// Feeder: once ctx is cancelled no new resource is handed to a worker, so
	// checks that were still queued never start.
	go func() {
		defer close(in)
		for _, t := range gossConfig.Resources() {
			if util.IsValueInList(t.TypeName(), skipList) || util.IsValueInList(t.TypeKey(), skipList) {
				t.SetSkip()
			}

			select {
			case in <- t:
			case <-ctx.Done():
				return
			}
		}
	}()

	workerCount := runtime.NumCPU() * 5
	if workerCount > maxConcurrent {
		workerCount = maxConcurrent
	}
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case f, ok := <-in:
					if !ok {
						return
					}
					// A cancellation that landed while this resource was being
					// dequeued means it had not started yet; do not run it.
					if ctx.Err() != nil {
						return
					}
					results := f.Validate(ctx, sys)
					// Never block the worker on a result send after cancel:
					// the reader may have gone away and in-flight results are
					// no longer wanted.
					select {
					case out <- results:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out, nil
}
