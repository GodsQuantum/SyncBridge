package main

import (
	"bytes"
	"sync"
	"time"
)

// LogLimits bounds retained log memory. Line limits exclude the final newline.
type LogLimits struct {
	MaxBytes     int
	MaxLineBytes int
	MaxRuns      int
	MaxAge       time.Duration
}

type logEntry struct {
	at   time.Time
	line []byte
}

// LogBuffer retains complete normalized lines and evicts the oldest data first.
type LogBuffer struct {
	mu      sync.RWMutex
	limits  LogLimits
	entries []logEntry
	bytes   int
}

func NewLogBuffer(limits LogLimits) *LogBuffer {
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = 1 << 20
	}
	if limits.MaxLineBytes <= 0 {
		limits.MaxLineBytes = MaxExecutionLogLineBytes
	}
	return &LogBuffer{limits: limits}
}

func (b *LogBuffer) Append(data []byte) { b.AppendAt(time.Now().UTC(), data) }

func (b *LogBuffer) AppendAt(at time.Time, data []byte) {
	if b == nil {
		return
	}
	for len(data) > 0 {
		end := bytes.IndexAny(data, "\r\n")
		line := data
		if end >= 0 {
			line = data[:end]
			wasCR := data[end] == '\r'
			data = data[end+1:]
			if wasCR && len(data) > 0 && data[0] == '\n' {
				data = data[1:]
			}
		} else {
			data = nil
		}
		b.appendLine(at, line)
	}
}

func (b *LogBuffer) appendLine(at time.Time, line []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(line) > b.limits.MaxLineBytes {
		line = line[:b.limits.MaxLineBytes]
	}
	line = append(append([]byte(nil), line...), '\n')
	if len(line) > b.limits.MaxBytes {
		return
	}
	b.entries = append(b.entries, logEntry{at: at, line: line})
	b.bytes += len(line)
	b.trimLocked(time.Time{})
}

func (b *LogBuffer) TrimBefore(cutoff time.Time) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.trimLocked(cutoff)
}

func (b *LogBuffer) trimLocked(cutoff time.Time) {
	for len(b.entries) > 0 && (!cutoff.IsZero() && b.entries[0].at.Before(cutoff) || b.bytes > b.limits.MaxBytes) {
		b.bytes -= len(b.entries[0].line)
		b.entries = b.entries[1:]
	}
}

func (b *LogBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make([]byte, 0, b.bytes)
	for _, entry := range b.entries {
		result = append(result, entry.line...)
	}
	return result
}
