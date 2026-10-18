package installer

import "testing"

const goodDialplan = `
[irnoti-outbound]
exten => _X.,1,NoOp(irnoti outbound to ${EXTEN})
 same => n,ExecIf($["${IRNOTI_MAXSEC}" != ""]?Set(TIMEOUT(absolute)=${IRNOTI_MAXSEC}))
 same => n,ExecIf($["${IRNOTI_RECORD}" = "1"]?Playback(this-call-may-be-monitored-or-recorded))
 same => n,Dial(PJSIP/${EXTEN}@trunk,60)
 same => n,Hangup()

[irnoti-inbound]
exten => _X.,1,Stasis(irnoti,inbound,${EXTEN})
 same => n(done),Hangup()
`

func TestValidateDialplan(t *testing.T) {
	if err := ValidateDialplan("x.conf", goodDialplan); err != nil {
		t.Fatalf("the platform's own dialplan must pass: %v", err)
	}
	bad := map[string]string{
		"system":      "exten => _X.,1,System(rm -rf /)",
		"trysystem":   "exten => s,1,TrySystem(id)",
		"same system": "exten => s,1,NoOp()\n same => n,System(id)",
		"nested":      `exten => s,1,ExecIf($[1=1]?System(id))`,
		"nested else": `exten => s,1,GotoIf($[1=1]?a:b)` + "\n same => n,ExecIf($[1=1]?Set(X=1):AGI(evil))",
		"shell fn":    `exten => s,1,Set(X=${SHELL(id)})`,
		"curl fn":     `exten => s,1,Set(X=${CURL(http://evil)})`,
		"include":     `#include "/etc/passwd"`,
		"exec":        `#exec "/bin/sh"`,
		"unreadable":  `exten => s`,
		"agi":         `exten => s,1,AGI(evil.agi)`,
	}
	for name, content := range bad {
		if ValidateDialplan("x.conf", content) == nil {
			t.Errorf("%s should be refused", name)
		}
	}
}
