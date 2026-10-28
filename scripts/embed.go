// Package scripts carries the helper scripts inside the binary so `irnoti-asterisk setup` works without downloading anything extra.
package scripts

import _ "embed"

//go:embed pjsip-webrtc-setup.sh
var PJSIPWebRTC string
