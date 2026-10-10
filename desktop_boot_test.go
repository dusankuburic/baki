package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDesktopBoot_Smoke builds the backend and runs it as the Tauri shell does (no arguments, no PAD_* settings), with
// the per-user directories at a temporary one: the first stdout line is the CONFIG handshake, the session key is at
// its tokenPath, /healthz answers, and an interrupt stops it cleanly within fx's stop budget, with nothing else ever
// written to stdout. desktop.yml runs it on Windows; internal/app's TestApp_StopOrder shows the boot never reaches the
// OS keychain.
func TestDesktopBoot_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and boots the backend binary")
	}
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go tool on PATH to build the backend")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "baki-backend")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command(goTool, "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	cmd.Env = hermeticEnv(home)
	prepareInterrupt(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr := &syncBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	first, rest := make(chan string, 1), make(chan string, 1)
	go func() {
		r := bufio.NewReader(stdout)
		line, _ := r.ReadString('\n')
		first <- line
		b, _ := io.ReadAll(r)
		rest <- string(b)
	}()
	stopped := false
	defer func() {
		if !stopped {
			_ = cmd.Process.Kill()
			<-rest
			_ = cmd.Wait()
			t.Logf("backend stderr:\n%s", stderr.String())
		}
	}()

	// The shell gives the backend 30 s to hand over (BACKEND_STARTUP_TIMEOUT_SECS in src-tauri/src/lib.rs).
	var line string
	select {
	case line = <-first:
	case <-time.After(30 * time.Second):
		t.Fatal("no CONFIG line within the shell's 30 s startup timeout")
	}
	payload, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "CONFIG:")
	if !ok {
		t.Fatalf("first stdout line = %q, want the CONFIG handshake", line)
	}
	var handshake map[string]any
	if err := json.Unmarshal([]byte(payload), &handshake); err != nil {
		t.Fatalf("CONFIG payload %q: %v", payload, err)
	}
	port, _ := handshake["port"].(float64)
	tokenPath, _ := handshake["tokenPath"].(string)
	if len(handshake) != 2 || port <= 0 || tokenPath == "" {
		t.Fatalf("CONFIG payload %s, want exactly a port and a tokenPath", payload)
	}
	if !strings.HasPrefix(tokenPath, home) {
		t.Errorf("tokenPath %q is outside the redirected config directory %q", tokenPath, home)
	}
	if key, err := os.ReadFile(tokenPath); err != nil || len(key) == 0 {
		t.Errorf("session key at tokenPath: %d bytes, err %v", len(key), err)
	}

	// The listener was bound before the CONFIG line, so this waits in its backlog until the server serves.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(int(port)) + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	// fx installs its signal handler once every start hook has returned, a moment after the server serves.
	time.Sleep(500 * time.Millisecond)
	graceful := interrupt(cmd.Process)
	var out string
	select {
	case out = <-rest:
	case <-time.After(35 * time.Second): // fx's 25 s StopTimeout, and some
		t.Fatal("the backend did not exit within the stop budget")
	}
	stopped = true
	err = cmd.Wait()
	if graceful {
		if err != nil {
			t.Errorf("exit after the interrupt: %v, want a clean stop (exit 0)\nstderr:\n%s", err, stderr.String())
		}
	} else {
		t.Log("no console to deliver CTRL_BREAK to: killed instead, as the Tauri shell stops the backend on Windows")
	}
	if out != "" {
		t.Errorf("stdout after the CONFIG line = %q, want nothing (the logs go to a file and stderr)", out)
	}
}

// syncBuffer is a bytes buffer safe for the exec copier and the test to share.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// hermeticEnv is this process's environment for the backend, with the per-user directories (config, cache, home,
// temp) at home, so the boot never reads or writes the developer's %APPDATA%\pad-analyzer, and without the PAD_* and
// OTEL_* settings of the developer's shell (internal/app's stop-order test boots the same way).
func hermeticEnv(home string) []string {
	redirected := []string{"APPDATA", "LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "TMP", "TEMP", "TMPDIR"}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		k = strings.ToUpper(k)
		if strings.HasPrefix(k, "PAD_") || strings.HasPrefix(k, "OTEL_") || slices.Contains(redirected, k) {
			continue
		}
		env = append(env, kv)
	}
	for _, k := range redirected {
		env = append(env, k+"="+home)
	}
	return env
}
