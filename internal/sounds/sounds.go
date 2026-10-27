// Package sounds keeps the voice prompts the platform manages (IVR menus, greetings, campaigns) on the PBX.
// The platform publishes a manifest; the agent downloads what is missing or changed, verifies each checksum and
// removes what is no longer listed. Everything lives in one directory of its own, so the files an operator put
// in the sounds directory by hand are never touched.
package sounds

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const (
	// Subdir is the only place the agent writes; prompts are played as `irnoti-sync/<name>`.
	Subdir     = "irnoti-sync"
	maxFiles   = 5000
	maxBytes   = 20 << 20
	DefaultDir = "/var/lib/asterisk/sounds"
)

var (
	nameRe = regexp.MustCompile(`^u[0-9]{1,18}/[a-z0-9][a-z0-9_-]{0,39}$`)
	// only formats Asterisk reads without a codec module
	exts = map[string]bool{"wav": true, "sln": true, "ulaw": true, "alaw": true}
)

// Item is one prompt in the manifest.
type Item struct {
	ID     string `json:"id"`
	Name   string `json:"name"` // u<userId>/<name>, no extension
	Ext    string `json:"ext"`
	Sha256 string `json:"sha256"`
	Size   int    `json:"size"`
}

type Manifest struct {
	Version string `json:"version"`
	Files   []Item `json:"files"`
}

// Fetch downloads one prompt's bytes from the platform.
type Fetch func(ctx context.Context, id string) ([]byte, error)

type Syncer struct {
	Dir   string // Asterisk's sounds directory
	Fetch Fetch
	Log   *slog.Logger
}

// Validate refuses a manifest that could write outside the managed directory or fill the disk.
func Validate(m Manifest) error {
	if len(m.Files) > maxFiles {
		return fmt.Errorf("too many files (%d)", len(m.Files))
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		switch {
		case !nameRe.MatchString(f.Name):
			return fmt.Errorf("sound name %q is not allowed", f.Name)
		case !exts[f.Ext]:
			return fmt.Errorf("sound format %q is not allowed", f.Ext)
		case len(f.Sha256) != 64 || strings.Trim(f.Sha256, "0123456789abcdef") != "":
			return fmt.Errorf("sound %q has a bad checksum", f.Name)
		case f.Size <= 0 || f.Size > maxBytes:
			return fmt.Errorf("sound %q has a bad size", f.Name)
		case f.ID == "" || strings.ContainsAny(f.ID, "/?#% "):
			return fmt.Errorf("sound %q has a bad id", f.Name)
		case seen[f.Name+"."+f.Ext]:
			return fmt.Errorf("sound %q appears twice", f.Name)
		}
		seen[f.Name+"."+f.Ext] = true
	}
	return nil
}

func (s *Syncer) root() string { return filepath.Join(s.Dir, Subdir) }

// Sync makes the managed directory match the manifest. It returns whether anything changed.
// A file that fails to download is skipped (and reported) so one bad prompt cannot block the others; the manifest version
// is then not considered applied, and the next heartbeat retries.
func (s *Syncer) Sync(ctx context.Context, m Manifest) (bool, error) {
	if err := Validate(m); err != nil {
		return false, fmt.Errorf("refusing the sound list from the API: %w", err)
	}
	if st, err := os.Stat(s.Dir); err != nil || !st.IsDir() {
		return false, fmt.Errorf("sounds directory %s not found", s.Dir)
	}
	uid, gid := owner(s.Dir)
	changed := false
	want := map[string]bool{}
	var failed []string
	for _, f := range m.Files {
		path := filepath.Join(s.root(), filepath.FromSlash(f.Name)+"."+f.Ext)
		want[path] = true
		if cur, err := os.ReadFile(path); err == nil && sum(cur) == f.Sha256 {
			continue
		}
		data, err := s.Fetch(ctx, f.ID)
		if err == nil && (len(data) != f.Size || sum(data) != f.Sha256) {
			err = errors.New("the downloaded file does not match its checksum")
		}
		if err != nil {
			s.Log.Warn("cannot update a sound", "name", f.Name, "err", err)
			failed = append(failed, f.Name)
			continue
		}
		if err := writeFile(path, data, uid, gid); err != nil {
			return changed, err
		}
		changed = true
	}
	pruned, err := s.prune(want)
	if err != nil {
		return changed, err
	}
	changed = changed || pruned
	if len(failed) > 0 {
		return changed, fmt.Errorf("%d sound(s) could not be downloaded (first: %s)", len(failed), failed[0])
	}
	return changed, nil
}

// prune deletes managed files the manifest no longer lists, then the directories left empty.
func (s *Syncer) prune(want map[string]bool) (bool, error) {
	removed := false
	var dirs []string
	err := filepath.WalkDir(s.root(), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			if p != s.root() {
				dirs = append(dirs, p)
			}
			return nil
		}
		if !want[p] {
			removed = true
			return os.Remove(p)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails (harmlessly) while the directory still holds files
	}
	return removed, err
}

func writeFile(path string, data []byte, uid, gid int) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	// Asterisk runs as its own user and must be able to read what we wrote
	_ = os.Chown(tmp, uid, gid)
	_ = os.Chown(dir, uid, gid)
	return os.Rename(tmp, path)
}

func owner(dir string) (int, int) {
	if st, err := os.Stat(dir); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			return int(sys.Uid), int(sys.Gid)
		}
	}
	return -1, -1
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
