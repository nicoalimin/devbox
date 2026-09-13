package tui

import (
	"fmt"
	"io"
	"log"
	"sync"
	"time"
)

// LogEntry represents a single log entry
type LogEntry struct {
	Timestamp time.Time
	Level     string
	Message   string
}

// LogBuffer is a thread-safe ring buffer for capturing logs
type LogBuffer struct {
	mu      sync.RWMutex
	entries []LogEntry
	maxSize int
	pos     int
	wrapped bool
}

// NewLogBuffer creates a new log buffer with the specified capacity
func NewLogBuffer(maxSize int) *LogBuffer {
	return &LogBuffer{
		entries: make([]LogEntry, maxSize),
		maxSize: maxSize,
	}
}

// Add adds a new log entry to the buffer
func (lb *LogBuffer) Add(level, message string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.entries[lb.pos] = LogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Message:   message,
	}

	lb.pos++
	if lb.pos >= lb.maxSize {
		lb.pos = 0
		lb.wrapped = true
	}
}

// GetAll returns all log entries in chronological order
func (lb *LogBuffer) GetAll() []LogEntry {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	var result []LogEntry

	if !lb.wrapped {
		// Haven't wrapped yet, return from start to pos
		for i := 0; i < lb.pos; i++ {
			result = append(result, lb.entries[i])
		}
	} else {
		// Wrapped, return from pos to end, then from start to pos
		for i := lb.pos; i < lb.maxSize; i++ {
			result = append(result, lb.entries[i])
		}
		for i := 0; i < lb.pos; i++ {
			result = append(result, lb.entries[i])
		}
	}

	return result
}

// GetRecent returns the most recent N log entries
func (lb *LogBuffer) GetRecent(n int) []LogEntry {
	all := lb.GetAll()
	if len(all) <= n {
		return all
	}
	return all[len(all)-n:]
}

// Clear clears all log entries
func (lb *LogBuffer) Clear() {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	lb.entries = make([]LogEntry, lb.maxSize)
	lb.pos = 0
	lb.wrapped = false
}

// LogWriter implements io.Writer for capturing log output
type LogWriter struct {
	buffer *LogBuffer
	level  string
}

// Write implements io.Writer
func (lw *LogWriter) Write(p []byte) (n int, err error) {
	message := string(p)
	lw.buffer.Add(lw.level, message)
	return len(p), nil
}

// Global log buffer (initialized when TUI starts)
var globalLogBuffer *LogBuffer

// InitGlobalLogBuffer initializes the global log buffer
func InitGlobalLogBuffer(size int) {
	globalLogBuffer = NewLogBuffer(size)
}

// GetGlobalLogBuffer returns the global log buffer
func GetGlobalLogBuffer() *LogBuffer {
	return globalLogBuffer
}

// RedirectStdLog redirects the standard Go logger to the log buffer
func RedirectStdLog() {
	if globalLogBuffer == nil {
		return
	}
	
	// Redirect standard log package
	log.SetOutput(&LogWriter{buffer: globalLogBuffer, level: "INFO"})
	log.SetFlags(0) // Remove default timestamp/prefix since we add our own
}

// RestoreStdLog restores standard logging to stdout
func RestoreStdLog(w io.Writer) {
	log.SetOutput(w)
	log.SetFlags(log.LstdFlags) // Restore default flags
}

// LogInfo adds an info log entry
func LogInfo(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if globalLogBuffer != nil {
		globalLogBuffer.Add("INFO", message)
	}
}

// LogWarn adds a warning log entry
func LogWarn(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if globalLogBuffer != nil {
		globalLogBuffer.Add("WARN", message)
	}
}

// LogError adds an error log entry
func LogError(format string, args ...interface{}) {
	message := fmt.Sprintf(format, args...)
	if globalLogBuffer != nil {
		globalLogBuffer.Add("ERROR", message)
	}
}
