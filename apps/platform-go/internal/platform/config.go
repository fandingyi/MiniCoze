package platform

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NestJS commonly uses 2h/7d. Bare numbers are deliberately rejected to avoid
// jsonwebtoken's string-milliseconds versus numeric-seconds ambiguity.
func ParseTokenTTL(raw string) (time.Duration, error) {
	if raw == "" {
		return 2 * time.Hour, nil
	}
	if strings.HasSuffix(raw, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
		if err != nil || days < 1 || days > 365 {
			return 0, errors.New("invalid JWT_EXPIRES_IN")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < time.Second || value > 365*24*time.Hour {
		return 0, errors.New("JWT_EXPIRES_IN must be a duration such as 2h or 7d")
	}
	return value, nil
}

func DatabaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", errors.New("valid DATABASE_URL required")
	}
	query := u.Query()
	query.Set("timezone", "UTC")
	if schema := query.Get("schema"); schema != "" {
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(schema) {
			return "", errors.New("invalid database schema")
		}
		query.Del("schema")
		query.Set("search_path", `"`+schema+`"`)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}
