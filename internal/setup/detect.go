// Package setup is the guided installer behind `irnoti-asterisk setup`: it looks at the server, asks what it needs, and then either connects an
// existing Asterisk or installs and configures one from nothing (trunk, RTP, firewall, browser phone), reusing the installer's backups and rollback.
package setup

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/irnoti/asterisk-agent/internal/installer"
)

type AsteriskInfo struct {
	Found   bool
	Binary  string
	Version string
	Running bool
	// ChanSIP is true when the old SIP driver is loaded (it holds UDP 5060); SIPPeers counts what is configured on it.
	ChanSIP  bool
	SIPPeers int
	// Panel is "FreePBX" or "Issabel" when a web panel manages the dialplan; the wizard then must not touch generated files.
	Panel string
}

type Env struct {
	OSID, OSName string
	Family       string // debian | rhel | ""
	Arch         string
	CPUs, MemMB  int
	Systemd      bool
	Root         bool
	PublicIP     string
	LocalIP      string
	// NAT is true when the public address is not on any local interface (cloud VPS, home router).
	NAT      bool
	Asterisk AsteriskInfo
}

func parseOSRelease(s string) (id, name, family string) {
	kv := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	id, name = kv["ID"], kv["PRETTY_NAME"]
	all := " " + id + " " + kv["ID_LIKE"] + " "
	switch {
	case strings.Contains(all, " debian ") || strings.Contains(all, " ubuntu "):
		family = "debian"
	case strings.Contains(all, " rhel ") || strings.Contains(all, " fedora ") || strings.Contains(all, " centos "):
		family = "rhel"
	}
	return
}

var asteriskVersion = regexp.MustCompile(`Asterisk\s+(?:GIT-)?(\d+(?:\.\d+)*\S*)`)

func parseAsteriskVersion(out string) string {
	if m := asteriskVersion.FindStringSubmatch(out); m != nil {
		return strings.SplitN(m[1], "~", 2)[0] // Debian appends "~dfsg+~cs6.13…"
	}
	return ""
}

func parseMemMB(meminfo string) int {
	for _, l := range strings.Split(meminfo, "\n") {
		if strings.HasPrefix(l, "MemTotal:") {
			f := strings.Fields(l)
			if len(f) >= 2 {
				kb, _ := strconv.Atoi(f[1])
				return kb / 1024
			}
		}
	}
	return 0
}

// PublicIPLookup is replaced in tests. It asks a public echo service; failure is not an error (the wizard then asks).
var PublicIPLookup = func(ctx context.Context) string {
	for _, u := range []string{"https://api.ipify.org", "https://ifconfig.me/ip"} {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		c := http.Client{Timeout: 5 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			continue
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if ip := net.ParseIP(strings.TrimSpace(string(b))); ip != nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

func localIP() string {
	// a UDP "connect" sends nothing; it only makes the kernel pick the source address of the default route
	c, err := net.Dial("udp4", "192.0.2.1:9")
	if err != nil {
		return ""
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String()
}

func hasLocalAddr(ip string) bool {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.String() == ip {
			return true
		}
	}
	return false
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Detect inspects the server. Nothing here changes it.
func Detect(ctx context.Context, r installer.Runner) Env {
	e := Env{Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), Root: os.Geteuid() == 0}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		e.OSID, e.OSName, e.Family = parseOSRelease(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		e.MemMB = parseMemMB(string(b))
	}
	e.Systemd = fileExists("/run/systemd/system")
	e.LocalIP = localIP()
	e.PublicIP = PublicIPLookup(ctx)
	if e.PublicIP != "" {
		e.NAT = !hasLocalAddr(e.PublicIP)
	}
	if bin, err := exec.LookPath("asterisk"); err == nil {
		e.Asterisk.Found, e.Asterisk.Binary = true, bin
		if out, err := r.Run(ctx, "asterisk", "-V"); err == nil {
			e.Asterisk.Version = parseAsteriskVersion(out)
		}
		_, err := r.Run(ctx, "asterisk", "-rx", "core show uptime")
		e.Asterisk.Running = err == nil
		if e.Asterisk.Running {
			if out, err := r.Run(ctx, "asterisk", "-rx", "module show like chan_sip.so"); err == nil {
				e.Asterisk.ChanSIP = strings.Contains(out, "chan_sip.so") && strings.Contains(out, "Running")
			}
			if e.Asterisk.ChanSIP {
				if out, err := r.Run(ctx, "asterisk", "-rx", "sip show peers"); err == nil {
					e.Asterisk.SIPPeers = parseSIPPeers(out)
				}
			}
		}
	}
	switch {
	case fileExists("/etc/freepbx.conf") || fileExists("/var/www/html/admin/modules/core"):
		e.Asterisk.Panel = "FreePBX"
	case fileExists("/etc/issabel.conf"):
		e.Asterisk.Panel = "Issabel"
	}
	return e
}

func archString() string { return runtime.GOARCH }

var sipPeers = regexp.MustCompile(`(?m)^(\d+) sip peers`)

func parseSIPPeers(out string) int {
	if m := sipPeers.FindStringSubmatch(out); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}
