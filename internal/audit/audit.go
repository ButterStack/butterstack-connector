// Package audit writes the connector's local audit log: one line per command,
// including every rejected and denied one.
//
// The log is the studio's evidence, not ours. It stays on the studio's disk and
// no verb can read it. Arguments are recorded as a SHA-256 of their canonical
// JSON rather than verbatim, so the log is useful for correlating with our side
// ("we sent command X, they ran command X") without becoming a second copy of
// whatever the arguments contained.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one audit line.
type Entry struct {
	TS         string `json:"ts"`
	Event      string `json:"event"`
	SessionID  string `json:"session_id,omitempty"`
	CommandID  string `json:"command_id,omitempty"`
	Verb       string `json:"verb,omitempty"`
	ArgsSHA256 string `json:"args_sha256,omitempty"`
	Status     string `json:"status,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Detail     string `json:"detail,omitempty"`
	BytesOut   int    `json:"bytes_out,omitempty"`
	DurationMS int64  `json:"duration_ms,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Logger appends JSON lines to a dated file and mirrors them to stderr.
type Logger struct {
	mu     sync.Mutex
	dir    string
	day    string
	file   *os.File
	mirror io.Writer
}

// New opens (or creates) the audit directory.
func New(dir string, mirror io.Writer) (*Logger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: %w", err)
	}
	return &Logger{dir: dir, mirror: mirror}, nil
}

func (l *Logger) rotate(now time.Time) error {
	day := now.UTC().Format("2006-01-02")
	if l.file != nil && l.day == day {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
	}
	path := filepath.Join(l.dir, "audit-"+day+".log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("audit: %w", err)
	}
	l.file, l.day = f, day
	return nil
}

// Write emits one entry.
func (l *Logger) Write(e Entry) {
	now := time.Now().UTC()
	if e.TS == "" {
		e.TS = now.Format(time.RFC3339Nano)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.rotate(now); err == nil && l.file != nil {
		_, _ = l.file.Write(b)
	}
	if l.mirror != nil {
		_, _ = l.mirror.Write(b)
	}
}

// Event records a non-command line (connect, reconnect, revoke, shutdown).
func (l *Logger) Event(event, message string) {
	l.Write(Entry{Event: event, Message: message})
}

// Close flushes and closes the current file.
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// ArgsDigest is the canonical hash recorded for a command's arguments.
func ArgsDigest(raw []byte) string {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	// Canonicalise through a map so key order does not change the digest.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err == nil {
		if c, err := json.Marshal(m); err == nil {
			raw = c
		}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
