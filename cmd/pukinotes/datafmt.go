package main

// The data folder (~/.pukinotes) carries a format number. Code updates arrive
// with git pull; this file makes sure old data keeps working with new code.
//
// Rules for future changes:
//   - Changing what's on disk (files in ~/.pukinotes, board folders, peers.json…)
//     means: bump dataFormat and add a migration from the previous number.
//   - Changing what's inside a board (Yjs fields) must stay backward compatible:
//     add new fields freely; renaming or removing needs a migration in the UI
//     (see BOARD_SCHEMA in web/src/main.js).

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const dataFormat = 1

// migrations[n] upgrades a data folder from format n to n+1.
var migrations = map[int]func(home string) error{}

func ensureDataFormat(home string) error {
	p := filepath.Join(home, "format.json")
	cur := dataFormat
	if b, err := os.ReadFile(p); err == nil {
		var f struct {
			Format int `json:"format"`
		}
		if err := json.Unmarshal(b, &f); err != nil || f.Format < 1 {
			return fmt.Errorf("%s is damaged; restore it from %s or delete it", p, filepath.Join(home, "backups"))
		}
		cur = f.Format
	} else if _, err := os.Stat(filepath.Join(home, "identity.json")); err == nil {
		cur = 1 // data from v0.1, before format.json existed
	}
	if cur > dataFormat {
		return fmt.Errorf("your data was saved by a newer PukiNotes (data format %d, this copy knows %d). Update first: git pull", cur, dataFormat)
	}
	if cur < dataFormat {
		dst := filepath.Join(home, "backups", fmt.Sprintf("format-%d-%s", cur, time.Now().Format("20060102-150405")))
		if err := copyTree(home, dst, filepath.Join(home, "backups")); err != nil {
			return fmt.Errorf("backing up before the data upgrade failed: %w", err)
		}
		for ; cur < dataFormat; cur++ {
			m := migrations[cur]
			if m == nil {
				return fmt.Errorf("no upgrade from data format %d", cur)
			}
			if err := m(home); err != nil {
				return fmt.Errorf("upgrading data from format %d failed (your backup is in %s): %w", cur, dst, err)
			}
			if err := writeFormat(p, cur+1); err != nil {
				return err
			}
		}
	}
	return writeFormat(p, cur)
}

func writeFormat(p string, n int) error {
	b, _ := json.Marshal(map[string]any{"format": n, "note": "PukiNotes data format. Don't edit."})
	return os.WriteFile(p, b, 0o600)
}

// copyTree copies src into dst, skipping the skip directory.
func copyTree(src, dst, skip string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == skip {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(src, path)
		to := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
}
