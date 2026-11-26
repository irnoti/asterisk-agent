package proxy

import "testing"

func TestAllowed(t *testing.T) {
	allow := []struct{ m, p string }{
		{"GET", "/asterisk/info"},
		{"POST", "/channels?endpoint=PJSIP/1001&app=irnoti&appArgs=join,b1"},
		{"POST", "/channels/irnoti-auto-1/play?media=sound:hello-world"},
		{"DELETE", "/channels/chan1"},
		{"DELETE", "/channels/chan1/hold"},
		{"POST", "/bridges/b-1/addChannel?channel=a,b"},
		{"POST", "/bridges/b-1/record?name=call-1&format=wav"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=ulaw&channelId=irnoti-live-c1-ext&encapsulation=rtp&transport=udp&connection_type=client&direction=both"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=api.example.com:40002&format=alaw"},
		{"GET", "/recordings/stored/vm-1/file"},
		{"DELETE", "/recordings/stored/vm-1"},
		{"POST", "/recordings/live/vm-1/stop"},
		{"PUT", "/asterisk/config/dynamic/res_pjsip/endpoint/u7-ab"},
		{"DELETE", "/asterisk/config/dynamic/res_pjsip/aor/u7-ab"},
	}
	for _, c := range allow {
		if !Allowed(c.m, c.p) {
			t.Errorf("%s %s should be allowed", c.m, c.p)
		}
	}
	deny := []struct{ m, p string }{
		{"GET", "/events"},
		{"GET", "/applications"},
		{"POST", "/asterisk/modules/res_pjsip.so"},
		{"DELETE", "/asterisk/modules/res_pjsip.so"},
		{"PUT", "/asterisk/config/dynamic/res_pjsip/transport/x"},
		{"PUT", "/asterisk/config/dynamic/res_http/general/x"},
		{"POST", "/channels/c1/variable"},
		{"GET", "/sounds"},
		{"POST", "/channels"},                    // fine…
		{"GET", "/channels/../asterisk/logging"}, // …but dot segments never are
		{"GET", "/channels/%2e%2e/asterisk/logging"},
		{"GET", "/channels/a%2Fb"},
		{"GET", "//channels"},
		{"GET", "http://evil.example/channels"},
		{"GET", "/channels?api_key=x:y"},
		{"PATCH", "/channels/c1"},
		{"POST", "/channels/c1/play/extra"},
		{"GET", ""},
		{"POST", "/channels/externalMedia"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:80&format=ulaw"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=http://evil.example:40000&format=ulaw"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000/x&format=ulaw"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=slin16"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=ulaw&transport=tcp"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=ulaw&encapsulation=audiosocket"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=ulaw&connection_type=server"},
		{"POST", "/channels/externalMedia?app=irnoti&external_host=2.144.21.248:40000&format=ulaw&variables=x"},
	}
	for _, c := range deny {
		if c.p == "/channels" && c.m == "POST" {
			continue
		}
		if Allowed(c.m, c.p) {
			t.Errorf("%s %s should be refused", c.m, c.p)
		}
	}
}
