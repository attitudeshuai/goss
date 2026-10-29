package outputs

import (
	"io"

	"github.com/goss-org/goss/resource"
	"github.com/goss-org/goss/util"
)

type Silent struct{}

func (r Silent) ValidOptions() []*formatOption {
	return []*formatOption{}
}

func (r Silent) Output(w io.Writer, results <-chan []resource.TestResult,
	outConfig util.OutputConfig) (exitCode int, err error) {

	var failed int
	for resultGroup := range results {
		for _, testResult := range resultGroup {
			switch testResult.Result {
			case resource.FAIL:
				failed++
			}
		}
	}

	// silent never writes, so there is no write error to report and the
	// shared write-error contract is satisfied with a nil error.
	if failed > 0 {
		return 1, nil
	}
	return 0, nil
}
