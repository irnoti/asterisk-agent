package installer

import (
	"strings"
	"testing"
)

// Stock Debian/Ubuntu http.conf has a [general] section without `enabled`; Asterisk reads only the first section of a name,
// so the options must be inside it, not in a second [general] appended at the end.
func TestInsertInSectionKeepsOneGeneral(t *testing.T) {
	in := "[general]\nservername=Asterisk\nbindaddr=127.0.0.1\n\n[other]\nx=1\n"
	out := InsertInSection(in, "general", "http", "enabled=yes")
	if strings.Count(out, "[general]") != 1 {
		t.Fatalf("second [general] added:\n%s", out)
	}
	if !(strings.Index(out, "enabled=yes") > strings.Index(out, "bindaddr=") && strings.Index(out, "enabled=yes") < strings.Index(out, "[other]")) {
		t.Fatalf("block must sit at the end of [general]:\n%s", out)
	}
	if again := InsertInSection(out, "general", "http", "enabled=yes"); again != out {
		t.Fatalf("not idempotent:\n%s", again)
	}
}

func TestInsertAtSectionStartWinsFirstMatch(t *testing.T) {
	out := InsertAtSectionStart("[general]\nrtpend=20000\n", "general", "rtp", "rtpend=10199")
	if strings.Index(out, "rtpend=10199") > strings.Index(out, "rtpend=20000") {
		t.Fatalf("our value must come first:\n%s", out)
	}
	if got := InsertAtSectionStart("", "general", "rtp", "rtpend=1"); !strings.Contains(got, "[general]") {
		t.Fatalf("section not created: %q", got)
	}
}
