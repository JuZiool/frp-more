// Package logbuf captures frp's global logger output into an in-memory ring
// buffer so the web UI can display recent logs. frp uses a package-level
// singleton logger shared by all embedded instances, so per-instance files are
// not possible; the UI filters the global buffer by keyword instead.
package logbuf

import (
	"os"
	"strings"
	"sync"
	"time"

	goliblog "github.com/fatedier/golib/log"

	frplog "github.com/fatedier/frp/pkg/util/log"
)

// Buffer is a bounded, thread-safe line buffer.
type Buffer struct {
	mu      sync.Mutex
	lines   []string
	max     int
	dropped int
}

func New(max int) *Buffer { return &Buffer{max: max} }

// Append stores one pre-formatted log line (trailing newline stripped).
func (b *Buffer) Append(line string) {
	line = strings.TrimRight(line, "\r\n")
	line = stripANSI(line)
	if line == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, line)
	if len(b.lines) > b.max {
		b.dropped += len(b.lines) - b.max
		b.lines = b.lines[len(b.lines)-b.max:]
	}
}

// Snapshot returns a copy of the buffered lines in chronological order.
func (b *Buffer) Snapshot() (lines []string, dropped int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.lines))
	copy(out, b.lines)
	return out, b.dropped
}

func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b[") {
		return s
	}
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\x1b' && i+1 < len(s) && s[i+1] == '[' {
			for i++; i < len(s) && s[i] != 'm'; i++ {
			}
			continue
		}
		sb.WriteByte(s[i])
	}
	return sb.String()
}

// captureWriter forwards log entries to the colorized console (same as frp's
// default output) and into the ring buffer for the UI.
type captureWriter struct {
	console consoleLike
	buf     *Buffer
}

type consoleLike interface {
	Write(p []byte) (n int, err error)
	WriteLog(p []byte, level goliblog.Level, when time.Time) (n int, err error)
}

func (w *captureWriter) Write(p []byte) (int, error) { return w.console.Write(p) }

func (w *captureWriter) WriteLog(p []byte, level goliblog.Level, when time.Time) (int, error) {
	n, err := w.console.WriteLog(p, level, when)
	if err == nil {
		w.buf.Append(string(p))
	}
	return n, err
}

// Attach replaces frp's global logger output with a capturing writer and
// returns the buffer feeding from it. Call once at startup, after InitLogger.
func Attach(maxLines int) *Buffer {
	buf := New(maxLines)
	console := goliblog.NewConsoleWriter(goliblog.ConsoleConfig{Colorful: true}, os.Stdout).(*goliblog.ConsoleWriter)
	frplog.Logger = frplog.Logger.WithOptions(goliblog.WithOutput(&captureWriter{
		console: console,
		buf:     buf,
	}))
	return buf
}
