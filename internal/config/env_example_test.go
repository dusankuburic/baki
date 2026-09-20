package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envVarPattern finds every PAD_* environment variable named in loader.go,
// whether it comes from the envBindings table or from one of the hand-written
// appliers (storage backend, audit retention, list values).
var envVarPattern = regexp.MustCompile(`"(PAD_[A-Z0-9_]+)"`)

// TestEnvExample_DocumentsEveryRegisteredVariable keeps .env.example honest.
// The file is advertised in the README as the annotated env reference, and it
// had drifted to 43 of 78 variables — rate limits, retention, the CI webhook
// secret, tracing and every resilience knob were readable by the server and
// mentioned nowhere in it.
func TestEnvExample_DocumentsEveryRegisteredVariable(t *testing.T) {
	loader, err := os.ReadFile("loader.go")
	if err != nil {
		t.Fatalf("read loader.go: %v", err)
	}
	example, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}

	seen := map[string]bool{}
	var missing []string
	for _, m := range envVarPattern.FindAllStringSubmatch(string(loader), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if !strings.Contains(string(example), name) {
			missing = append(missing, name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("found no PAD_* variables in loader.go — did the pattern break?")
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("%d variable(s) the server reads are absent from .env.example: %s",
			len(missing), strings.Join(missing, ", "))
	}
}
