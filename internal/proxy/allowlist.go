// Package proxy decides which ARI requests the API may forward through the tunnel.
package proxy

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type rule struct {
	method string
	path   *regexp.Regexp
	// query, when set, must accept the request's query too: a path alone is too wide for calls that can redirect media.
	query func(url.Values) bool
}

const id = `[A-Za-z0-9_.:@%+-]{1,200}`

func r(method, pattern string) rule {
	return rule{method: method, path: regexp.MustCompile("^" + pattern + "$")}
}

var externalHost = regexp.MustCompile(`^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*:([0-9]{4,5})$`)

// externalMediaOK lets the platform open a live-call media link and nothing else: RTP over UDP in G.711, towards a plain
// host:port (no scheme, no path, no credentials) on an unprivileged port. Every other externalMedia option is refused, so the
// call cannot be turned into an audio tap with an exotic encapsulation or a server-side connection.
func externalMediaOK(q url.Values) bool {
	allowed := map[string]bool{"app": true, "external_host": true, "format": true, "channelId": true, "encapsulation": true, "transport": true, "connection_type": true, "direction": true}
	for k, v := range q {
		if !allowed[k] || len(v) != 1 {
			return false
		}
	}
	if q.Get("app") == "" || !regexp.MustCompile("^"+id+"$").MatchString(q.Get("app")) {
		return false
	}
	if ch := q.Get("channelId"); ch != "" && !regexp.MustCompile("^"+id+"$").MatchString(ch) {
		return false
	}
	if f := q.Get("format"); f != "ulaw" && f != "alaw" {
		return false
	}
	if v := q.Get("encapsulation"); v != "" && v != "rtp" {
		return false
	}
	if v := q.Get("transport"); v != "" && v != "udp" {
		return false
	}
	if v := q.Get("connection_type"); v != "" && v != "client" {
		return false
	}
	if v := q.Get("direction"); v != "" && v != "both" {
		return false
	}
	h := q.Get("external_host")
	if !externalHost.MatchString(h) {
		return false
	}
	port, err := strconv.Atoi(h[strings.LastIndex(h, ":")+1:])
	return err == nil && port >= 1024 && port <= 65535
}

// The API never gets a general ARI door: only the calls the platform itself makes. Everything else (event subscriptions,
// applications, sounds, device states, asterisk modules/logging/config outside PJSIP …) is refused here, whatever the API asks.
var rules = []rule{
	r("GET", `/asterisk/info`),
	r("GET", `/channels`),
	r("POST", `/channels`),
	{method: "POST", path: regexp.MustCompile(`^/channels/externalMedia$`), query: externalMediaOK},
	r("GET", `/channels/`+id),
	r("DELETE", `/channels/`+id),
	r("POST", `/channels/`+id+`/(answer|play|record|snoop|continue|redirect|hold|ring|dtmf)`),
	r("DELETE", `/channels/`+id+`/hold`),
	r("GET", `/bridges`),
	r("POST", `/bridges`),
	r("DELETE", `/bridges/`+id),
	r("POST", `/bridges/`+id+`/(addChannel|removeChannel|record)`),
	r("DELETE", `/playbacks/`+id),
	r("POST", `/recordings/live/`+id+`/stop`),
	r("GET", `/recordings/stored/`+id+`/file`),
	r("DELETE", `/recordings/stored/`+id),
	r("PUT", `/asterisk/config/dynamic/res_pjsip/(auth|aor|endpoint)/`+id),
	r("DELETE", `/asterisk/config/dynamic/res_pjsip/(auth|aor|endpoint)/`+id),
}

// Allowed reports whether `method target` (target = path plus optional query, as the API sent it) may reach the local ARI.
func Allowed(method, target string) bool {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Fragment != "" {
		return false
	}
	// u.Path is already decoded: a path that still contains dot segments or backslashes could walk out of the allowed prefixes.
	p := u.Path
	if !strings.HasPrefix(p, "/") || strings.Contains(p, "//") || strings.Contains(p, `\`) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	// the raw form must not hide a different path behind percent-encoding of a slash or a dot
	if raw := strings.ToLower(u.EscapedPath()); strings.Contains(raw, "%2f") || strings.Contains(raw, "%2e") || strings.Contains(raw, "%5c") {
		return false
	}
	// ARI treats `api_key` as credentials; the agent owns those, the API must not smuggle its own.
	if _, ok := u.Query()["api_key"]; ok {
		return false
	}
	for _, rl := range rules {
		if rl.method == method && rl.path.MatchString(p) && (rl.query == nil || rl.query(u.Query())) {
			return true
		}
	}
	return false
}
