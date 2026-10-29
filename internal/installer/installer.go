// Package installer writes the Asterisk configuration the platform needs, in files of its own and managed blocks, with backups and rollback.
package installer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/irnoti/asterisk-agent/internal/api"
	"github.com/irnoti/asterisk-agent/internal/config"
)

const (
	IncludeDir   = "irnoti"
	ariFile      = "ari_irnoti.conf"
	dialplanFile = "extensions_irnoti.conf"
	BackupRoot   = "/var/lib/irnoti-asterisk/backups"
	maxFiles     = 10
	maxFileBytes = 128 << 10
	tagARI       = "ari"
	tagHTTP      = "http"
	tagDialplan  = "dialplan"
	// TagPJSIP and TagRTP mark the blocks the setup wizard adds to pjsip.conf (or pjsip_custom.conf) and rtp.conf.
	TagPJSIP      = "pjsip"
	TagRTP        = "rtp"
	manifestName  = "manifest.json"
	systemdUnit   = "/etc/systemd/system/irnoti-asterisk.service"
	defaultBinary = "/usr/local/bin/irnoti-asterisk"
)

var fileName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}\.conf$`)

// Runner runs external commands (asterisk, systemctl); tests replace it.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// Installer holds everything Install/Sync/Uninstall share. Paths are fields so tests can point them at a temp dir.
type Installer struct {
	Cfg        config.Config
	ConfigPath string
	Runner     Runner
	Log        *slog.Logger
	BackupRoot string
	UnitPath   string
	Binary     string
	DryRun     bool
	NoService  bool
	// Extra files are written in the same transaction as the install (one backup, one reload, one rollback); the setup wizard uses it for PJSIP and RTP.
	Extra []FileWrite
	// ExtraReload are further `asterisk -rx` commands run after the standard reload, e.g. "module reload res_pjsip.so".
	ExtraReload []string
	// BeforeReload runs after the files are written and before Asterisk reloads (e.g. free a port another module holds). OnRollback undoes it
	// when the install is rolled back. ExtraVerify is a second check, after ARI answers, that the extra configuration really loaded:
	// `module reload` reports success even when PJSIP rejected an object.
	BeforeReload func(ctx context.Context) error
	OnRollback   func(ctx context.Context)
	ExtraVerify  func(ctx context.Context) error
	// Verify checks that ARI answers with the agent's credentials after the reload.
	Verify func(ctx context.Context) error
	Now    func() time.Time
}

func (in *Installer) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Installer) backupRoot() string {
	if in.BackupRoot != "" {
		return in.BackupRoot
	}
	return BackupRoot
}

type manifest struct {
	Created  []string          `json:"created"`
	Modified map[string]string `json:"modified"` // original path → backup copy
}

func (in *Installer) path(parts ...string) string {
	return filepath.Join(append([]string{in.Cfg.AsteriskDir}, parts...)...)
}

func (in *Installer) extensionsFile() string {
	if in.Cfg.ExtensionsFile != "" {
		return in.Cfg.ExtensionsFile
	}
	// FreePBX/Issabel regenerate extensions.conf, so their own include point is the safe place for custom dialplan
	if _, err := os.Stat(in.path("extensions_custom.conf")); err == nil {
		return in.path("extensions_custom.conf")
	}
	return in.path("extensions.conf")
}

func newPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidateBundle refuses anything but a few small, well-named, checksummed .conf files with a safe dialplan.
func ValidateBundle(b api.Bundle) error {
	if len(b.Files) == 0 {
		return errors.New("the API sent no files")
	}
	if len(b.Files) > maxFiles {
		return fmt.Errorf("too many files (%d)", len(b.Files))
	}
	seen := map[string]bool{}
	for _, f := range b.Files {
		if !fileName.MatchString(f.Name) {
			return fmt.Errorf("file name %q is not allowed", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("file %q appears twice", f.Name)
		}
		seen[f.Name] = true
		if len(f.Content) > maxFileBytes {
			return fmt.Errorf("%s is too large", f.Name)
		}
		sum := sha256.Sum256([]byte(f.Content))
		if hex.EncodeToString(sum[:]) != strings.ToLower(f.Sha256) {
			return fmt.Errorf("%s does not match its checksum", f.Name)
		}
		if f.Name == ariFile {
			return fmt.Errorf("%s is written by the agent itself", ariFile) // the API must never set ARI credentials
		}
		if err := ValidateDialplan(f.Name, f.Content); err != nil {
			return err
		}
	}
	return nil
}

var (
	reEnabled  = regexp.MustCompile(`(?m)^\s*enabled\s*=\s*yes`)
	reBindPort = regexp.MustCompile(`(?m)^\s*bindport\s*=\s*(\d+)`)
	reBindAddr = regexp.MustCompile(`(?m)^\s*bindaddr\s*=\s*(\S+)`)
)

// ariURLFrom reads the URL ARI is reachable at from an existing http.conf (FreePBX often already serves 8088).
func ariURLFrom(httpConf string) string {
	port, host := "8088", "127.0.0.1"
	if m := reBindPort.FindStringSubmatch(httpConf); m != nil {
		port = m[1]
	}
	if m := reBindAddr.FindStringSubmatch(httpConf); m != nil && m[1] != "0.0.0.0" && m[1] != "::" {
		host = m[1]
	}
	return "http://" + host + ":" + port
}

func readOrEmpty(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// plan is the set of file writes an install makes; applying it and dry-running it share the same list.
type FileWrite struct {
	Path    string
	Content string
	Mode    os.FileMode
}

type write struct {
	path    string
	content string
	mode    os.FileMode
}

// Install applies the configuration, reloads Asterisk, checks ARI, and rolls everything back if any of that fails.
func (in *Installer) Install(ctx context.Context, bundle api.Bundle) error {
	if err := ValidateBundle(bundle); err != nil {
		return fmt.Errorf("refusing the configuration from the API: %w", err)
	}
	if st, err := os.Stat(in.Cfg.AsteriskDir); err != nil || !st.IsDir() {
		return fmt.Errorf("Asterisk config directory %s not found (use --asterisk-dir)", in.Cfg.AsteriskDir)
	}
	if in.Cfg.ARIPassword == "" {
		pw, err := newPassword()
		if err != nil {
			return err
		}
		in.Cfg.ARIPassword = pw
	}

	httpConf, err := readOrEmpty(in.path("http.conf"))
	if err != nil {
		return err
	}
	ariConf, err := readOrEmpty(in.path("ari.conf"))
	if err != nil {
		return err
	}
	extPath := in.extensionsFile()
	extConf, err := readOrEmpty(extPath)
	if err != nil {
		return err
	}

	var writes []write
	// the agent's own ARI user, in a file of its own
	writes = append(writes, write{in.path(IncludeDir, ariFile), fmt.Sprintf("[%s]\ntype=user\nread_only=no\npassword=%s\npassword_format=plain\n", in.Cfg.ARIUser, in.Cfg.ARIPassword), 0o640})
	for _, f := range bundle.Files {
		writes = append(writes, write{in.path(IncludeDir, f.Name), f.Content, 0o644})
	}
	// ari.conf only gets an include line
	writes = append(writes, write{in.path("ari.conf"), UpsertBlock(ariConf, tagARI, fmt.Sprintf("#include \"%s/%s\"", IncludeDir, ariFile)), 0o644})
	// http.conf: leave a working setup alone, otherwise enable the local-only server
	if reEnabled.MatchString(RemoveBlock(httpConf, tagHTTP)) {
		in.Cfg.ARIURL = ariURLFrom(RemoveBlock(httpConf, tagHTTP))
	} else {
		in.Cfg.ARIURL = config.DefaultARIURL
		writes = append(writes, write{in.path("http.conf"), InsertInSection(httpConf, "general", tagHTTP, "enabled=yes\nbindaddr=127.0.0.1\nbindport=8088"), 0o644})
	}
	for _, f := range bundle.Files {
		if f.Name == dialplanFile {
			writes = append(writes, write{extPath, UpsertBlock(extConf, tagDialplan, fmt.Sprintf("#include \"%s/%s\"", IncludeDir, dialplanFile)), 0o644})
		}
	}

	for _, f := range in.Extra {
		writes = append(writes, write{f.Path, f.Content, f.Mode})
	}

	if in.DryRun {
		for _, w := range writes {
			in.Log.Info("would write", "file", w.path, "bytes", len(w.content))
		}
		return nil
	}

	man, err := in.apply(writes)
	if err != nil {
		in.rollbackManifest(man)
		return err
	}
	undo := func() {
		in.rollbackManifest(man)
		if in.OnRollback != nil {
			in.OnRollback(ctx)
		}
		_ = in.reload(ctx)
	}
	if in.BeforeReload != nil {
		if err := in.BeforeReload(ctx); err != nil {
			undo()
			return fmt.Errorf("could not prepare Asterisk for the new configuration (rolled back): %w", err)
		}
	}
	if err := in.reload(ctx); err != nil {
		undo()
		return fmt.Errorf("Asterisk did not accept the configuration (rolled back): %w", err)
	}
	if in.Verify != nil {
		if err := in.Verify(ctx); err != nil {
			undo()
			return fmt.Errorf("ARI did not come up with the agent's credentials (rolled back): %w", err)
		}
	}
	if in.ExtraVerify != nil {
		if err := in.ExtraVerify(ctx); err != nil {
			undo()
			return fmt.Errorf("the configuration loaded but is not working (rolled back): %w", err)
		}
	}
	if err := config.Save(in.ConfigPath, in.Cfg); err != nil {
		return err
	}
	if !in.NoService {
		if err := in.installService(ctx); err != nil {
			return fmt.Errorf("configuration is in place, but the service could not be started: %w", err)
		}
	}
	return nil
}

// apply writes every file after saving a copy of the ones that exist, and records what it did so it can be undone.
func (in *Installer) apply(writes []write) (string, error) {
	dir := filepath.Join(in.backupRoot(), in.now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	man := manifest{Modified: map[string]string{}}
	for i, w := range writes {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o755); err != nil {
			return dir, err
		}
		old, err := os.ReadFile(w.path)
		switch {
		case err == nil:
			bak := filepath.Join(dir, strconv.Itoa(i)+"-"+filepath.Base(w.path))
			if err := os.WriteFile(bak, old, 0o600); err != nil {
				return dir, err
			}
			man.Modified[w.path] = bak
		case errors.Is(err, os.ErrNotExist):
			man.Created = append(man.Created, w.path)
		default:
			return dir, err
		}
		if err := os.WriteFile(w.path, []byte(w.content), w.mode); err != nil {
			in.saveManifest(dir, man)
			return dir, err
		}
		// the ARI file holds a password: Asterisk (its group) may read it, nobody else
		if w.mode == 0o640 {
			if g, err := user.LookupGroup("asterisk"); err == nil {
				if gid, err := strconv.Atoi(g.Gid); err == nil {
					_ = os.Chown(w.path, -1, gid)
				}
			}
		}
	}
	return dir, in.saveManifest(dir, man)
}

func (in *Installer) saveManifest(dir string, m manifest) error {
	raw, _ := json.MarshalIndent(m, "", "  ")
	return os.WriteFile(filepath.Join(dir, manifestName), raw, 0o600)
}

func (in *Installer) rollbackManifest(dir string) {
	if dir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		in.Log.Error("cannot read the backup manifest", "dir", dir, "err", err)
		return
	}
	var m manifest
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for path, bak := range m.Modified {
		if data, err := os.ReadFile(bak); err == nil {
			_ = os.WriteFile(path, data, 0o644)
		}
	}
	for _, path := range m.Created {
		_ = os.Remove(path)
	}
}

func (in *Installer) reload(ctx context.Context) error {
	for _, cmd := range append([]string{"module reload http", "module reload res_ari.so", "dialplan reload"}, in.ExtraReload...) {
		if out, err := in.Runner.Run(ctx, "asterisk", "-rx", cmd); err != nil {
			return fmt.Errorf("%q: %v %s", cmd, err, out)
		}
	}
	return nil
}

// Sync applies a newer dialplan bundle while the daemon runs: only changed files are written and only the dialplan is reloaded.
func (in *Installer) Sync(ctx context.Context, bundle api.Bundle) (bool, error) {
	if err := ValidateBundle(bundle); err != nil {
		return false, fmt.Errorf("refusing the configuration from the API: %w", err)
	}
	var writes []write
	for _, f := range bundle.Files {
		p := in.path(IncludeDir, f.Name)
		if cur, err := os.ReadFile(p); err == nil && string(cur) == f.Content {
			continue
		}
		writes = append(writes, write{p, f.Content, 0o644})
	}
	if len(writes) == 0 {
		return false, nil
	}
	man, err := in.apply(writes)
	if err != nil {
		in.rollbackManifest(man)
		return false, err
	}
	if out, err := in.Runner.Run(ctx, "asterisk", "-rx", "dialplan reload"); err != nil {
		in.rollbackManifest(man)
		_, _ = in.Runner.Run(ctx, "asterisk", "-rx", "dialplan reload")
		return false, fmt.Errorf("dialplan reload failed (rolled back): %v %s", err, out)
	}
	return true, nil
}

// Rollback restores the files saved by the latest install/sync backup.
func (in *Installer) Rollback(ctx context.Context) error {
	entries, err := os.ReadDir(in.backupRoot())
	if err != nil || len(entries) == 0 {
		return errors.New("no backup found")
	}
	latest := entries[len(entries)-1].Name()
	in.rollbackManifest(filepath.Join(in.backupRoot(), latest))
	return in.reload(ctx)
}

// Uninstall removes the managed blocks and the irnoti/ directory, reloads Asterisk and stops the service.
func (in *Installer) Uninstall(ctx context.Context, purge bool) error {
	for _, f := range []struct{ file, tag string }{{in.path("ari.conf"), tagARI}, {in.path("http.conf"), tagHTTP}, {in.extensionsFile(), tagDialplan},
		{in.path("pjsip.conf"), TagPJSIP}, {in.path("pjsip_custom.conf"), TagPJSIP}, {in.path("rtp.conf"), TagRTP}} {
		cur, err := readOrEmpty(f.file)
		if err != nil || !HasBlock(cur, f.tag) {
			continue
		}
		if err := os.WriteFile(f.file, []byte(RemoveBlock(cur, f.tag)), 0o644); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(in.path(IncludeDir)); err != nil {
		return err
	}
	if !in.NoService {
		_, _ = in.Runner.Run(ctx, "systemctl", "disable", "--now", "irnoti-asterisk")
		_ = os.Remove(in.unitPath())
		_, _ = in.Runner.Run(ctx, "systemctl", "daemon-reload")
	}
	if purge {
		_ = os.Remove(in.ConfigPath)
	}
	return in.reload(ctx)
}

func (in *Installer) unitPath() string {
	if in.UnitPath != "" {
		return in.UnitPath
	}
	return systemdUnit
}

func (in *Installer) binary() string {
	if in.Binary != "" {
		return in.Binary
	}
	if exe, err := os.Executable(); err == nil {
		return exe
	}
	return defaultBinary
}

// rwDirective is the unit setting that lists writable paths: systemd 231 renamed ReadWriteDirectories to ReadWritePaths, and older
// systemd (CentOS 7 ships 219) rejects the new name with "Unknown lvalue", leaving the whole service read-only.
func rwDirective(systemdVersion int) string {
	if systemdVersion > 0 && systemdVersion < 231 {
		return "ReadWriteDirectories"
	}
	return "ReadWritePaths"
}

// systemdVersion reads the number from `systemctl --version` ("systemd 219"); 0 when it cannot be told.
func systemdVersion(out string) int {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "systemd" {
		return 0
	}
	n, err := strconv.Atoi(f[1])
	if err != nil {
		return 0
	}
	return n
}

// UnitFile is the systemd service. It runs as root only because it edits /etc/asterisk and reloads Asterisk; everything else is locked down.
// soundsDir is listed only when it exists: a path that is missing makes older systemd refuse to start the service.
func UnitFile(binary, configPath string, systemdVersion int, soundsDir string) string {
	paths := []string{"/etc/asterisk", "/var/lib/irnoti-asterisk", "/etc/irnoti-asterisk"}
	if st, err := os.Stat(soundsDir); err == nil && st.IsDir() {
		paths = append(paths, soundsDir)
	}
	return fmt.Sprintf(`[Unit]
Description=IRNoti Asterisk agent
After=network-online.target asterisk.service
Wants=network-online.target

[Service]
ExecStart=%s run --config %s
Restart=always
RestartSec=5
NoNewPrivileges=true
ProtectSystem=full
ProtectHome=true
PrivateTmp=true
%s=%s

[Install]
WantedBy=multi-user.target
`, binary, configPath, rwDirective(systemdVersion), strings.Join(paths, " "))
}

func (in *Installer) soundsDir() string {
	if in.Cfg.SoundsDir != "" {
		return in.Cfg.SoundsDir
	}
	return config.DefaultSoundsDir
}

func (in *Installer) installService(ctx context.Context) error {
	out, _ := in.Runner.Run(ctx, "systemctl", "--version")
	if err := os.WriteFile(in.unitPath(), []byte(UnitFile(in.binary(), in.ConfigPath, systemdVersion(out), in.soundsDir())), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", "irnoti-asterisk"}} {
		if out, err := in.Runner.Run(ctx, "systemctl", args...); err != nil {
			return fmt.Errorf("systemctl %s: %v %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}
