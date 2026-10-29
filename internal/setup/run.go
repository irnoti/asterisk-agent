package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/irnoti/asterisk-agent/internal/api"
	"github.com/irnoti/asterisk-agent/internal/ari"
	"github.com/irnoti/asterisk-agent/internal/config"
	"github.com/irnoti/asterisk-agent/internal/installer"
	"github.com/irnoti/asterisk-agent/internal/tui"
	"github.com/irnoti/asterisk-agent/internal/version"
)

func yn(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func mask(s string) string {
	if len(s) <= 8 {
		return "••••••••"
	}
	return s[:5] + "••••••••"
}

func summary(ui *tui.UI, a Answers, env Env, o Options) {
	rows := [][2]string{{"Platform", a.APIURL}, {"Token", mask(a.Token)}}
	if a.Mode == ModeFull {
		if !env.Asterisk.Found {
			rows = append(rows, [2]string{"Asterisk", "install (" + a.Method + ")"})
		} else {
			rows = append(rows, [2]string{"Asterisk", "found " + env.Asterisk.Version})
		}
		switch a.Trunk.Auth {
		case AuthNone:
			rows = append(rows, [2]string{"Trunk", "add later"})
		default:
			rows = append(rows, [2]string{"Trunk", fmt.Sprintf("%s:%d (%s)", a.Trunk.Host, a.Trunk.Port, a.Trunk.Auth)})
		}
		rows = append(rows, [2]string{"SIP port", fmt.Sprintf("%d (PJSIP)", SIPPortOf(a))}, [2]string{"Public IP", a.PublicIP + map[bool]string{true: "  (behind NAT)", false: ""}[a.NAT]},
			[2]string{"RTP ports", fmt.Sprintf("%d-%d/udp", a.RTPStart, a.RTPEnd)}, [2]string{"Firewall", yn(a.Firewall)})
	} else {
		rows = append(rows, [2]string{"Asterisk", "keep as is (" + env.Asterisk.Version + ")"})
	}
	rows = append(rows, [2]string{"Browser phone", yn(a.Softphone)})
	if o.DryRun {
		rows = append(rows, [2]string{"Dry run", "nothing will be changed"})
	}
	ui.Section("Review")
	ui.Panel("Your choices", rows)
}

// Run is the whole wizard: look, ask, confirm, do, verify.
func Run(ctx context.Context, ui *tui.UI, o Options) error {
	ui.Banner("Asterisk setup for the IRNoti platform", version.Version)
	r := installer.ExecRunner{}

	var env Env
	if o.Env != nil {
		env = *o.Env
	} else {
		ui.Section("This server")
		if err := ui.Task(1, 1, "Looking at the server", func(log func(string)) error { env = Detect(ctx, r); log(env.OSName); return nil }); err != nil {
			return err
		}
	}
	if !env.Root && !o.DryRun {
		return errors.New("run as root (sudo): the wizard edits /etc/asterisk and installs packages")
	}
	ast := "not installed"
	if env.Asterisk.Found {
		ast = env.Asterisk.Version
		if env.Asterisk.Panel != "" {
			ast += " (" + env.Asterisk.Panel + ")"
		}
		if !env.Asterisk.Running {
			ast += " — not running"
		}
	}
	nat := "-"
	if env.PublicIP != "" {
		nat = env.PublicIP
		if env.NAT {
			nat += " (NAT)"
		}
	}
	ui.Panel("Detected", [][2]string{
		{"System", fmt.Sprintf("%s · %s · %d CPU · %d MB RAM", env.OSName, env.Arch, env.CPUs, env.MemMB)},
		{"Network", nat}, {"Asterisk", ast},
	})
	if env.MemMB > 0 && env.MemMB < 900 {
		ui.Warn("Under 1 GB of RAM: building from source will be slow or fail; prefer the package install.")
	}

	a, err := Collect(ctx, ui, o, env)
	if err != nil {
		if errors.Is(err, tui.ErrAborted) {
			ui.Info("Cancelled. Nothing was changed.")
			return nil
		}
		return err
	}
	summary(ui, a, env, o)
	if !o.Yes {
		ok, err := ui.Confirm("Start?", true)
		if err != nil || !ok {
			ui.Info("Cancelled. Nothing was changed.")
			return nil
		}
	}
	ui.Section("Installing")
	return execute(ctx, ui, o, env, a)
}

type step struct {
	title string
	skip  string // non-empty: not needed, shown greyed out with this reason
	run   func(log func(string)) error
}

func execute(ctx context.Context, ui *tui.UI, o Options, env Env, a Answers) error {
	dir := o.AsteriskDir
	if dir == "" {
		dir = config.DefaultAsteriskDir
	}
	c := config.Config{APIURL: a.APIURL, Token: a.Token, AsteriskDir: dir}
	if old, err := config.Load(o.ConfigPath); err == nil {
		c = old
		c.APIURL, c.Token, c.AsteriskDir = a.APIURL, a.Token, dir
	}
	c = c.WithDefaults()
	var in *installer.Installer
	var fwNotes, planned []string

	steps := []step{
		{title: "Install Asterisk", skip: skipIf(a.Mode != ModeFull || env.Asterisk.Found, "already installed", o.DryRun, "dry run"), run: func(log func(string)) error {
			return InstallAsterisk(ctx, env, a.Method, log)
		}},
		{title: "Configure Asterisk and connect to the platform", run: func(log func(string)) error {
			if !o.DryRun {
				if err := SelfInstall(binaryPath); err != nil {
					return fmt.Errorf("cannot install the agent binary: %w", err)
				}
			}
			log("registering with the platform")
			client := api.New(c.APIURL, c.Token)
			h, _ := os.Hostname()
			if _, err := client.Hello(ctx, api.Hello{Version: version.Version, Hostname: h, OS: "linux", Arch: Archname()}); err != nil {
				return fmt.Errorf("the API did not accept the token: %w", err)
			}
			bundle, err := client.Config(ctx)
			if err != nil {
				return fmt.Errorf("cannot fetch the configuration: %w", err)
			}
			in = o.NewInstaller(c, o.ConfigPath)
			in.Log = slog.New(slog.NewTextHandler(lineWriter(func(l string) {
				log(l)
				if m := wouldWrite.FindStringSubmatch(l); m != nil {
					planned = append(planned, m[1])
				}
			}), &slog.HandlerOptions{ReplaceAttr: func(_ []string, at slog.Attr) slog.Attr {
				if at.Key == slog.TimeKey || at.Key == slog.LevelKey {
					return slog.Attr{}
				}
				return at
			}}))
			in.DryRun, in.NoService = o.DryRun, o.NoService
			in.Binary = binaryPath
			if a.Mode == ModeFull {
				if in.Extra, in.ExtraReload, err = BuildExtra(dir, a); err != nil {
					return err
				}
			}
			if a.Mode == ModeFull {
				in.BeforeReload = func(ctx context.Context) error {
					if a.DisableChanSIP {
						// the reload would otherwise fail with "Address already in use"; an error here only means it was not loaded
						_, _ = in.Runner.Run(ctx, "asterisk", "-rx", "module unload chan_sip.so")
					}
					return nil
				}
				in.OnRollback = func(ctx context.Context) {
					if a.DisableChanSIP {
						_, _ = in.Runner.Run(ctx, "asterisk", "-rx", "module load chan_sip.so")
					}
				}
				in.ExtraVerify = func(ctx context.Context) error { return verifyPJSIP(ctx, in.Runner, a) }
			}
			log("writing the configuration (with backup)")
			return in.Install(ctx, bundle)
		}},
		{title: "Open firewall ports", skip: skipIf(a.Mode != ModeFull || !a.Firewall, "not requested", o.DryRun, "dry run"), run: func(log func(string)) error {
			notes, err := Firewall(ctx, a, log)
			for _, n := range notes {
				log(n)
			}
			fwNotes = notes
			return err
		}},
		{title: "Browser phone (WebRTC)", skip: skipIf(!a.Softphone, "not requested", o.DryRun, "dry run"), run: func(log func(string)) error {
			return BrowserPhone(ctx, a, log)
		}},
		{title: "Apply the RTP port range", skip: skipIf(a.Mode != ModeFull, "not changed", o.DryRun, "dry run"), run: func(log func(string)) error {
			return applyRTP(ctx, in.Runner, env.Systemd, a, log)
		}},
		{title: "Check that everything works", skip: skipIf(false, "", o.DryRun, "dry run"), run: func(log func(string)) error {
			cc := in.Cfg
			var ver string
			var err error
			for i := 0; i < 20; i++ { // ARI needs a moment after a restart
				if ver, err = ari.New(cc.ARIURL, cc.ARIUser, cc.ARIPassword).Info(ctx); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Second):
				}
			}
			if err != nil {
				return fmt.Errorf("ARI does not answer: %w", err)
			}
			log("ARI ok, Asterisk " + ver)
			if _, err := api.New(cc.APIURL, cc.Token).Heartbeat(ctx, api.Heartbeat{AriUp: true}); err != nil {
				return fmt.Errorf("the platform does not answer: %w", err)
			}
			log("platform ok")
			return nil
		}},
	}
	for i, s := range steps {
		if s.skip != "" {
			ui.Skipped(i+1, len(steps), s.title, s.skip)
			continue
		}
		if err := ui.Task(i+1, len(steps), s.title, s.run); err != nil {
			ui.Fail("%s failed: %v", s.title, err)
			ui.Dim("Configuration changes are rolled back automatically; backups are in %s.", installer.BackupRoot)
			return err
		}
	}
	finish(ui, a, o, fwNotes, planned)
	return nil
}

func skipIf(cond bool, why string, dry bool, dryWhy string) string {
	switch {
	case cond:
		return why
	case dry:
		return dryWhy
	}
	return ""
}

func finish(ui *tui.UI, a Answers, o Options, fwNotes, planned []string) {
	ui.Section("Done")
	if o.DryRun {
		ui.Info("Dry run finished. These files would be written (each is backed up first):")
		for _, p := range planned {
			ui.Dim("%s", p)
		}
		ui.Info("Run again without --dry-run to apply.")
		return
	}
	ui.OK("The agent is installed and connected.")
	ui.Panel("Next steps", nextSteps(a))
	for _, n := range fwNotes {
		ui.Dim("%s", n)
	}
	ui.Dim("Check any time:  irnoti-asterisk status      Logs:  journalctl -u irnoti-asterisk -f")
	ui.Dim("Undo the IRNoti changes:  irnoti-asterisk uninstall")
	fmt.Fprintln(ui.Out)
}

func nextSteps(a Answers) [][2]string {
	rows := [][2]string{{"1.", "In the panel the agent shows Online · ARI connected"}}
	if a.Mode == ModeFull && a.Trunk.Auth != AuthNone {
		rows = append(rows, [2]string{"2.", fmt.Sprintf("Set the trunk channel to  PJSIP/{number}@%s", a.Trunk.Name)},
			[2]string{"3.", "Bind a VoIP number to this server and set its route"})
	} else {
		rows = append(rows, [2]string{"2.", "Send the trunk's inbound calls to context  irnoti-inbound"},
			[2]string{"3.", "Bind a VoIP number to this server and set its route"})
	}
	if a.Softphone {
		rows = append(rows, [2]string{"4.", fmt.Sprintf("Panel server settings: sipDomain=%s  sipWsUrl=wss://%s/ws", a.Domain, a.Domain)})
		if a.NAT {
			rows = append(rows, [2]string{"5.", fmt.Sprintf("Forward UDP %d-%d from your router to this server", a.RTPStart, a.RTPEnd)})
		}
	}
	return rows
}

var wouldWrite = regexp.MustCompile(`msg="would write" file=(\S+)`)

// lineWriter turns the installer's log lines into step output instead of letting them tear through the spinner.
type lineWriter func(string)

func (w lineWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w(l)
	}
	return len(p), nil
}

// verifyPJSIP asks Asterisk whether our transports and trunk really exist: `module reload res_pjsip.so` succeeds even when an object was rejected.
func verifyPJSIP(ctx context.Context, r installer.Runner, a Answers) error {
	out, err := r.Run(ctx, "asterisk", "-rx", "pjsip show transports")
	if err != nil {
		return err
	}
	if !strings.Contains(out, "irnoti-udp") {
		return fmt.Errorf("PJSIP did not start its UDP transport on port %d (is another service using it?)", SIPPortOf(a))
	}
	if a.Trunk.Auth == AuthNone || a.Trunk.Host == "" {
		return nil
	}
	out, err = r.Run(ctx, "asterisk", "-rx", "pjsip show endpoints")
	if err != nil {
		return err
	}
	if !strings.Contains(out, a.Trunk.Name) {
		return fmt.Errorf("PJSIP did not load the trunk %q; see /var/log/asterisk/full", a.Trunk.Name)
	}
	return nil
}

var rtpRange = regexp.MustCompile(`Port start:\s+(\d+)[\s\S]*?Port end:\s+(\d+)`)

// applyRTP makes the running Asterisk use the range from rtp.conf: a reload does not move the RTP sockets, a restart does. It restarts only an
// idle PBX; with calls in progress it leaves the restart to the operator instead of cutting them.
func applyRTP(ctx context.Context, r installer.Runner, systemd bool, a Answers, log func(string)) error {
	out, err := r.Run(ctx, "asterisk", "-rx", "rtp show settings")
	if err != nil {
		return err
	}
	m := rtpRange.FindStringSubmatch(out)
	if m != nil && m[1] == strconv.Itoa(a.RTPStart) && m[2] == strconv.Itoa(a.RTPEnd) {
		log("already in effect")
		return nil
	}
	ch, _ := r.Run(ctx, "asterisk", "-rx", "core show channels count")
	if !strings.Contains(ch, "0 active channels") || !systemd {
		log("the new range takes effect at the next Asterisk restart (calls are active or there is no systemd)")
		return nil
	}
	log("restarting Asterisk (no calls in progress)")
	if _, err := r.Run(ctx, "systemctl", "restart", "asterisk"); err != nil {
		return err
	}
	return waitAsterisk(ctx, log)
}
