package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"strings"

	"github.com/irnoti/asterisk-agent/internal/config"
	"github.com/irnoti/asterisk-agent/internal/installer"
	"github.com/irnoti/asterisk-agent/internal/setup"
	"github.com/irnoti/asterisk-agent/internal/tui"
)

// cmdSetup is the guided installer. Every question has a flag, so the same command also runs unattended with --yes.
func cmdSetup(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	var o setup.Options
	var ips string
	fs.StringVar(&o.Pre.Mode, "mode", "", "connect (use the Asterisk that is here) or full (install and configure everything)")
	fs.StringVar(&o.Pre.APIURL, "api", "", "base URL of the IRNoti API")
	fs.StringVar(&o.Pre.Token, "token", os.Getenv("IRNOTI_AGENT_TOKEN"), "agent token (or IRNOTI_AGENT_TOKEN)")
	fs.StringVar(&o.Pre.Method, "install-method", "", "apt (Debian/Ubuntu packages) or source (build Asterisk 22 LTS)")
	fs.StringVar(&o.Pre.Trunk.Auth, "trunk-auth", "", "register, ip or none")
	fs.StringVar(&o.Pre.Trunk.Name, "trunk-name", "", "trunk endpoint name (the panel channel is PJSIP/{number}@NAME)")
	fs.StringVar(&o.Pre.Trunk.Host, "trunk-host", "", "provider SIP server")
	fs.IntVar(&o.Pre.Trunk.Port, "trunk-port", 0, "provider SIP port (default 5060)")
	fs.StringVar(&o.Pre.Trunk.User, "trunk-user", "", "trunk user name")
	fs.StringVar(&o.Pre.Trunk.Pass, "trunk-pass", os.Getenv("IRNOTI_TRUNK_PASSWORD"), "trunk password (or IRNOTI_TRUNK_PASSWORD)")
	fs.StringVar(&ips, "trunk-ips", "", "provider IPs allowed to call you (comma separated)")
	fs.StringVar(&o.Pre.Trunk.Codecs, "codecs", "", "allowed codecs (default ulaw,alaw)")
	fs.StringVar(&o.Pre.PublicIP, "public-ip", "", "public IPv4 of this server")
	fs.IntVar(&o.Pre.RTPStart, "rtp-start", 0, "first RTP port (default 10000)")
	fs.IntVar(&o.Pre.RTPEnd, "rtp-end", 0, "last RTP port (default 10199)")
	fs.BoolVar(&o.Softphone, "softphone", false, "set up the browser phone (WebRTC)")
	fs.StringVar(&o.Pre.TLSMode, "tls", "", "browser phone TLS: proxy (CDN/reverse proxy) or cert (Asterisk itself)")
	fs.StringVar(&o.Pre.Domain, "domain", "", "browser phone domain")
	fs.StringVar(&o.Pre.Cert, "cert", "", "TLS certificate (with --tls cert)")
	fs.StringVar(&o.Pre.Key, "key", "", "TLS private key (with --tls cert)")
	fs.BoolVar(&o.NoFirewall, "no-firewall", false, "do not touch the firewall")
	fs.BoolVar(&o.NoService, "no-service", false, "do not install/start the agent's systemd service")
	fs.BoolVar(&o.DryRun, "dry-run", false, "show what would change without touching anything")
	fs.BoolVar(&o.Yes, "yes", false, "never ask; use flags and defaults (for automation)")
	fs.StringVar(&o.AsteriskDir, "asterisk-dir", config.DefaultAsteriskDir, "Asterisk configuration directory")
	fs.StringVar(&o.ConfigPath, "config", config.DefaultPath, "where to save the agent settings")
	_ = fs.Parse(args)

	for _, ip := range strings.FieldsFunc(ips, func(r rune) bool { return r == ',' || r == ' ' }) {
		o.Pre.Trunk.IPs = append(o.Pre.Trunk.IPs, ip)
	}
	o.Log = log
	o.NewInstaller = func(c config.Config, path string) *installer.Installer { return newInstaller(c, path, log) }

	// `curl | sudo bash` leaves stdin on the pipe; questions must come from the terminal
	in := os.Stdin
	if tty, err := os.Open("/dev/tty"); err == nil && !o.Yes {
		defer tty.Close()
		in = tty
	}
	return setup.Run(ctx, tui.New(in, os.Stdout), o)
}
