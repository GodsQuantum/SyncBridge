package main

import (
	"strings"
	"testing"
	"time"
)

func TestLogBufferEvictsOldestBytes(t *testing.T) {
	b := NewLogBuffer(LogLimits{MaxBytes: 8, MaxLineBytes: 6})
	b.Append([]byte("123456\n"))
	b.Append([]byte("abcdef\n"))
	if got := string(b.Bytes()); got != "abcdef\n" {
		t.Fatalf("Bytes() = %q, want %q", got, "abcdef\n")
	}
}

func TestLogBufferBoundsLineAndNormalizesNewlines(t *testing.T) {
	b := NewLogBuffer(LogLimits{MaxBytes: 64, MaxLineBytes: 4})
	b.Append([]byte("123456\r\nxy"))
	if got := string(b.Bytes()); got != "1234\nxy\n" {
		t.Fatalf("Bytes() = %q", got)
	}
}

func TestLogBufferExpiresOldEntries(t *testing.T) {
	b := NewLogBuffer(LogLimits{MaxBytes: 64, MaxLineBytes: 16})
	b.AppendAt(time.Unix(1, 0), []byte("old\n"))
	b.AppendAt(time.Unix(10, 0), []byte("new\n"))
	b.TrimBefore(time.Unix(5, 0))
	if got := string(b.Bytes()); got != "new\n" {
		t.Fatalf("Bytes() = %q", got)
	}
}

func TestLogBufferDoesNotGrowForOversizedLine(t *testing.T) {
	b := NewLogBuffer(LogLimits{MaxBytes: 16, MaxLineBytes: 4})
	b.Append([]byte(strings.Repeat("x", 100)))
	if got := string(b.Bytes()); got != "xxxx\n" {
		t.Fatalf("Bytes() = %q", got)
	}
}

func TestLogBufferKeepsEmptyLinesForLFCRAndCRLF(t *testing.T) {
	b := NewLogBuffer(LogLimits{MaxBytes: 64, MaxLineBytes: 16})
	b.Append([]byte("a\n\nb\rc\r\nd"))
	if got := string(b.Bytes()); got != "a\n\nb\nc\nd\n" {
		t.Fatalf("Bytes() = %q", got)
	}
}
