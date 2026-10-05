package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateBackups(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	_ = os.WriteFile(cfg, []byte("{}"), 0o600)
	now := time.Date(2026, 10, 5, 12, 9, 0, 0, time.UTC)

	// A second copy of the same version gets the time appended instead of
	// overwriting the first.
	first := newUpdateBackupPath(cfg, "unknown", now)
	if filepath.Base(first) != "config.json.bak-unknown" {
		t.Fatalf("first copy: %s", first)
	}
	_ = os.WriteFile(first, []byte("{}"), 0o600)
	second := newUpdateBackupPath(cfg, "unknown", now)
	if filepath.Base(second) != "config.json.bak-unknown-20261005-1209" {
		t.Fatalf("second copy: %s", second)
	}
	_ = os.WriteFile(second, []byte("{}"), 0o600)
	for i, v := range []string{"v0.2.0", "v0.3.0", "v0.4.0"} {
		f := filepath.Join(dir, "config.json.bak-"+v)
		_ = os.WriteFile(f, []byte("{}"), 0o600)
		_ = os.Chtimes(f, now, now.Add(time.Duration(i+1)*time.Hour))
	}
	_ = os.Chtimes(first, now, now.Add(-2*time.Hour))
	_ = os.Chtimes(second, now, now.Add(-time.Hour))
	_ = os.Mkdir(filepath.Join(dir, "config.json.bak-dir"), 0o700) // not a file: ignored

	list, err := listUpdateBackups(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range list {
		got = append(got, b.Version)
	}
	if strings.Join(got, " ") != "v0.4.0 v0.3.0 v0.2.0 unknown unknown" {
		t.Fatalf("versions, newest first: %v", got)
	}

	for _, bad := range []string{"config.json", "config.json.bak-", "config.json.bak-dir", "../config.json.bak-v0.4.0", "config.json.bak-v0.4.0/x"} {
		if err := removeUpdateBackup(cfg, bad); err == nil {
			t.Errorf("removed %q", bad)
		}
	}

	if n, err := pruneUpdateBackups(cfg, keepUpdateBackups); err != nil || n != 2 {
		t.Fatalf("prune: %d, %v", n, err)
	}
	if list, _ = listUpdateBackups(cfg); len(list) != 3 || list[2].Version != "v0.2.0" {
		t.Fatalf("after prune: %v", list)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Fatal("config.json is gone")
	}
}
