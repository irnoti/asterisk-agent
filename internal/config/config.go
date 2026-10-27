// Package config is the agent's own settings file (/etc/irnoti-asterisk/agent.json).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DefaultPath        = "/etc/irnoti-asterisk/agent.json"
	DefaultAsteriskDir = "/etc/asterisk"
	DefaultSoundsDir   = "/var/lib/asterisk/sounds"
	DefaultARIURL      = "http://127.0.0.1:8088"
	DefaultApp         = "irnoti"
	DefaultARIUser     = "irnoti"
)

// Config is everything the daemon needs to run. The ARI password is generated on this machine and never sent to the API:
// the agent adds it itself when it forwards a request to the local ARI.
type Config struct {
	APIURL      string `json:"apiUrl"`
	Token       string `json:"token"`
	AsteriskDir string `json:"asteriskDir"`
	ARIURL      string `json:"ariUrl"`
	ARIUser     string `json:"ariUser"`
	ARIPassword string `json:"ariPassword"`
	App         string `json:"app"`
	// ExtensionsFile is the dialplan file that carries the managed #include of irnoti/extensions_irnoti.conf.
	ExtensionsFile string `json:"extensionsFile"`
	// SoundsDir is where Asterisk reads prompts from; the agent writes only inside its irnoti-sync subdirectory.
	SoundsDir string `json:"soundsDir"`
}

// WithDefaults fills the fields an older or hand-written file may leave empty.
func (c Config) WithDefaults() Config {
	if c.AsteriskDir == "" {
		c.AsteriskDir = DefaultAsteriskDir
	}
	if c.SoundsDir == "" {
		c.SoundsDir = DefaultSoundsDir
	}
	if c.ARIURL == "" {
		c.ARIURL = DefaultARIURL
	}
	if c.ARIUser == "" {
		c.ARIUser = DefaultARIUser
	}
	if c.App == "" {
		c.App = DefaultApp
	}
	return c
}

// Validate reports the first missing field the daemon cannot run without.
func (c Config) Validate() error {
	switch {
	case c.APIURL == "":
		return errors.New("apiUrl is required")
	case c.Token == "":
		return errors.New("token is required")
	case c.ARIPassword == "":
		return errors.New("ariPassword is required (run `irnoti-asterisk install` first)")
	}
	return nil
}

// Load reads and defaults the settings file.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c.WithDefaults(), nil
}

// Save writes the file with owner-only permissions, because it holds the API token and the ARI password.
func Save(path string, c Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
