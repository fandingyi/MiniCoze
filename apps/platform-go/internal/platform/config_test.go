package platform

import (
	"net/url"
	"testing"
	"time"
)

func TestConfig(t *testing.T) {
	for _, raw := range []string{"", "2h", "7d"} {
		if ttl, err := ParseTokenTTL(raw); err != nil || ttl <= 0 {
			t.Fatal(raw, err)
		}
	}
	for _, raw := range []string{"0s", "-1h", "120", "1000d", "broken"} {
		if _, err := ParseTokenTTL(raw); err == nil {
			t.Fatal("accepted invalid TTL", raw)
		}
	}
	ttl, _ := ParseTokenTTL("7d")
	if ttl != 7*24*time.Hour {
		t.Fatal(ttl)
	}
	dsn, err := DatabaseURL("postgresql://user:password@localhost:5432/minicoze?schema=public&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := url.Parse(dsn)
	if parsed.Query().Get("schema") != "" || parsed.Query().Get("search_path") != `"public"` || parsed.Query().Get("sslmode") != "disable" {
		t.Fatal("Prisma DSN not adapted")
	}
}
