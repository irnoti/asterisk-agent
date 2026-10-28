package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// scripts/pjsip-webrtc-setup.sh pins the sha256 of every prebuilt table schema it downloads and runs as the database root.
// This keeps the pins, the files and the "newest per major" table in step, and checks the files hold only table creation.
func TestPrebuiltSchemaPinsMatchTheFiles(t *testing.T) {
	script, err := os.ReadFile("../../scripts/pjsip-webrtc-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, m := range regexp.MustCompile(`(ps-schema-[0-9.]+\.sql)\) echo ([0-9a-f]{64});;`).FindAllStringSubmatch(string(script), -1) {
		pins[m[1]] = m[2]
	}
	files, _ := filepath.Glob("../../scripts/pjsip-schema/ps-schema-*.sql")
	if len(files) == 0 || len(files) != len(pins) {
		t.Fatalf("%d schema files but %d pins in the script", len(files), len(pins))
	}
	stmt := regexp.MustCompile(`^(CREATE TABLE|ALTER TABLE|CREATE INDEX|CREATE UNIQUE INDEX) |^(UPDATE|INSERT INTO) alembic_version `)
	for _, f := range files {
		name := filepath.Base(f)
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(raw)
		if pins[name] != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: the pin in the script is %q, the file hashes to %x (re-run scripts/gen-pjsip-schema.sh and update the pin)", name, pins[name], sum)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if line == "" || strings.HasPrefix(line, "--") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, ")") {
				continue
			}
			if !stmt.MatchString(line) {
				t.Errorf("%s:%d: unexpected statement %q", name, i+1, line)
			}
		}
	}
	// every "newest per major" entry must be a shipped file
	for _, m := range regexp.MustCompile(`([0-9]+\)) echo ([0-9]+\.[0-9]+\.[0-9]+);;`).FindAllStringSubmatch(string(script), -1) {
		if _, ok := pins["ps-schema-"+m[2]+".sql"]; !ok {
			t.Errorf("schema_latest names %s, but no such file is pinned", m[2])
		}
	}
}
