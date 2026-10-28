package setup

import (
	"strings"
	"testing"
)

func TestPJSIPConfRegistration(t *testing.T) {
	a := Answers{PublicIP: "203.0.113.7", NAT: true, Trunk: Trunk{Name: "trunk", Host: "sip.example.ir", Port: 5060, Auth: AuthRegister, User: "u1", Pass: "p1", IPs: []string{"198.51.100.5", "198.51.100.6"}}}
	got := PJSIPConf(a)
	for _, want := range []string{
		"external_media_address=203.0.113.7", "context=irnoti-inbound", "outbound_auth=trunk-auth", "username=u1", "password=p1",
		"server_uri=sip:sip.example.ir:5060", "client_uri=sip:u1@sip.example.ir:5060", "match=198.51.100.5,198.51.100.6", "allow=ulaw,alaw",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestPJSIPConfIPAuthHasNoCredentials(t *testing.T) {
	got := PJSIPConf(Answers{Trunk: Trunk{Name: "t", Host: "198.51.100.5", Port: 5060, Auth: AuthIP}})
	if strings.Contains(got, "type=auth") || strings.Contains(got, "type=registration") || strings.Contains(got, "external_media_address") {
		t.Errorf("unexpected sections:\n%s", got)
	}
	if !strings.Contains(got, "match=198.51.100.5\n") {
		t.Errorf("identify should fall back to the host:\n%s", got)
	}
}

func TestPJSIPConfWithoutTrunk(t *testing.T) {
	got := PJSIPConf(Answers{Trunk: Trunk{Auth: AuthNone}})
	if strings.Contains(got, "type=endpoint") || !strings.Contains(got, "[irnoti-udp]") {
		t.Errorf("only transports expected:\n%s", got)
	}
}

func TestTrunkValidate(t *testing.T) {
	ok := Trunk{Name: "trunk", Host: "sip.example.ir", Port: 5060, Auth: AuthRegister, User: "u", Pass: "p"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*Trunk){
		"bad host":   func(x *Trunk) { x.Host = "bad host!" },
		"bad port":   func(x *Trunk) { x.Port = 0 },
		"semicolon":  func(x *Trunk) { x.Pass = "a;b" },
		"bad ip":     func(x *Trunk) { x.IPs = []string{"nope"} },
		"empty user": func(x *Trunk) { x.User = "" },
		"bad name":   func(x *Trunk) { x.Name = "a b" },
	} {
		x := ok
		mut(&x)
		if x.Validate() == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestInsertInGeneral(t *testing.T) {
	in := "; rtp\n[general]\nrtpstart=5000\n\n[ice_host_candidates]\n;192.168.1.1 => 1.2.3.4\n"
	out := InsertInGeneral(in, "rtp", "rtpstart=10000\nrtpend=10199")
	gi, ei, bi, si := strings.Index(out, "[general]"), strings.Index(out, "rtpstart=5000"), strings.Index(out, "rtpend=10199"), strings.Index(out, "[ice_host_candidates]")
	if !(gi < bi && bi < ei && ei < si) {
		t.Fatalf("block not inside [general]:\n%s", out)
	}
	if again := InsertInGeneral(out, "rtp", "rtpstart=10000\nrtpend=10199"); again != out {
		t.Errorf("not idempotent:\n%s", again)
	}
	if got := InsertInGeneral("", "rtp", "rtpend=1"); !strings.HasPrefix(got, "\n[general]") && !strings.Contains(got, "[general]\n") {
		t.Errorf("no [general] added: %q", got)
	}
}

func TestValidRTP(t *testing.T) {
	if ValidRTP(10000, 10199) != nil || ValidRTP(10000, 10198) == nil || ValidRTP(100, 200) == nil || ValidRTP(10100, 10000) == nil {
		t.Fatal("range validation wrong")
	}
}

func TestParsers(t *testing.T) {
	id, name, fam := parseOSRelease("ID=ubuntu\nPRETTY_NAME=\"Ubuntu 24.04 LTS\"\nID_LIKE=debian\n")
	if id != "ubuntu" || name != "Ubuntu 24.04 LTS" || fam != "debian" {
		t.Errorf("os-release: %s %s %s", id, name, fam)
	}
	if _, _, fam := parseOSRelease("ID=rocky\nID_LIKE=\"rhel centos fedora\"\n"); fam != "rhel" {
		t.Errorf("rocky family %q", fam)
	}
	if v := parseAsteriskVersion("Asterisk 20.11.1 built by root"); v != "20.11.1" {
		t.Errorf("version %q", v)
	}
	if v := parseAsteriskVersion("Asterisk GIT-22-abc123"); v == "" {
		t.Errorf("git version not parsed")
	}
	if m := parseMemMB("MemTotal:       16384000 kB\n"); m != 16000 {
		t.Errorf("mem %d", m)
	}
}
