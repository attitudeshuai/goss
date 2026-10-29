package outputs

import (
	"fmt"
	"io"
	"strconv"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
)

type Tap struct{}

func (r Tap) ValidOptions() []*formatOption {
	return []*formatOption{
		{name: foSort},
	}
}

func (r Tap) Output(w io.Writer, results <-chan []resource.TestResult,
	outConfig util.OutputConfig) (exitCode int, err error) {
	includeRaw := !util.IsValueInList(foExcludeRaw, outConfig.FormatOptions)

	sort := util.IsValueInList(foSort, outConfig.FormatOptions)
	results = getResults(results, sort)

	fw := newFailWriter(w)

	testCount := 0
	failed := 0

	var summary = make(map[int]string)

	for resultGroup := range results {
		for _, testResult := range resultGroup {
			switch testResult.Result {
			case resource.SUCCESS:
				summary[testCount] = "ok " + strconv.Itoa(testCount+1) + " - " + humanizeResult(testResult, true, includeRaw) + "\n"
			case resource.FAIL:
				summary[testCount] = "not ok " + strconv.Itoa(testCount+1) + " - " + humanizeResult(testResult, true, includeRaw) + "\n"
				failed++
			case resource.SKIP:
				summary[testCount] = "ok " + strconv.Itoa(testCount+1) + " - # SKIP " + humanizeResult(testResult, true, includeRaw) + "\n"
			default:
				panic(fmt.Sprintf("Unexpected Result Code: %v\n", testResult.Result))
			}
			testCount++
		}
	}

	fmt.Fprintf(fw, "1..%d\n", testCount)

	for i := 0; i < testCount; i++ {
		fmt.Fprintf(fw, "%s", summary[i])
	}

	if fw.Err() != nil {
		return 0, fw.Err()
	}

	if failed > 0 {
		return 1, nil
	}

	return 0, nil
}
