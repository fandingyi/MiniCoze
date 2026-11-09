package platform

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestPublicPolicy(t *testing.T) {
	web, err := publicPolicy("WEB", []byte(`{"allowAnonymous":false,"allowedOrigins":["https://embed.example"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "http://service/api/public/agents/demo", nil)
	request.Header.Set("Origin", "https://embed.example")
	if web.allowsRequest("WEB", request) {
		t.Fatal("anonymous-disabled web policy allowed request")
	}
	web.AllowAnonymous = true
	if !web.allowsRequest("WEB", request) {
		t.Fatal("configured web origin rejected")
	}
	request.Header.Set("Origin", "https://other.example")
	if web.allowsRequest("WEB", request) {
		t.Fatal("unconfigured web origin allowed")
	}

	api, err := publicPolicy("API", []byte(`{"allowedOrigins":["https://console.example"],"allowedIps":["203.0.113.0/24","2001:db8::1"]}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", "https://console.example")
	request.RemoteAddr = "203.0.113.5:443"
	if !api.allowsRequest("API", request) {
		t.Fatal("configured IPv4 CIDR rejected")
	}
	request.RemoteAddr = "2001:db8::1:443"
	if api.allowsRequest("API", request) {
		t.Fatal("malformed remote address accepted")
	}
	request.RemoteAddr = "[2001:db8::1]:443"
	if !api.allowsRequest("API", request) {
		t.Fatal("configured IPv6 address rejected")
	}
	request.RemoteAddr = "198.51.100.2:443"
	if api.allowsRequest("API", request) {
		t.Fatal("unconfigured IP allowed")
	}
	request.RemoteAddr = "203.0.113.5:443"
	request.Header.Set("Origin", "https://other.example")
	if api.allowsRequest("API", request) {
		t.Fatal("unconfigured API origin allowed")
	}
	if api.RateLimitPerMinute != 60 || api.RateLimitPerDay != 1000 {
		t.Fatalf("API quota defaults: %+v", api)
	}
}

func TestPublicPolicyRejectsInvalidEntries(t *testing.T) {
	for _, config := range [][]byte{
		[]byte(`{"allowedOrigins":["ftp://example"]}`),
		[]byte(`{"allowedIps":["not-an-ip"]}`),
		[]byte(`{"rateLimitPerMinute":0}`),
		[]byte(`{"rateLimitPerDay":1000001}`),
		[]byte(`not-json`),
	} {
		if _, err := publicPolicy("API", config); err == nil {
			t.Fatal("invalid policy accepted", string(config))
		}
	}
}

func TestPublicQuotaBucketsUseRemainingUTCWindow(t *testing.T) {
	access := &PublicAccess{APIHash: "hash", RateLimitPerMinute: 2, RateLimitPerDay: 3}
	now := time.Date(2026, time.November, 4, 23, 59, 45, 0, time.UTC)
	buckets := publicQuotaBuckets(access, now)
	if len(buckets) != 2 || buckets[0].RetryAfter != 15*time.Second || buckets[1].RetryAfter != 15*time.Second {
		t.Fatalf("unexpected remaining windows: %+v", buckets)
	}
}
