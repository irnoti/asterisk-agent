package installer

import (
	"fmt"
	"regexp"
	"strings"
)

// The API renders the dialplan, so a compromised API could otherwise run commands on the PBX with `System()`. The agent therefore
// refuses any bundle that uses an application or function outside the small set the platform's own dialplan needs.
var allowedApps = map[string]bool{
	"NoOp": true, "Set": true, "ExecIf": true, "GotoIf": true, "Goto": true, "Playback": true, "MixMonitor": true, "Dial": true,
	"Hangup": true, "Stasis": true, "Answer": true, "Wait": true, "Ringing": true, "Progress": true, "Congestion": true, "Busy": true, "Return": true,
}

var (
	extenLine = regexp.MustCompile(`^\s*(exten|same)\s*=>\s*(.*)$`)
	appCall   = regexp.MustCompile(`^\s*([A-Za-z_]+)\(`)
	nestedApp = regexp.MustCompile(`[?:]\s*([A-Za-z_]+)\(`)
	badFunc   = regexp.MustCompile(`(?i)\b(SHELL|CURL|ODBC_[A-Z_]*|FILE|ENV|AGENT_[A-Z_]*|SYSTEM|TRYSYSTEM|EVAL|ARRAY|HASH|AST_CONFIG|DB|REALTIME[A-Z_]*)\s*\(`)
)

// ValidateDialplan checks one dialplan file from the API. Directives (#include, #exec …) are refused outright.
func ValidateDialplan(name, content string) error {
	for n, line := range strings.Split(content, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, ";") {
			continue
		}
		if strings.HasPrefix(trim, "#") {
			return fmt.Errorf("%s:%d: directives are not allowed", name, n+1)
		}
		if badFunc.MatchString(trim) {
			return fmt.Errorf("%s:%d: function not allowed", name, n+1)
		}
		m := extenLine.FindStringSubmatch(line)
		if m == nil {
			continue // a context header like [irnoti-inbound] or a plain setting
		}
		// exten => pattern,priority,App(args)   |   same => n,App(args)   (priority may carry a label: n(lbl))
		parts := strings.SplitN(m[2], ",", 3)
		var call string
		switch {
		case m[1] == "exten" && len(parts) == 3:
			call = parts[2]
		case m[1] == "same" && len(parts) >= 2:
			call = strings.TrimPrefix(m[2], parts[0]+",")
		default:
			return fmt.Errorf("%s:%d: cannot read the extension line", name, n+1)
		}
		am := appCall.FindStringSubmatch(call)
		if am == nil || !allowedApps[am[1]] {
			return fmt.Errorf("%s:%d: application not allowed", name, n+1)
		}
		for _, nm := range nestedApp.FindAllStringSubmatch(call, -1) {
			if !allowedApps[nm[1]] {
				return fmt.Errorf("%s:%d: application %s not allowed", name, n+1, nm[1])
			}
		}
	}
	return nil
}
