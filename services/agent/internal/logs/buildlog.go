package logs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const logDir = "/tmp/stranger-logs"

// BuildLog captures streaming build-phase output (git clone + docker build) and
// persists it to disk so it survives agent restarts.
// Safe for concurrent writers and readers.
type BuildLog struct {
	mu   sync.Mutex
	data []byte
	done chan struct{}
	once sync.Once
	file *os.File
}

func newBuildLog(projectID string) *BuildLog {
	bl := &BuildLog{done: make(chan struct{})}
	if err := os.MkdirAll(logDir, 0o755); err == nil {
		f, err := os.OpenFile(logFilePath(projectID), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if err == nil {
			bl.file = f
		}
	}
	return bl
}

func logFilePath(projectID string) string {
	return filepath.Join(logDir, fmt.Sprintf("%s.log", projectID))
}

// Write appends data to both memory and the on-disk log file. Implements io.Writer.
func (b *BuildLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	b.data = append(b.data, p...)
	if b.file != nil {
		_, _ = b.file.Write(p)
	}
	b.mu.Unlock()
	return len(p), nil
}

// Close signals that the build is complete (success or failure).
// Safe to call multiple times.
func (b *BuildLog) Close() {
	b.once.Do(func() {
		close(b.done)
		b.mu.Lock()
		if b.file != nil {
			_ = b.file.Close()
			b.file = nil
		}
		b.mu.Unlock()
	})
}

// StreamTo streams all current content to w, polling for new data every 100ms
// until Close() is called or ctx is cancelled.
func (b *BuildLog) StreamTo(ctx context.Context, w flushWriter) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var offset int

	flush := func() {
		b.mu.Lock()
		chunk := b.data[offset:]
		b.mu.Unlock()
		if len(chunk) > 0 {
			_, _ = w.Write(chunk)
			offset += len(chunk)
			w.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-b.done:
			flush() // drain any final bytes written before Close()
			return nil
		case <-ticker.C:
			flush()
		}
	}
}

// ReadLogFile returns the persisted log bytes for a project from disk.
// Returns nil, nil if no log file exists yet.
func ReadLogFile(projectID string) ([]byte, error) {
	data, err := os.ReadFile(logFilePath(projectID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

// flushWriter is an io.Writer that also supports HTTP chunked flushing.
type flushWriter interface {
	Write([]byte) (int, error)
	Flush()
}

// LogRegistry maps project IDs to their active BuildLog.
type LogRegistry struct {
	mu   sync.RWMutex
	logs map[string]*BuildLog
}

var DefaultRegistry = &LogRegistry{logs: make(map[string]*BuildLog)}

// NewBuild creates (and registers) a fresh BuildLog for projectID,
// replacing any previous one.
func (r *LogRegistry) NewBuild(projectID string) *BuildLog {
	bl := newBuildLog(projectID)
	r.mu.Lock()
	r.logs[projectID] = bl
	r.mu.Unlock()
	return bl
}

// Get returns the current BuildLog for projectID, or nil if none is active.
func (r *LogRegistry) Get(projectID string) *BuildLog {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.logs[projectID]
}
