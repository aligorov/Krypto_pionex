package auth

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractClientIP(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		remote   string
		expected string
	}{
		{
			// CF-Connecting-IP is never trusted directly: it passes through
			// proxies unvalidated. X-Real-IP / rightmost XFF decide.
			name:     "CF header ignored, X-Real-IP wins",
			headers:  map[string]string{"CF-Connecting-IP": "203.0.113.195", "X-Real-IP": "198.51.100.42"},
			remote:   "10.0.0.1:12345",
			expected: "198.51.100.42",
		},
		{
			name:     "X-Real-IP behind trusted proxy",
			headers:  map[string]string{"X-Real-IP": "198.51.100.42", "X-Forwarded-For": "192.168.1.1"},
			remote:   "10.0.0.1:12345",
			expected: "198.51.100.42",
		},
		{
			// Rightmost XFF entry is the hop our proxy received from;
			// leftmost entries are attacker-controllable.
			name:     "X-Forwarded-For rightmost entry",
			headers:  map[string]string{"X-Forwarded-For": "192.0.2.1, 10.0.0.2, 127.0.0.1"},
			remote:   "10.0.0.1:12345",
			expected: "127.0.0.1",
		},
		{
			name:     "RemoteAddr fallback",
			headers:  map[string]string{},
			remote:   "198.51.100.99:54321",
			expected: "198.51.100.99",
		},
		{
			// A direct public client must not impersonate someone else via
			// spoofed headers: headers are ignored for untrusted peers.
			name:     "spoofed headers from public peer ignored",
			headers:  map[string]string{"X-Real-IP": "1.2.3.4", "X-Forwarded-For": "5.6.7.8", "CF-Connecting-IP": "9.9.9.9"},
			remote:   "203.0.113.7:4444",
			expected: "203.0.113.7",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			ip := ExtractClientIP(req)
			if ip == nil || ip.String() != tt.expected {
				t.Fatalf("expected IP %s, got %v", tt.expected, ip)
			}
		})
	}
}

func TestWhitelistMatching(t *testing.T) {
	fb := NewFail2Ban(nil)

	// Add manual whitelist subnets and exact IPs to test in-memory logic
	_, subnet1, _ := net.ParseCIDR("192.168.0.0/16")
	_, subnet2, _ := net.ParseCIDR("10.50.0.0/24")
	fb.whitelist = []*net.IPNet{subnet1, subnet2}
	fb.whiteExact = map[string]struct{}{
		"203.0.113.5": {},
	}

	// 1. Loopbacks are always whitelisted
	if !fb.IsWhitelisted(net.ParseIP("127.0.0.1")) {
		t.Errorf("expected 127.0.0.1 to be whitelisted")
	}
	if !fb.IsWhitelisted(net.ParseIP("::1")) {
		t.Errorf("expected ::1 to be whitelisted")
	}

	// 2. Exact IP
	if !fb.IsWhitelisted(net.ParseIP("203.0.113.5")) {
		t.Errorf("expected 203.0.113.5 to be whitelisted")
	}

	// 3. Subnet match
	if !fb.IsWhitelisted(net.ParseIP("192.168.1.100")) {
		t.Errorf("expected 192.168.1.100 to be whitelisted (in 192.168.0.0/16)")
	}
	if !fb.IsWhitelisted(net.ParseIP("10.50.0.45")) {
		t.Errorf("expected 10.50.0.45 to be whitelisted (in 10.50.0.0/24)")
	}

	// 4. Non-whitelisted IP
	if fb.IsWhitelisted(net.ParseIP("8.8.8.8")) {
		t.Errorf("expected 8.8.8.8 to NOT be whitelisted")
	}
}
