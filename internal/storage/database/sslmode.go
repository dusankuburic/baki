package database

import (
	"net/url"
	"strings"
)

// secureSSLModes is the set of libpq sslmode values that actually enforce an
// encrypted connection. "require" encrypts but does not verify the server
// certificate (MITM-able but not plaintext); "verify-ca"/"verify-full" also
// validate the certificate. Any of these is acceptable when RequireSSL is set.
var secureSSLModes = map[string]bool{
	"require":     true,
	"verify-ca":   true,
	"verify-full": true,
}

// sslModeFromDSN extracts the sslmode setting from a libpq/PostgreSQL DSN. It
// understands both the URL form (postgres://...?sslmode=require) and the
// keyword/value form (host=... sslmode=disable). An empty result means the DSN
// left sslmode at its libpq default (prefer), which is NOT treated as secure.
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
