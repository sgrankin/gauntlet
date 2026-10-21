package testreport

import (
	"reflect"
	"testing"

	"github.com/sgrankin/gauntlet/internal/core"
)

func TestReportFormatsAndStableIDs(t *testing.T) {
	for _, tc := range []struct{ format, body, id, status string }{
		{"json", `{"tests":[{"id":"db/TestRetry/42","status":"failed","message":"timeout"}]}`, "db/TestRetry/42", "failed"},
		{"junit", `<testsuites><testsuite name="db"><testcase classname="Sql" name="TestRetry"><failure message="timeout"/></testcase></testsuite></testsuites>`, "db::Sql::TestRetry", "failed"},
		{"trx", `<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"><Results><UnitTestResult testName="Database.Tests.Retry(42)" outcome="Passed" testId="variable-guid"/></Results></TestRun>`, "Database.Tests.Retry(42)", "passed"},
	} {
		tests, err := Parse(tc.format, []byte(tc.body))
		if err != nil || len(tests) != 1 || tests[0].ID != tc.id || tests[0].Status != tc.status {
			t.Fatalf("%s: %+v %v", tc.format, tests, err)
		}
	}
	for _, body := range []string{`{}`, `{"tests":[{"id":"a","status":"approved"}]}`, `{"tests":[{"id":"a","status":"passed"},{"id":"a","status":"passed"}]}`, `{"tests":[{"id":"a","status":"passed"}]} {}`} {
		if _, err := Parse("json", []byte(body)); err == nil {
			t.Errorf("accepted invalid report %s", body)
		}
	}
	if _, err := Parse("junit", []byte(`<testsuite><testcase name="a"/></testsuite><extra/>`)); err == nil {
		t.Fatal("accepted trailing XML")
	}
}

func TestSubsetCoverageMustBeExecutedAndPassing(t *testing.T) {
	previous := []core.TestCase{{ID: "green", Status: "passed"}, {ID: "red", Status: "failed"}}
	for _, rerun := range [][]core.TestCase{nil, {{ID: "other", Status: "passed"}}, {{ID: "red", Status: "skipped"}}} {
		if _, err := MergeRerun(previous, rerun, []string{"red"}); err == nil {
			t.Errorf("accepted missing/skipped coverage: %+v", rerun)
		}
	}
	merged, err := MergeRerun(previous, []core.TestCase{{ID: "red", Status: "passed"}}, []string{"red"})
	if err != nil || !reflect.DeepEqual(merged, []core.TestCase{{ID: "green", Status: "passed"}, {ID: "red", Status: "passed"}}) {
		t.Fatalf("lost passing sibling: %+v %v", merged, err)
	}
	if previous[1].Status != "failed" {
		t.Fatal("mutated original report")
	}
}
