package database

import (
	"net/url"
	"strings"
)

// secureSSLModes is the set of libpq sslmode values that actually enforce an
// encrypted connection: "require" encrypts without certificate verification
// (MITM-able but not plaintext); "verify-ca"/"verify-full" also validate it.
var secureSSLModes = map[string]bool{
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// sslModeFromDSN extracts the sslmode from a libpq DSN, in both the URL form
// (postgres://...?sslmode=require) and the keyword/value form (sslmode=disable).
// Empty means the libpq default (prefer), which is NOT treated as secure.
func sslModeFromDSN(dsn string) string {
	// URL form first. url.Parse on a keyword/value DSN yields an empty Scheme
	// (no "postgres://"), so it falls through to the keyword scan below.
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if m := u.Query().Get("sslmode"); m != "" {
			return m
		}
	}
	// keyword=value form — whitespace-separated tokens.
	for _, tok := range strings.Fields(dsn) {
		if k, v, ok := strings.Cut(tok, "="); ok && k == "sslmode" {
			return v
		}
	}
	return ""
}

// sslModeIsSecure reports whether the DSN's sslmode enforces encryption.
func sslModeIsSecure(dsn string) bool {
	return secureSSLModes[sslModeFromDSN(dsn)]
}
