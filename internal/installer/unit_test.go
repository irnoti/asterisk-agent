package installer

import (
	"strings"
	"testing"
)

func TestSystemdVersion(t *testing.T) {
	for out, want := range map[string]int{"systemd 219\n+PAM +AUDIT": 219, "systemd 252 (252.22-1)": 252, "": 0, "weird": 0, "systemd x": 0} {
		if got := systemdVersion(out); got != want {
			t.Errorf("systemdVersion(%q) = %d, want %d", out, got, want)
		}
	}
}

func TestUnitFileUsesTheDirectiveTheSystemdUnderstands(t *testing.T) {
	dir := t.TempDir()
	old := UnitFile("/usr/local/bin/irnoti-asterisk", "/etc/irnoti-asterisk/agent.json", 219, dir)
	if !strings.Contains(old, "\nReadWriteDirectories=") || strings.Contains(old, "ReadWritePaths") {
		t.Errorf("systemd 219 needs ReadWriteDirectories:\n%s", old)
	}
	if !strings.Contains(old, dir) {
		t.Errorf("an existing sounds directory must be writable:\n%s", old)
	}
	for _, v := range []int{231, 252, 0} {
		cur := UnitFile("/usr/local/bin/irnoti-asterisk", "/etc/irnoti-asterisk/agent.json", v, dir)
		if !strings.Contains(cur, "\nReadWritePaths=") || strings.Contains(cur, "ReadWriteDirectories") {
			t.Errorf("systemd %d needs ReadWritePaths:\n%s", v, cur)
		}
	}
	if missing := UnitFile("/b", "/c", 219, "/nonexistent/sounds"); strings.Contains(missing, "/nonexistent/sounds") {
		t.Errorf("a missing sounds directory must not be listed (old systemd would refuse to start):\n%s", missing)
	}
}
