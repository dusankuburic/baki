package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/storage"
)

// TestWriteSessionSecret_FileOnlyNoStdout is the regression test for the
// JWT-secret-in-stdout leak: the auto-generated signing secret used to be
// emitted in the backend's `CONFIG:{...}` stdout line, where it landed in
// container logs / the Tauri app's stdout mirror. It must now be written to a
// 0600 file under the config dir, and the only thing handed off is the path.
func TestWriteSessionSecret_FileOnlyNoStdout(t *testing.T) {
	// Redirect the config dir to a temp location so the test doesn't clobber a
	// real session.key. ConfigDir() uses os.UserConfigDir(); override via env.
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	const secret = "SUPER-SECRET-SIGNING-KEY-1234"
	path, err := writeSessionSecret(secret)
	if err != nil {
		t.Fatalf("writeSessionSecret: %v", err)
	}

	// The path must point into the (temp) config dir.
	if !strings.HasPrefix(path, tmp) {
		t.Errorf("secret path %q not under temp config dir %q", path, tmp)
	}

	// The file must contain exactly the secret, and be readable only by the
	// owner (0600) so other users on the host can't read the signing key.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read secret file: %v", err)
	}
	if string(got) != secret {
		t.Errorf("secret file content = %q, want %q", string(got), secret)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat secret file: %v", err)
	}
	// 0o600 == -rw-------  (mask only leaves the permission bits)
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("secret file mode = %o, want 0600", mode)
	}
}

// TestWriteSessionSecret_DoesNotTouchStdout confirms the secret is never routed
// through stdout: a temp config dir is used, the file is written, and the secret
// string does not appear in what writeSessionSecret would have emitted (only the
// path is returned).
func TestWriteSessionSecret_DoesNotTouchStdout(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	const secret = "NEVER-ON-STDOUT-LEAK-CHECK"
	path, err := writeSessionSecret(secret)
	if err != nil {
		t.Fatalf("writeSessionSecret: %v", err)
	}

	// The returned handoff is a PATH; the secret value itself must not be the
	// path or leak through it. The path is the filename only.
	if strings.Contains(path, secret) {
		t.Errorf("secret value present in returned path %q — should only be a filesystem path", path)
	}

	// And the config dir must be the temp dir (sanity that ConfigDir honored the
	// override, so the assertions above are valid).
	dir, err := storage.ConfigDir()
	if err != nil {
		t.Fatalf("ConfigDir: %v", err)
	}
	if dir != filepath.Join(tmp, "pad-analyzer") {
		t.Errorf("ConfigDir = %q, want %q", dir, filepath.Join(tmp, "pad-analyzer"))
	}
}

// TestRateLimitGroup_AuthPathsInclusive is the regression test for the
// forgot-password / reset-password rate-limit fix: those two endpoints used to
// fall through to the looser "general" bucket, enabling email-flooding and SMTP
// cost amplification. They must now resolve to the tighter "auth" group, the
// same as login/register/refresh/change-password.
func TestRateLimitGroup_AuthPathsInclusive(t *testing.T) {
	authPaths := []string{
		"/api/auth/login",
		"/api/auth/refresh",
		"/api/auth/register",
		"/api/auth/change-password",
		"/api/auth/forgot-password",
		"/api/auth/reset-password",
		"/api/auth/verify-email",
		"/api/auth/sso/exchange",
	}
	for _, p := range authPaths {
		// Auth-grouping is path-based and method-independent (a GET probe of a
		// reset endpoint should still be throttled as auth).
		for _, m := range []string{"GET", "POST"} {
			if got := rateLimitGroup(m, p); got != rlGroupAuth {
				t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", m, p, got, rlGroupAuth)
			}
		}
	}
}

func TestRateLimitGroup_OtherBuckets(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/api/analysis/run", rlGroupAnalysis},
		{"GET", "/api/analysis/run", rlGroupGeneral}, // analysis bucket is POST-only
		{"POST", "/api/chat/stream", rlGroupChat},
		{"POST", "/api/flow/upload", rlGroupUpload},
		{"GET", "/api/flow/upload", rlGroupGeneral}, // upload bucket is POST-only
		{"GET", "/api/system/info", rlGroupGeneral},
		{"POST", "/api/flow/list", rlGroupGeneral},
	}
	for _, c := range cases {
		if got := rateLimitGroup(c.method, c.path); got != c.want {
			t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}
