package pluginhost

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// maxStderrLine bounds one line copied to the debug log.
	maxStderrLine = 2 << 10
	// stderrTailBytes bounds the tail an error result shows.
	stderrTailBytes = 8 << 10
	// defaultStderrBytes matches the manifest default.
	defaultStderrBytes = 1 << 20
	// stderrMarker ends the file when the cap dropped output.
	stderrMarker = "[stderr truncated]\n"
)

// stderrSink copies one plugin's stderr to
// <artifacts>/plugins/<name>.stderr.log, bounded by the manifest's
// stderrBytes. It keeps the last 8 KiB for error results. A sink without an
// artifacts directory only logs and keeps the tail.
type stderrSink struct {
	name   string
	limit  int64
	logger *slog.Logger

	mu        sync.Mutex
	file      *os.File
	partial   []byte
	written   int64
	truncated bool
	closed    bool
	tail      []byte
}

// newStderrSink opens the log of one plugin. A limit below one byte means the
// manifest default.
func newStderrSink(name, artifactsDir string, limit int64, logger *slog.Logger) (*stderrSink, error) {
	if limit <= 0 {
		limit = defaultStderrBytes
	}
	sink := &stderrSink{name: name, limit: limit, logger: logger}
	if artifactsDir == "" {
		return sink, nil
	}
	dir := filepath.Join(artifactsDir, "plugins")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("plugin %q: stderr directory: %w", name, err)
	}
	file, err := os.OpenFile(filepath.Join(dir, name+".stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: stderr log: %w", name, err)
	}
	sink.file = file
	return sink, nil
}

// Write accepts stderr bytes. It never fails the plugin process.
func (s *stderrSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return len(p), nil
	}
	s.appendTail(p)
	s.partial = append(s.partial, p...)
	for {
		index := bytes.IndexByte(s.partial, '\n')
		if index < 0 {
			break
		}
		line := s.partial[:index]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		s.writeLine(line)
		s.partial = append(s.partial[:0], s.partial[index+1:]...)
	}
	if len(s.partial) > maxStderrLine {
		// A line this long is cut in the log; keep memory bounded.
		s.writeLine(s.partial[:maxStderrLine])
		s.partial = append(s.partial[:0], s.partial[maxStderrLine:]...)
	}
	return len(p), nil
}

// Tail returns the last stderr bytes, for an error result.
func (s *stderrSink) Tail() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.tail)
}

// Close flushes the last partial line and closes the file.
func (s *stderrSink) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if len(s.partial) > 0 {
		s.writeLine(s.partial)
		s.partial = nil
	}
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// writeLine logs one line and appends it to the file until the cap.
func (s *stderrSink) writeLine(line []byte) {
	s.logger.Debug("plugin stderr", "plugin", s.name, "line", strings.ToValidUTF8(truncate(string(line), maxStderrLine), "\uFFFD"))
	if s.file == nil || s.truncated || len(line) == 0 {
		return
	}
	remaining := s.limit - s.written
	if remaining <= 0 {
		s.markTruncated()
		return
	}
	data := make([]byte, 0, len(line)+1)
	data = append(data, line...)
	data = append(data, '\n')
	if int64(len(data)) > remaining {
		_, _ = s.file.Write(data[:remaining])
		s.written = s.limit
		s.markTruncated()
		return
	}
	_, _ = s.file.Write(data)
	s.written += int64(len(data))
}

// markTruncated writes the single marker that reports dropped output.
func (s *stderrSink) markTruncated() {
	if s.truncated || s.file == nil {
		return
	}
	s.truncated = true
	_, _ = s.file.WriteString(stderrMarker)
}

// appendTail keeps the last stderrTailBytes written.
func (s *stderrSink) appendTail(p []byte) {
	s.tail = append(s.tail, p...)
	if len(s.tail) > stderrTailBytes {
		s.tail = append(s.tail[:0], s.tail[len(s.tail)-stderrTailBytes:]...)
	}
}
