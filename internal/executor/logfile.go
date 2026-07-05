package executor

import (
	"os"
	"path/filepath"
)

// openCheckLog opens path for writing the full per-check log (DESIGN.md
// "Full per-check log files"), creating any missing parent directories
// first. path == "" means no log file was requested — it returns (nil,
// nil) in that case, the same shape as any other "nothing to do" result.
//
// A non-nil error here is deliberately not fatal to the caller's check:
// losing the log file must never fail the check itself (that's the whole
// point of it being a supplementary record, not the check's verdict
// source). Callers open the log alongside the tail buffer that always
// backs CheckResult.Output, and on error simply skip the log file — the
// tail-only capture keeps working exactly as it did before this file
// existed.
func openCheckLog(path string) (*os.File, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}
