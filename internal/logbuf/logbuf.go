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

// InstanceTag is added to every log produced by an embedded instance. The tag
// is deliberately unique and is the only criterion used to route a line into
// an instance stream; ordinary global FRP lines never leak into it.
func InstanceTag(name string) string { return "frp-more:" + name }

// InstanceStore keeps a bounded and isolated log stream for each managed
// instance. FRP exposes a process-wide logger, so the manager adds InstanceTag
// to the context of every Service before it starts.
type InstanceStore struct {
	mu        sync.RWMutex
	max       int
	instances map[string]*Buffer
}

func NewInstanceStore(max int) *InstanceStore {
	if max < 1 {
		max = 1
	}
	return &InstanceStore{max: max, instances: make(map[string]*Buffer)}
}

// Register creates an instance stream. Existing lines are retained across
// restarts and configuration updates.
func (s *InstanceStore) Register(name string) {
	if s == nil || name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.instances[name]; !ok {
		s.instances[name] = New(s.max)
	}
}

func (s *InstanceStore) Rename(oldName, newName string) {
	if s == nil || oldName == "" || newName == "" || oldName == newName {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	buf, ok := s.instances[oldName]
	if !ok {
		return
	}
	delete(s.instances, oldName)
	s.instances[newName] = buf
}

func (s *InstanceStore) Remove(name string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.instances, name)
	s.mu.Unlock()
}

// AppendGlobal routes only lines that include an exact InstanceTag. This is
// intentionally strict: a global message, another instance's run ID, or a
// coincidental proxy/name substring must not appear in an instance log.
func (s *InstanceStore) AppendGlobal(line string) {
	if s == nil || line == "" {
		return
	}
	const marker = "[frp-more:"
	start := strings.Index(line, marker)
	if start == -1 {
		return
	}
	nameStart := start + len(marker)
	nameEnd := strings.IndexByte(line[nameStart:], ']')
	if nameEnd == -1 {
		return
	}
	s.Append(line[nameStart:nameStart+nameEnd], line)
}

func (s *InstanceStore) Append(name, line string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	buf := s.instances[name]
	s.mu.RUnlock()
	if buf != nil {
		buf.Append(line)
	}
}

func (s *InstanceStore) Snapshot(name string) (lines []string, dropped int, ok bool) {
	if s == nil {
		return nil, 0, false
	}
	s.mu.RLock()
	buf, ok := s.instances[name]
	s.mu.RUnlock()
	if !ok {
		return nil, 0, false
	}
	lines, dropped = buf.Snapshot()
	return lines, dropped, true
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
	console   consoleLike
	buf       *Buffer
	instances *InstanceStore
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
		w.instances.AppendGlobal(string(p))
	}
	return n, err
}

// Attach replaces frp's global logger output with a capturing writer and
// returns the buffer feeding from it. Call once at startup, after InitLogger.
func Attach(maxLines int) *Buffer {
	return AttachWithInstances(maxLines, nil)
}

// AttachWithInstances installs the global capture and optionally routes
// matching lines to per-instance buffers as well.
func AttachWithInstances(maxLines int, instances *InstanceStore) *Buffer {
	buf := New(maxLines)
	console := goliblog.NewConsoleWriter(goliblog.ConsoleConfig{Colorful: true}, os.Stdout).(*goliblog.ConsoleWriter)
	frplog.Logger = frplog.Logger.WithOptions(goliblog.WithOutput(&captureWriter{
		console:   console,
		buf:       buf,
		instances: instances,
	}))
	return buf
}
