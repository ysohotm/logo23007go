package clientip

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
		trusted bool
	}{
		{"untrusted ignores headers", "8.8.8.8:1234", map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Forwarded-For": "1.2.3.4"}, "8.8.8.8", false},
		{"cloudflare uses CF-Connecting-IP", "172.68.1.1:443", map[string]string{"CF-Connecting-IP": "1.2.3.4"}, "1.2.3.4", true},
		{"v99 proxy XFF right-to-left", "104.238.220.50:5000", map[string]string{"X-Forwarded-For": "9.9.9.9, 5.6.7.8, 172.68.1.1"}, "5.6.7.8", true},
		{"bwh8 tailscale X-Real-IP", "100.95.91.5:5000", map[string]string{"X-Real-IP": "5.6.7.8"}, "5.6.7.8", true},
		{"loopback no headers", "127.0.0.1:80", nil, "127.0.0.1", true},
		{"ipv4-mapped ipv6", "[::ffff:8.8.8.8]:80", nil, "8.8.8.8", false},
		{"dmit02 direct", "179.255.115.142:80", nil, "179.255.115.142", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = c.remote
			for k, v := range c.headers {
				r.Header.Set(k, v)
			}
			if got := ClientIP(r); got != c.want {
				t.Fatalf("ClientIP = %q, want %q", got, c.want)
			}
			if got := FromTrustedPeer(r); got != c.trusted {
				t.Fatalf("FromTrustedPeer = %v, want %v", got, c.trusted)
			}
		})
	}
}

func TestParsePrefix(t *testing.T) {
	for in, want := range map[string]string{
		"1.2.3.4":        "1.2.3.4/32",
		"10.1.2.3/8":     "10.0.0.0/8",
		"::1":            "::1/128",
		"::ffff:1.2.3.4": "1.2.3.4/32",
	} {
		p, err := ParsePrefix(in)
		if err != nil || p.String() != want {
			t.Fatalf("ParsePrefix(%q) = %v, %v; want %s", in, p, err, want)
		}
	}
	if _, err := ParsePrefix("nope"); err == nil {
		t.Fatal("expected error")
	}
}
