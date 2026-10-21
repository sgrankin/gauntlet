// Package testreport reads bounded test-runner reports and validates rerun coverage.
package testreport

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/sgrankin/gauntlet/internal/core"
)

const MaxBytes = 1 << 20
const MaxTests = 10000

func Parse(format string, data []byte) ([]core.TestCase, error) {
	if len(data) == 0 || len(data) > MaxBytes {
		return nil, fmt.Errorf("test report must contain 1..%d bytes", MaxBytes)
	}
	var tests []core.TestCase
	var err error
	switch format {
	case "json":
		var report struct {
			Tests []core.TestCase `json:"tests"`
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&report)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = fmt.Errorf("trailing test report data")
		}
		tests = report.Tests
	case "junit", "trx":
		tests, err = parseXML(format, data)
	default:
		return nil, fmt.Errorf("unsupported test report format %q", format)
	}
	if err != nil {
		return nil, fmt.Errorf("invalid %s test report: %w", format, err)
	}
	if err = Validate(tests); err != nil {
		return nil, err
	}
	return tests, nil
}

func Validate(tests []core.TestCase) error {
	if len(tests) == 0 || len(tests) > MaxTests {
		return fmt.Errorf("test report must contain 1..%d tests", MaxTests)
	}
	seen := map[string]bool{}
	total := 0
	for _, test := range tests {
		total += len(test.ID) + len(test.Message)
		if total > 2*MaxBytes {
			return fmt.Errorf("test report is too large")
		}
		if strings.TrimSpace(test.ID) == "" || len(test.ID) > 2048 || seen[test.ID] || len(test.Message) > 8192 {
			return fmt.Errorf("invalid or duplicate test identity %q", test.ID)
		}
		if !slices.Contains([]string{"passed", "failed", "skipped"}, test.Status) {
			return fmt.Errorf("test %q: invalid status", test.ID)
		}
		seen[test.ID] = true
	}
	return nil
}

func parseXML(format string, data []byte) ([]core.TestCase, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var tests []core.TestCase
	var suites []string
	depth, roots, total := 0, 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		previous := len(tests)
		switch element := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots > 1 {
					return nil, fmt.Errorf("multiple XML roots")
				}
			}
			depth++
			if depth > 64 {
				return nil, fmt.Errorf("XML nesting exceeds 64")
			}
			if format == "junit" && element.Name.Local == "testsuite" {
				suites = append(suites, attr(element, "name"))
			}
			if format == "junit" && element.Name.Local == "testcase" {
				var testcase struct {
					Name    string `xml:"name,attr"`
					Class   string `xml:"classname,attr"`
					Failure *struct {
						Message string `xml:"message,attr"`
						Text    string `xml:",chardata"`
					} `xml:"failure"`
					Error *struct {
						Message string `xml:"message,attr"`
						Text    string `xml:",chardata"`
					} `xml:"error"`
					Skipped *struct{} `xml:"skipped"`
				}
				if err := decoder.DecodeElement(&testcase, &element); err != nil {
					return nil, err
				}
				depth--
				if testcase.Name == "" {
					return nil, fmt.Errorf("testcase has no name")
				}
				test := core.TestCase{ID: strings.Join(suites, "/") + "::" + testcase.Class + "::" + testcase.Name, Status: "passed"}
				if testcase.Skipped != nil {
					test.Status = "skipped"
				}
				if testcase.Failure != nil {
					test.Status = "failed"
					test.Message = strings.TrimSpace(testcase.Failure.Message + " " + testcase.Failure.Text)
				}
				if testcase.Error != nil {
					test.Status = "failed"
					test.Message = strings.TrimSpace(testcase.Error.Message + " " + testcase.Error.Text)
				}
				tests = append(tests, test)
			}
			if format == "trx" && element.Name.Local == "UnitTestResult" {
				var result struct {
					Message string `xml:"Output>ErrorInfo>Message"`
				}
				if err := decoder.DecodeElement(&result, &element); err != nil {
					return nil, err
				}
				depth--
				test := core.TestCase{ID: attr(element, "testName"), Message: strings.TrimSpace(result.Message)}
				switch attr(element, "outcome") {
				case "Passed":
					test.Status = "passed"
				case "Failed", "Error", "Timeout", "Aborted":
					test.Status = "failed"
				case "NotExecuted", "Inconclusive", "Skipped":
					test.Status = "skipped"
				default:
					return nil, fmt.Errorf("unsupported TRX outcome")
				}
				tests = append(tests, test)
			}
		case xml.EndElement:
			depth--
			if format == "junit" && element.Name.Local == "testsuite" && len(suites) > 0 {
				suites = suites[:len(suites)-1]
			}
		}
		if len(tests) > previous {
			test := tests[len(tests)-1]
			total += len(test.ID) + len(test.Message)
			if total > 2*MaxBytes || len(test.ID) > 2048 || len(test.Message) > 8192 {
				return nil, fmt.Errorf("test report identity or message exceeds bounds")
			}
		}
		if len(tests) > MaxTests {
			return nil, fmt.Errorf("too many tests")
		}
	}
	return tests, nil
}
func attr(element xml.StartElement, name string) string {
	for _, a := range element.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func Failed(tests []core.TestCase) []string {
	var ids []string
	for _, test := range tests {
		if test.Status == "failed" {
			ids = append(ids, test.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// MergeRerun retains passing siblings and requires every selected failure to be
// reported again. A green process exit cannot compensate for omitted coverage.
func MergeRerun(previous, rerun []core.TestCase, requested []string) ([]core.TestCase, error) {
	if err := Validate(rerun); err != nil {
		return previous, err
	}
	byID := map[string]core.TestCase{}
	for _, test := range rerun {
		byID[test.ID] = test
	}
	for _, id := range requested {
		test, ok := byID[id]
		if !ok || test.Status == "skipped" {
			return previous, fmt.Errorf("retry did not execute failed test %q", id)
		}
	}
	merged := slices.Clone(previous)
	for i, test := range merged {
		if replacement, ok := byID[test.ID]; ok {
			merged[i] = replacement
			delete(byID, test.ID)
		}
	}
	for _, test := range rerun {
		if _, ok := byID[test.ID]; ok {
			merged = append(merged, test)
		}
	}
	return merged, Validate(merged)
}
