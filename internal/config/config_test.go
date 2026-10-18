package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTripKeepsSecretsPrivate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "etc", "agent.json")
	in := Config{APIURL: "https://api.example.com", Token: "irna_x", ARIPassword: "pw"}
	if err := Save(p, in); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	out, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if out.Token != "irna_x" || out.AsteriskDir != DefaultAsteriskDir || out.ARIURL != DefaultARIURL || out.App != DefaultApp || out.ARIUser != DefaultARIUser {
		t.Errorf("%+v", out)
	}
	if err := out.Validate(); err != nil {
		t.Error(err)
	}
	if (Config{APIURL: "x", Token: "y"}).Validate() == nil {
		t.Error("a config without the ARI password must not run")
	}
}
