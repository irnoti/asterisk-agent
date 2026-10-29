package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/irnoti/asterisk-agent/internal/installer"
	"github.com/irnoti/asterisk-agent/internal/tui"
)

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// stock Debian/Ubuntu files: rtp.conf already sets the range and modules.conf has a [modules] section
func TestBuildExtraOnStockFiles(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"rtp.conf":     "[general]\nrtpstart=10000\nrtpend=20000\n\n[ice_host_candidates]\n",
		"modules.conf": "[modules]\nautoload=yes\nnoload => pbx_dundi.so\n\n[global]\n",
		"pjsip.conf":   "; empty\n",
	})
	a := Answers{RTPStart: 10000, RTPEnd: 10199, DisableChanSIP: true, Trunk: Trunk{Auth: AuthNone}}
	files, reload, err := BuildExtra(dir, a)
	if err != nil {
		t.Fatal(err)
	}
	if len(reload) != 2 {
		t.Errorf("reloads: %v", reload)
	}
	by := map[string]string{}
	for _, f := range files {
		by[filepath.Base(f.Path)] = f.Content
	}
	if r := by["rtp.conf"]; strings.Index(r, "rtpend=10199") > strings.Index(r, "rtpend=20000") {
		t.Errorf("our range must come before the packaged one (first match wins):\n%s", r)
	}
	if m := by["modules.conf"]; strings.Index(m, "noload => chan_sip.so") > strings.Index(m, "[global]") || !strings.Contains(m, "noload => chan_sip.so") {
		t.Errorf("chan_sip noload must sit inside [modules]:\n%s", m)
	}
	if !strings.Contains(by["pjsip.conf"], `#include "irnoti/pjsip_irnoti.conf"`) {
		t.Errorf("pjsip include missing:\n%s", by["pjsip.conf"])
	}
}

func TestBuildExtraUsesPJSIPCustomWhenPresent(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"pjsip_custom.conf": "", "pjsip.conf": "#include pjsip_custom.conf\n"})
	files, _, err := BuildExtra(dir, Answers{RTPStart: 10000, RTPEnd: 10199, Trunk: Trunk{Auth: AuthNone}})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range files {
		if filepath.Base(f.Path) == "pjsip_custom.conf" && strings.Contains(f.Content, installer.IncludeDir) {
			found = true
		}
	}
	if !found {
		t.Error("the include must go to pjsip_custom.conf on FreePBX-style layouts")
	}
}

func fakeEnv(found bool) Env {
	return Env{OSName: "Ubuntu 24.04", Family: "debian", Root: true, PublicIP: "203.0.113.7", LocalIP: "10.0.0.5", NAT: true, Systemd: true,
		Asterisk: AsteriskInfo{Found: found, Version: "20.6.0", Running: found}}
}

func TestCollectFullFromScratch(t *testing.T) {
	in := strings.Join([]string{
		"2",                    // mode: full
		"https://api.test.ir/", // api
		"irna_secret_token",    // token
		"1",                    // install method: apt
		"1",                    // trunk auth: register
		"sip.provider.ir",      // host
		"",                     // port default
		"user1",                // user
		"pa55",                 // password
		"198.51.100.5, 198.51.100.6",
		"",  // public ip default
		"",  // rtp start default
		"",  // rtp end default
		"y", // firewall
		"n", // browser phone
		"",
	}, "\n")
	ui := tui.NewPlain(strings.NewReader(in), &strings.Builder{})
	var hello string
	o := Options{Hello: func(_ context.Context, u, tok string) error { hello = u + " " + tok; return nil }}
	a, err := Collect(context.Background(), ui, o, fakeEnv(false))
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != ModeFull || a.APIURL != "https://api.test.ir" || a.Method != MethodAPT || !a.DisableChanSIP {
		t.Errorf("answers: %+v", a)
	}
	if hello != "https://api.test.ir irna_secret_token" {
		t.Errorf("token was not checked: %q", hello)
	}
	t1 := a.Trunk
	if t1.Auth != AuthRegister || t1.Host != "sip.provider.ir" || t1.Port != 5060 || t1.User != "user1" || t1.Pass != "pa55" || len(t1.IPs) != 2 || t1.Name != "trunk" {
		t.Errorf("trunk: %+v", t1)
	}
	if a.PublicIP != "203.0.113.7" || !a.NAT || a.RTPStart != 10000 || a.RTPEnd != 10199 || !a.Firewall || a.Softphone {
		t.Errorf("network: %+v", a)
	}
}

func TestCollectConnectNeedsAsterisk(t *testing.T) {
	ui := tui.NewPlain(strings.NewReader("1\n"), &strings.Builder{})
	_, err := Collect(context.Background(), ui, Options{}, fakeEnv(false))
	if err == nil || !strings.Contains(err.Error(), "no Asterisk found") {
		t.Fatalf("want a clear error, got %v", err)
	}
}

func TestCollectPanelForcesConnect(t *testing.T) {
	env := fakeEnv(true)
	env.Asterisk.Panel = "FreePBX"
	in := "2\nhttps://api.test.ir\nirna_secret_token\nn\n"
	ui := tui.NewPlain(strings.NewReader(in), &strings.Builder{})
	a, err := Collect(context.Background(), ui, Options{Hello: func(context.Context, string, string) error { return nil }}, env)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mode != ModeConnect {
		t.Errorf("a panel-managed PBX must only be connected, got %q", a.Mode)
	}
}

func TestCollectYesNeedsFlags(t *testing.T) {
	ui := tui.NewPlain(strings.NewReader(""), &strings.Builder{})
	_, err := Collect(context.Background(), ui, Options{Yes: true, Pre: Answers{Mode: ModeConnect}}, fakeEnv(true))
	if err == nil || !strings.Contains(err.Error(), "--api") {
		t.Fatalf("want a missing --api error, got %v", err)
	}
}

func TestCollectRejectsBadToken(t *testing.T) {
	ui := tui.NewPlain(strings.NewReader(""), &strings.Builder{})
	o := Options{Yes: true, Pre: Answers{Mode: ModeConnect, APIURL: "https://api.test.ir", Token: "irna_secret_token"},
		Hello: func(context.Context, string, string) error { return errors.New("401") }}
	if _, err := Collect(context.Background(), ui, o, fakeEnv(true)); err == nil {
		t.Fatal("a rejected token must stop the run")
	}
}

func TestChanSIPWithPeersMovesPJSIP(t *testing.T) {
	env := fakeEnv(true)
	env.Asterisk.ChanSIP, env.Asterisk.SIPPeers = true, 3
	o := Options{Yes: true, Pre: Answers{Mode: ModeFull, APIURL: "https://api.test.ir", Token: "irna_secret_token", Trunk: Trunk{Auth: AuthNone}},
		Hello: func(context.Context, string, string) error { return nil }}
	a, err := Collect(context.Background(), tui.NewPlain(strings.NewReader(""), &strings.Builder{}), o, env)
	if err != nil {
		t.Fatal(err)
	}
	if a.SIPPort != 5070 || a.DisableChanSIP {
		t.Errorf("existing chan_sip peers must be left alone: %+v", a)
	}
	if !strings.Contains(PJSIPConf(a), "bind=0.0.0.0:5070") {
		t.Error("transport must follow SIPPort")
	}
}
