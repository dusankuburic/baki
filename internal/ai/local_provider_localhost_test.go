package ai

import "testing"

// "localhost" is accepted and pinned to IPv4 loopback; a look-alike host is not.
func TestLocalConfig_LocalhostIsPinnedToLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:11434":     "http://127.0.0.1:11434",
		"http://LocalHost:1234/v1":   "http://127.0.0.1:1234",
		"https://localhost":          "https://127.0.0.1",
		"http://localhost.evil.com":  "http://localhost.evil.com",
		"http://localhostx:11434":    "http://localhostx:11434",
		"http://127.0.0.1:11434/v1/": "http://127.0.0.1:11434",
	} {
		c := LocalConfig{BaseURL: in}
		c.normalize()
		if c.BaseURL != want {
			t.Errorf("normalize(%q) = %q, want %q", in, c.BaseURL, want)
		}
	}
	bad := LocalConfig{BaseURL: "http://localhost.evil.com"}
	bad.normalize()
	if bad.Validate() == nil {
		t.Error("a look-alike host must still be refused")
	}
}
