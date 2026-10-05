package main

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Each update copies config.json to config.json.bak-<old version> next to
// it, in case the new version must be rolled back. The copies hold the same
// secrets as a backup, so the web interface lists them and can remove them,
// and update keeps only the newest few.

const keepUpdateBackups = 3

type UpdateBackup struct {
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Modified time.Time `json:"modified"`
	Size     int64     `json:"size"`
}

// A copy that would overwrite an older one gets the time appended.
var backupStampRe = regexp.MustCompile(`-\d{8}-\d{4}$`)

func updateBackupPrefix(configPath string) string { return filepath.Base(configPath) + ".bak-" }

// newUpdateBackupPath names the copy update makes of configPath.
func newUpdateBackupPath(configPath, version string, now time.Time) string {
	p := configPath + ".bak-" + version
	if _, err := os.Lstat(p); err == nil {
		p += now.Format("-20060102-1504")
	}
	return p
}

// listUpdateBackups returns the copies next to configPath, newest first.
func listUpdateBackups(configPath string) ([]UpdateBackup, error) {
	entries, err := os.ReadDir(filepath.Dir(configPath))
	if err != nil {
		return nil, err
	}
	prefix := updateBackupPrefix(configPath)
	out := []UpdateBackup{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || name == prefix {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, UpdateBackup{Name: name, Version: backupStampRe.ReplaceAllString(strings.TrimPrefix(name, prefix), ""),
			Modified: fi.ModTime(), Size: fi.Size()})
	}
	slices.SortFunc(out, func(a, b UpdateBackup) int { return b.Modified.Compare(a.Modified) })
	return out, nil
}

// removeUpdateBackup deletes one copy; any other name is refused.
func removeUpdateBackup(configPath, name string) error {
	prefix := updateBackupPrefix(configPath)
	path := filepath.Join(filepath.Dir(configPath), name)
	fi, err := os.Lstat(path)
	if !strings.HasPrefix(name, prefix) || name == prefix || strings.ContainsAny(name, `/\`) ||
		errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.Mode().IsRegular()) {
		return badRequest("no copy named %q", name)
	}
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// pruneUpdateBackups keeps the newest keep copies and deletes the rest.
func pruneUpdateBackups(configPath string, keep int) (int, error) {
	list, err := listUpdateBackups(configPath)
	if err != nil || len(list) <= keep {
		return 0, err
	}
	n := 0
	for _, b := range list[keep:] {
		if err := removeUpdateBackup(configPath, b.Name); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (a *App) listUpdateBackups(w http.ResponseWriter, r *http.Request) {
	list, err := listUpdateBackups(a.store.path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list})
}

func (a *App) removeUpdateBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := removeUpdateBackup(a.store.path, name); err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "update backup removed", "file", name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *App) removeUpdateBackups(w http.ResponseWriter, r *http.Request) {
	n, err := pruneUpdateBackups(a.store.path, 0)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.audit(r, "update backups removed", "count", n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}
