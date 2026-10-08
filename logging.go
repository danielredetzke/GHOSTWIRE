package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

var logLevel = new(slog.LevelVar)

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

// rotatingWriter appends to <app>.jsonl and rotates it to .1, .2, ... when it
// grows past maxBytes, keeping maxFiles old files.
type rotatingWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	maxFiles int
	f        *os.File
	size     int64
}

func newRotatingWriter(path string, maxMB, maxFiles int) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, maxBytes: int64(maxMB) << 20, maxFiles: maxFiles}
	return w, w.open()
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f, w.size = f, st.Size()
	return nil
}

func (w *rotatingWriter) rotate() error {
	w.f.Close()
	for i := w.maxFiles - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", w.path, w.maxFiles+1))
	if err := os.Rename(w.path, w.path+".1"); err != nil {
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		if err := w.rotate(); err != nil {
			fmt.Fprintln(os.Stderr, "log rotation failed:", err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// SetLimits changes rotation size and file count at runtime. Rotated files
// beyond the new count are deleted.
func (w *rotatingWriter) SetLimits(maxMB, maxFiles int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.maxBytes = int64(maxMB) << 20
	for i := maxFiles + 1; i <= maxLogFiles+1; i++ {
		_ = os.Remove(fmt.Sprintf("%s.%d", w.path, i))
	}
	w.maxFiles = maxFiles
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func setupLogging(path string, c LogConfig) (*rotatingWriter, error) {
	w, err := newRotatingWriter(path, c.MaxSizeMB, c.MaxFiles)
	if err != nil {
		return nil, err
	}
	logLevel.Set(parseLevel(c.Level))
	slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: logLevel})))
	return w, nil
}

// readLogTail returns up to limit lines, newest first. level filters to that
// level and above; only keeps the records marked "audit" (changes) or "dns"
// (DNS queries) when set.
func readLogTail(path string, limit int, level, only string) ([]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	const window = 1 << 20
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(st.Size()-window, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(b, []byte("\n"))
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is probably cut
	}
	min := slog.Level(-100)
	if level != "" && level != "all" {
		min = parseLevel(level)
	}
	out := []json.RawMessage{}
	for i := len(lines) - 1; i >= 0 && len(out) < limit; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var rec struct {
			Level string `json:"level"`
			Audit bool   `json:"audit"`
			DNS   bool   `json:"dns"`
		}
		if json.Unmarshal(line, &rec) != nil {
			continue
		}
		if (only == "audit" && !rec.Audit) || (only == "dns" && !rec.DNS) {
			continue
		}
		if parseLevel(strings.ToLower(rec.Level)) < min {
			continue
		}
		out = append(out, json.RawMessage(bytes.Clone(line)))
	}
	return out, nil
}
