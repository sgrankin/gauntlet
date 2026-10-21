package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sgrankin/gauntlet/internal/core"
	"github.com/sgrankin/gauntlet/internal/testreport"
)

func prepareTestFiles(job core.CheckJob, host, visible string) ([]string, func(*core.CheckResult), error) {
	if job.TestResults == "" {
		return nil, func(*core.CheckResult) {}, nil
	}
	reportPath := filepath.Join(host, "test-results")
	if err := os.WriteFile(reportPath, nil, 0600); err != nil {
		return nil, nil, err
	}
	env := []string{core.EnvTestResultsFile + "=" + filepath.Join(visible, "test-results"), core.EnvRetryTestsFile + "="}
	if len(job.RetryTests) > 0 {
		data, err := json.Marshal(job.RetryTests)
		if err != nil {
			return nil, nil, err
		}
		if err = os.WriteFile(filepath.Join(host, "retry-tests"), data, 0600); err != nil {
			return nil, nil, err
		}
		env = append(env, core.EnvRetryTestsFile+"="+filepath.Join(visible, "retry-tests"))
	}
	capture := func(res *core.CheckResult) {
		if res.Err != nil {
			return
		}
		tests, err := testreport.Parse(job.TestResults, readReceiptResult(reportPath, testreport.MaxBytes))
		if err != nil {
			res.Status = core.CheckFailed
			res.Output += "\ngauntlet: " + err.Error()
			return
		}
		res.Tests = tests
		if len(testreport.Failed(tests)) > 0 {
			res.Status = core.CheckFailed
		}
	}
	return env, capture, nil
}

func testFilesError(name string, err error) core.CheckResult {
	return core.CheckResult{Name: name, Err: fmt.Errorf("executor: prepare test report: %w", err)}
}
