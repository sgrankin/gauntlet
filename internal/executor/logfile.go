package executor

import (
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

// checkLog is the open handle for a full per-check log file: a single zstd
// frame written directly to disk (no intermediate buffering beyond zstd's
// own). Writes go through the encoder; Close flushes and terminates that
// frame before closing the underlying file.
//
// Encoder concurrency is pinned to 1 (WithEncoderConcurrency(1)): the
// package doc for that option notes this "disable[s] async compression" for
// streams, trading away zstd's multi-goroutine block pipelining for a
// single, predictable writer per check. That trade is deliberate here —
// every concurrently-running check gets its own *checkLog, so the default
// (GOMAXPROCS workers each) would multiply goroutines by however many
// checks the daemon runs at once, for a log that's a supplementary record,
// not a throughput-critical path.
type checkLog struct {
	enc *zstd.Encoder
	f   *os.File
}

// Write implements io.Writer, feeding p through the zstd encoder.
func (l *checkLog) Write(p []byte) (int, error) {
	return l.enc.Write(p)
}

// Close flushes the trailing zstd frame and closes the underlying file.
// Per openCheckLog's doc, a non-nil error here must never fail the check
// that's calling it — callers already discard this the same way they
// discard the open error; a check killed mid-write simply leaves a
// truncated frame on disk, an acceptable degradation of a supplementary
// log (the read side is expected to decode what it can and stop there).
func (l *checkLog) Close() error {
	encErr := l.enc.Close()
	fileErr := l.f.Close()
	if encErr != nil {
		return encErr
	}
	return fileErr
}

// openCheckLog opens path for writing the full per-check log (DESIGN.md
// "Full per-check log files"), creating any missing parent directories
// first, and wraps it in a zstd encoder at the fastest level (SpeedFastest
// favors throughput over ratio: this log is a supplementary record of
// potentially large check output, not a space-optimized archive). path ==
// "" means no log file was requested — it returns (nil, nil) in that case,
// the same shape as any other "nothing to do" result.
//
// A non-nil error here is deliberately not fatal to the caller's check:
// losing the log file must never fail the check itself (that's the whole
// point of it being a supplementary record, not the check's verdict
// source). Callers open the log alongside the tail buffer that always
// backs CheckResult.Output, and on error simply skip the log file — the
// tail-only capture keeps working exactly as it did before this file
// existed.
func openCheckLog(path string) (*checkLog, error) {
	if path == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(f,
		zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1),
	)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &checkLog{enc: enc, f: f}, nil
}
