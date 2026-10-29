package outputs

import (
	"fmt"
	"io"
	"time"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
)

type Documentation struct{}

func (r Documentation) ValidOptions() []*formatOption {
	return []*formatOption{
		{name: foSort},
	}
}

func (r Documentation) Output(w io.Writer, results <-chan []resource.TestResult,
	outConfig util.OutputConfig) (exitCode int, err error) {
	includeRaw := !util.IsValueInList(foExcludeRaw, outConfig.FormatOptions)

	sort := util.IsValueInList(foSort, outConfig.FormatOptions)
	results = getResults(results, sort)

	fw := newFailWriter(w)

	var startTime time.Time
	var endTime time.Time
	testCount := 0
	var failedOrSkipped [][]resource.TestResult
	var skipped, failed int
	for resultGroup := range results {
		failedOrSkippedGroup := []resource.TestResult{}
		first := resultGroup[0]
		header := header(first)
		if header != "" {
			fmt.Fprint(fw, header)
		}
		for _, testResult := range resultGroup {
			if startTime.IsZero() || testResult.StartTime.Before(startTime) {
				startTime = testResult.StartTime
			}
			if endTime.IsZero() || testResult.EndTime.After(endTime) {
				endTime = testResult.EndTime
			}
			switch testResult.Result {
			case resource.SUCCESS:
				fmt.Fprintln(fw, humanizeResult(testResult, false, includeRaw))
			case resource.SKIP:
				fmt.Fprintln(fw, humanizeResult(testResult, false, includeRaw))
				failedOrSkippedGroup = append(failedOrSkippedGroup, testResult)
				skipped++
			case resource.FAIL:
				fmt.Fprintln(fw, humanizeResult(testResult, false, includeRaw))
				failedOrSkippedGroup = append(failedOrSkippedGroup, testResult)
				failed++
			}
			testCount++
		}
		if len(failedOrSkippedGroup) > 0 {
			failedOrSkipped = append(failedOrSkipped, failedOrSkippedGroup)
		}
	}

	fmt.Fprint(fw, "\n\n")
	fmt.Fprint(fw, failedOrSkippedSummary(failedOrSkipped, includeRaw))

	fmt.Fprint(fw, summary(startTime, endTime, testCount, failed, skipped))
	if fw.Err() != nil {
		return 0, fw.Err()
	}
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}
