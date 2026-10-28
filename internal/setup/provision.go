package setup

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/irnoti/asterisk-agent/scripts"
)

const (
	MethodAPT    = "apt"
	MethodSource = "source"
	// asteriskSourceURL always serves the newest 22.x LTS release.
	asteriskSourceURL = "https://downloads.asterisk.org/pub/telephony/asterisk/asterisk-22-current.tar.gz"
)

// Stream runs a command and passes every output line to log as it appears. Tests replace it.
var Stream = func(ctx context.Context, dir string, log func(string), name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			log(sc.Text())
		}
		close(done)
	}()
	err := cmd.Wait()
	pw.Close()
	<-done
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func sh(ctx context.Context, log func(string), script string) error {
	return Stream(ctx, "", log, "bash", "-ec", script)
}

// DefaultMethod: Debian/Ubuntu ship Asterisk in their archives; everything else builds the 22 LTS from source.
func DefaultMethod(e Env) string {
	if e.Family == "debian" {
		return MethodAPT
	}
	return MethodSource
}

// InstallAsterisk installs Asterisk and leaves it running under systemd.
func InstallAsterisk(ctx context.Context, e Env, method string, log func(string)) error {
	switch method {
	case MethodAPT:
		if e.Family != "debian" {
			return errors.New("the apt method needs Debian or Ubuntu")
		}
		if err := Stream(ctx, "", log, "apt-get", "update", "-qq"); err != nil {
			return err
		}
		if err := Stream(ctx, "", log, "apt-get", "install", "-y", "-qq", "asterisk"); err != nil {
			return err
		}
		// prompts for the recording notice and voicemail; the package name differs between releases, so this is best effort
		for _, p := range []string{"asterisk-core-sounds-en", "asterisk-core-sounds-en-gsm"} {
			if Stream(ctx, "", log, "apt-get", "install", "-y", "-qq", p) == nil {
				break
			}
		}
	case MethodSource:
		if err := buildFromSource(ctx, e, log); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown install method %q", method)
	}
	if e.Systemd {
		if err := Stream(ctx, "", log, "systemctl", "enable", "--now", "asterisk"); err != nil {
			return err
		}
	}
	return waitAsterisk(ctx, log)
}

func waitAsterisk(ctx context.Context, log func(string)) error {
	var last error
	for i := 0; i < 30; i++ {
		if last = Stream(ctx, "", func(string) {}, "asterisk", "-rx", "core show uptime"); last == nil {
			log("Asterisk answers")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("Asterisk did not start: %w", last)
}

const buildScript = `
set -e
src=/usr/src/asterisk-irnoti
rm -rf "$src" && mkdir -p "$src" && cd "$src"
curl -fsSL %q -o asterisk.tar.gz
tar xzf asterisk.tar.gz --strip-components=1
yes | contrib/scripts/install_prereq install || contrib/scripts/install_prereq install
./configure --with-jansson-bundled
make menuselect.makeopts
menuselect/menuselect --enable CORE-SOUNDS-EN-WAV menuselect.makeopts
make -j%d
make install
make samples
getent group asterisk >/dev/null || groupadd --system asterisk
getent passwd asterisk >/dev/null || useradd --system -g asterisk -d /var/lib/asterisk -s /sbin/nologin asterisk
sed -i 's/^;\?runuser.*/runuser = asterisk/; s/^;\?rungroup.*/rungroup = asterisk/' /etc/asterisk/asterisk.conf
chown -R asterisk:asterisk /etc/asterisk /var/lib/asterisk /var/log/asterisk /var/spool/asterisk /var/run/asterisk /usr/lib/asterisk 2>/dev/null || true
cat > /etc/systemd/system/asterisk.service <<'UNIT'
[Unit]
Description=Asterisk PBX
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
User=asterisk
Group=asterisk
RuntimeDirectory=asterisk
ExecStart=/usr/sbin/asterisk -f -C /etc/asterisk/asterisk.conf
ExecReload=/usr/sbin/asterisk -rx 'core reload'
Restart=on-failure
LimitNOFILE=65536
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
`

func buildFromSource(ctx context.Context, e Env, log func(string)) error {
	if !e.Systemd {
		return errors.New("building from source needs systemd (the service unit is written for it)")
	}
	jobs := max(1, min(e.CPUs, e.MemMB/700)) // a gcc job can use ~700 MB; more jobs than that thrash a small VPS
	log(fmt.Sprintf("building Asterisk 22 LTS with %d job(s); this takes 10-30 minutes", jobs))
	return sh(ctx, log, fmt.Sprintf(buildScript, asteriskSourceURL, jobs))
}

// Firewall opens what the PBX needs on whichever host firewall is active and returns a note for each thing it did or could not do.
func Firewall(ctx context.Context, a Answers, log func(string)) ([]string, error) {
	var notes []string
	srcs := trunkSources(a)
	sip := SIPPortOf(a)
	rtp := fmt.Sprintf("%d:%d", a.RTPStart, a.RTPEnd)
	switch {
	case active(ctx, "ufw", "status"):
		if len(srcs) == 0 {
			if err := Stream(ctx, "", log, "ufw", "allow", fmt.Sprintf("%d/udp", sip)); err != nil {
				return notes, err
			}
			notes = append(notes, fmt.Sprintf("SIP %d/udp is open to everyone because the provider's IPs are unknown; restrict it once you know them", sip))
		}
		for _, s := range srcs {
			for _, p := range []string{"udp", "tcp"} {
				if err := Stream(ctx, "", log, "ufw", "allow", "from", s, "to", "any", "port", strconv.Itoa(sip), "proto", p); err != nil {
					return notes, err
				}
			}
		}
		if err := Stream(ctx, "", log, "ufw", "allow", rtp+"/udp"); err != nil {
			return notes, err
		}
		notes = append(notes, fmt.Sprintf("ufw: SIP from %s, RTP %s/udp", orAny(srcs), rtp))
	case active(ctx, "firewall-cmd", "--state"):
		for _, s := range srcs {
			for _, p := range []string{"udp", "tcp"} {
				rule := fmt.Sprintf("rule family=ipv4 source address=%s port port=%d protocol=%s accept", s, sip, p)
				if err := Stream(ctx, "", log, "firewall-cmd", "--permanent", "--add-rich-rule", rule); err != nil {
					return notes, err
				}
			}
		}
		if len(srcs) == 0 {
			_ = Stream(ctx, "", log, "firewall-cmd", "--permanent", fmt.Sprintf("--add-port=%d/udp", sip))
		}
		if err := Stream(ctx, "", log, "firewall-cmd", "--permanent", fmt.Sprintf("--add-port=%d-%d/udp", a.RTPStart, a.RTPEnd)); err != nil {
			return notes, err
		}
		if err := Stream(ctx, "", log, "firewall-cmd", "--reload"); err != nil {
			return notes, err
		}
		notes = append(notes, fmt.Sprintf("firewalld: SIP from %s, RTP %d-%d/udp", orAny(srcs), a.RTPStart, a.RTPEnd))
	default:
		notes = append(notes, fmt.Sprintf("no active ufw/firewalld found; if you use another firewall allow UDP+TCP %d from your provider and UDP %s", sip, rtp))
	}
	return notes, nil
}

func orAny(s []string) string {
	if len(s) == 0 {
		return "anywhere"
	}
	return strings.Join(s, ", ")
}

func active(ctx context.Context, name string, args ...string) bool {
	if _, err := exec.LookPath(name); err != nil {
		return false
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return false
	}
	s := strings.ToLower(string(out))
	return strings.Contains(s, "active") && !strings.Contains(s, "inactive") || strings.Contains(s, "running")
}

// trunkSources are the provider addresses that may talk to port 5060: the explicit list, else the trunk host's resolved addresses.
func trunkSources(a Answers) []string {
	t := a.Trunk
	if t.Auth == AuthNone || t.Host == "" {
		return nil
	}
	if len(t.IPs) > 0 {
		return t.IPs
	}
	if net.ParseIP(t.Host) != nil {
		return []string{t.Host}
	}
	addrs, err := net.LookupIP(t.Host)
	if err != nil {
		return nil
	}
	var out []string
	for _, ip := range addrs {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}

// BrowserPhone runs the embedded WebRTC helper (PJSIP over WSS, ICE, realtime tables) with the answers given in the wizard.
func BrowserPhone(ctx context.Context, a Answers, log func(string)) error {
	dir, err := os.MkdirTemp("", "irnoti-webrtc-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "pjsip-webrtc-setup.sh")
	if err := os.WriteFile(path, []byte(scripts.PJSIPWebRTC), 0o700); err != nil {
		return err
	}
	args := []string{path}
	if a.TLSMode == "cert" {
		args = append(args, "--cert", a.Cert, "--key", a.Key)
	} else {
		args = append(args, "--behind-proxy")
	}
	if a.NAT && a.PublicIP != "" {
		args = append(args, "--public-ip", a.PublicIP)
	}
	return Stream(ctx, "", log, "bash", args...)
}

// SelfInstall copies the running binary to dest so the systemd unit points at a stable path (the wizard is often run from /tmp).
func SelfInstall(dest string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if ex, _ := filepath.EvalSymlinks(exe); ex == dest {
		return nil
	}
	in, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dest + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
