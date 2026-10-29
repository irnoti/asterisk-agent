package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/irnoti/asterisk-agent/internal/api"
	"github.com/irnoti/asterisk-agent/internal/config"
	"github.com/irnoti/asterisk-agent/internal/installer"
	"github.com/irnoti/asterisk-agent/internal/tui"
	"github.com/irnoti/asterisk-agent/internal/version"
)

const (
	ModeConnect = "connect"
	ModeFull    = "full"
	binaryPath  = "/usr/local/bin/irnoti-asterisk"
)

// Options are what the command line already answered. Anything left empty is asked; with Yes it must have a default or the run stops.
type Options struct {
	Pre         Answers
	Yes         bool
	DryRun      bool
	NoService   bool
	NoFirewall  bool
	Softphone   bool // --softphone given
	ConfigPath  string
	AsteriskDir string
	Log         *slog.Logger
	// NewInstaller is supplied by the command so the wizard and `install` verify ARI the same way.
	NewInstaller func(c config.Config, path string) *installer.Installer
	// Hello checks the token with the platform; tests replace it.
	Hello func(ctx context.Context, apiURL, token string) error
	// Env replaces detection (tests).
	Env *Env
}

func defaultHello(ctx context.Context, apiURL, token string) error {
	h, _ := os.Hostname()
	_, err := api.New(apiURL, token).Hello(ctx, api.Hello{Version: version.Version, Hostname: h, OS: "linux", Arch: Archname()})
	return err
}

func Archname() string { return archString() }

type asker struct {
	ui  *tui.UI
	yes bool
}

func (k asker) input(flag, label, preset, def, hint string, validate func(string) error) (string, error) {
	if preset != "" {
		if validate != nil {
			if err := validate(preset); err != nil {
				return "", fmt.Errorf("%s: %w", flag, err)
			}
		}
		return preset, nil
	}
	if k.yes {
		if def == "" {
			return "", fmt.Errorf("%s is required (running with --yes)", flag)
		}
		return def, nil
	}
	return k.ui.Input(label, def, hint, validate)
}

func (k asker) confirm(label string, preset *bool, def bool) (bool, error) {
	if preset != nil {
		return *preset, nil
	}
	if k.yes {
		return def, nil
	}
	return k.ui.Confirm(label, def)
}

func (k asker) choose(flagVal, label string, opts []tui.Option, keys []string, def int) (string, error) {
	if flagVal != "" {
		for _, kk := range keys {
			if kk == flagVal {
				return kk, nil
			}
		}
		return "", fmt.Errorf("%q is not one of %s", flagVal, strings.Join(keys, ", "))
	}
	if k.yes {
		return keys[def], nil
	}
	i, err := k.ui.Select(label, opts, def)
	if err != nil {
		return "", err
	}
	return keys[i], nil
}

func portOf(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("enter a port from 1 to 65535")
	}
	return n, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

// Collect asks every question and returns the answers. It changes nothing on the server.
func Collect(ctx context.Context, ui *tui.UI, o Options, env Env) (Answers, error) {
	a, k := o.Pre, asker{ui, o.Yes}
	hello := o.Hello
	if hello == nil {
		hello = defaultHello
	}

	if a.Mode == "" {
		ui.Section("Mode")
	}
	panel := env.Asterisk.Panel
	defIdx := 1
	if env.Asterisk.Found {
		defIdx = 0
	}
	var err error
	a.Mode, err = k.choose(a.Mode, "What do you want to do?", []tui.Option{
		{Label: "Connect the Asterisk I already have", Hint: "writes only the IRNoti blocks, keeps your setup"},
		{Label: "Set everything up from scratch", Hint: "install Asterisk if missing + trunk, RTP, firewall"},
	}, []string{ModeConnect, ModeFull}, defIdx)
	if err != nil {
		return a, err
	}
	if a.Mode == ModeFull && panel != "" {
		ui.Warn("%s manages this PBX's SIP configuration; the wizard will only connect it (add the trunk in %s itself).", panel, panel)
		a.Mode = ModeConnect
	}
	if a.Mode == ModeConnect && !env.Asterisk.Found {
		return a, errors.New("no Asterisk found on this server; choose \"Set everything up from scratch\"")
	}

	ui.Section("IRNoti platform")
	for {
		a.APIURL, err = k.input("--api", "API address", a.APIURL, "", "From the panel: /admin/voip → Asterisk agents → New agent", func(s string) error {
			if strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://localhost") || strings.HasPrefix(s, "http://127.0.0.1") {
				return nil
			}
			return errors.New("must start with https://")
		})
		if err != nil {
			return a, err
		}
		a.APIURL = strings.TrimRight(a.APIURL, "/")
		if a.Token == "" && !o.Yes {
			a.Token, err = ui.Secret("Agent token", "Shown once when the agent was created in the panel", func(s string) error {
				if len(s) < 8 || strings.ContainsAny(s, " \t") {
					return errors.New("that does not look like a token")
				}
				return nil
			})
			if err != nil {
				return a, err
			}
		}
		if a.Token == "" {
			return a, errors.New("--token (or IRNOTI_AGENT_TOKEN) is required")
		}
		herr := ui.Task(1, 1, "Checking the token with the platform", func(log func(string)) error {
			log(a.APIURL)
			return hello(ctx, a.APIURL, a.Token)
		})
		if herr == nil {
			break
		}
		if o.Yes || !ui.Interactive() {
			return a, herr
		}
		ui.Warn("The platform did not accept that; check the address and token.")
		a.Token, o.Pre.APIURL, a.APIURL = "", "", ""
	}

	if a.Mode == ModeFull {
		if err := collectFull(ui, k, &a, env, o.NoFirewall); err != nil {
			return a, err
		}
	}

	ui.Section("Browser phone (optional)")
	ui.Dim("Lets customers call and answer from the panel. Needs a domain with TLS (a CDN/proxy or a certificate).")
	var pre *bool
	if o.Softphone || a.Softphone {
		t := true
		pre = &t
	}
	a.Softphone, err = k.confirm("Set up the browser phone now?", pre, false)
	if err != nil {
		return a, err
	}
	if a.Softphone {
		a.TLSMode, err = k.choose(a.TLSMode, "Where does TLS (wss://) end?", []tui.Option{
			{Label: "A CDN or reverse proxy in front", Hint: "no certificate on this server (recommended)"},
			{Label: "Asterisk itself", Hint: "you have a certificate and key for the domain"},
		}, []string{"proxy", "cert"}, 0)
		if err != nil {
			return a, err
		}
		a.Domain, err = k.input("--domain", "Domain the browser connects to", a.Domain, "", "e.g. pbx.example.com", ValidHost)
		if err != nil {
			return a, err
		}
		if a.TLSMode == "cert" {
			exists := func(s string) error {
				if _, err := os.Stat(s); err != nil {
					return fmt.Errorf("cannot read %s", s)
				}
				return nil
			}
			if a.Cert, err = k.input("--cert", "Certificate file (full chain, PEM)", a.Cert, "", "", exists); err != nil {
				return a, err
			}
			if a.Key, err = k.input("--key", "Private key file (PEM)", a.Key, "", "", exists); err != nil {
				return a, err
			}
		}
	}
	return a, nil
}

func collectFull(ui *tui.UI, k asker, a *Answers, env Env, noFirewall bool) error {
	var err error
	if !env.Asterisk.Found {
		ui.Section("Asterisk installation")
		def := DefaultMethod(env)
		keys := []string{MethodAPT, MethodSource}
		opts := []tui.Option{
			{Label: "From the distribution packages", Hint: "fast (a minute); version depends on your OS"},
			{Label: "Build Asterisk 22 LTS from source", Hint: "10-30 minutes; newest long-term release"},
		}
		if env.Family != "debian" {
			keys, opts = keys[1:], opts[1:]
			def = MethodSource
		}
		di := 0
		for i, kk := range keys {
			if kk == def {
				di = i
			}
		}
		if a.Method, err = k.choose(a.Method, "How should Asterisk be installed?", opts, keys, di); err != nil {
			return err
		}
	}

	ui.Section("SIP trunk (your telephone provider)")
	ui.Dim("The line that carries calls to and from the phone network.")
	a.Trunk.Auth, err = k.choose(a.Trunk.Auth, "How does the provider identify you?", []tui.Option{
		{Label: "User name and password", Hint: "we register with the provider"},
		{Label: "Fixed IP addresses", Hint: "no credentials; the provider knows this server's IP"},
		{Label: "Skip for now", Hint: "add the trunk later"},
	}, []string{AuthRegister, AuthIP, AuthNone}, 0)
	if err != nil {
		return err
	}
	if a.Trunk.Auth != AuthNone {
		t := &a.Trunk
		if t.Name == "" {
			t.Name = "trunk"
		}
		if t.Host, err = k.input("--trunk-host", "Provider SIP server", t.Host, "", "host name or IP, e.g. sip.provider.ir", ValidHost); err != nil {
			return err
		}
		ps, err := k.input("--trunk-port", "Provider SIP port", portStr(t.Port), "5060", "", func(s string) error { _, e := portOf(s); return e })
		if err != nil {
			return err
		}
		t.Port, _ = portOf(ps)
		if t.Auth == AuthRegister {
			if t.User, err = k.input("--trunk-user", "Trunk user name", t.User, "", "", func(s string) error {
				if strings.ContainsAny(s, "; \r\n") {
					return errors.New("no spaces or ';'")
				}
				return nil
			}); err != nil {
				return err
			}
			if t.Pass == "" && !k.yes {
				if t.Pass, err = k.ui.Secret("Trunk password", "", ValidSecret); err != nil {
					return err
				}
			}
			if err := ValidSecret(t.Pass); err != nil {
				return fmt.Errorf("--trunk-pass: %w", err)
			}
		}
		ipPreset := strings.Join(t.IPs, ",")
		ips := ""
		if ips, err = k.optional("--trunk-ips", "Provider IP addresses allowed to call you", ipPreset, "comma separated; Enter = only the server above"); err != nil {
			return err
		}
		t.IPs = splitList(ips)
		if err := t.Validate(); err != nil {
			return err
		}
	}

	ui.Section("Network")
	switch {
	case !env.Asterisk.Found:
		a.DisableChanSIP = true // the packages still ship chan_sip; PJSIP needs port 5060 for itself
	case env.Asterisk.ChanSIP && env.Asterisk.SIPPeers == 0:
		a.DisableChanSIP, err = k.confirm("The old chan_sip driver is loaded and holds port 5060 (no peers configured). Unload it so PJSIP can use 5060?", nil, true)
		if err != nil {
			return err
		}
		if !a.DisableChanSIP {
			a.SIPPort = 5070
		}
	case env.Asterisk.ChanSIP:
		a.SIPPort = 5070
		ui.Warn("chan_sip has %d peer(s) configured and holds port 5060, so PJSIP will listen on %d instead. Ask your provider to use that port, or migrate the peers to PJSIP later.", env.Asterisk.SIPPeers, a.SIPPort)
	}
	defIP := env.PublicIP
	if defIP == "" {
		defIP = env.LocalIP // the lookup service may be unreachable (filtered networks); a LAN address is still a usable default
	}
	if a.PublicIP, err = k.input("--public-ip", "This server's public IP", a.PublicIP, defIP, "calls from the internet reach it here", func(s string) error {
		if net.ParseIP(s) == nil {
			return errors.New("not an IP address")
		}
		return nil
	}); err != nil {
		return err
	}
	a.NAT = (a.PublicIP == env.PublicIP && env.NAT) || (a.PublicIP != env.PublicIP && !hasLocalAddr(a.PublicIP))
	if a.NAT {
		ui.Info("This server is behind NAT: the public address is announced in SIP and RTP, and your router/cloud firewall must forward the ports below.")
	}
	rs, err := k.input("--rtp-start", "First RTP port", portStr(a.RTPStart), "10000", "each call uses two ports", func(s string) error { _, e := portOf(s); return e })
	if err != nil {
		return err
	}
	re, err := k.input("--rtp-end", "Last RTP port", portStr(a.RTPEnd), "10199", "200 ports ≈ 100 simultaneous calls", func(s string) error { _, e := portOf(s); return e })
	if err != nil {
		return err
	}
	a.RTPStart, _ = portOf(rs)
	a.RTPEnd, _ = portOf(re)
	if err := ValidRTP(a.RTPStart, a.RTPEnd); err != nil {
		return err
	}
	var pre *bool
	if noFirewall {
		f := false
		pre = &f
	}
	a.Firewall, err = k.confirm("Open the SIP and RTP ports in this server's firewall?", pre, true)
	return err
}

func (k asker) optional(flag, label, preset, hint string) (string, error) {
	if preset != "" || k.yes {
		return preset, nil
	}
	return k.ui.Input(label, "", hint, nil)
}

func portStr(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// BuildExtra prepares the PJSIP and RTP files for the installer's transaction.
func BuildExtra(asteriskDir string, a Answers) ([]installer.FileWrite, []string, error) {
	read := func(p string) (string, error) {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return string(b), err
	}
	target := filepath.Join(asteriskDir, "pjsip.conf")
	if _, err := os.Stat(filepath.Join(asteriskDir, "pjsip_custom.conf")); err == nil {
		target = filepath.Join(asteriskDir, "pjsip_custom.conf")
	}
	pj, err := read(target)
	if err != nil {
		return nil, nil, err
	}
	rtp, err := read(filepath.Join(asteriskDir, "rtp.conf"))
	if err != nil {
		return nil, nil, err
	}
	var mods []installer.FileWrite
	if a.DisableChanSIP {
		cur, err := read(filepath.Join(asteriskDir, "modules.conf"))
		if err != nil {
			return nil, nil, err
		}
		mods = append(mods, installer.FileWrite{Path: filepath.Join(asteriskDir, "modules.conf"), Content: InsertInSection(cur, "modules", "modules", "noload => chan_sip.so"), Mode: 0o644})
	}
	files := []installer.FileWrite{
		{Path: filepath.Join(asteriskDir, installer.IncludeDir, "pjsip_irnoti.conf"), Content: PJSIPConf(a), Mode: 0o640},
		{Path: target, Content: installer.UpsertBlock(pj, installer.TagPJSIP, `#include "`+installer.IncludeDir+`/pjsip_irnoti.conf"`), Mode: 0o644},
		{Path: filepath.Join(asteriskDir, "rtp.conf"), Content: InsertInGeneral(rtp, installer.TagRTP, RTPOptions(a)), Mode: 0o644},
	}
	files = append(files, mods...)
	return files, []string{"module reload res_pjsip.so", "module reload res_rtp_asterisk.so"}, nil
}
