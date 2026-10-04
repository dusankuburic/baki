package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deployPassthroughFiles maps each deployment artefact that must forward the
// Azure Foundry / knowledge-base operator knobs to its path relative to this
// package (the repo layout env_example_test.go already relies on).
var deployPassthroughFiles = map[string]string{
	"docker-compose.yml":           filepath.Join("..", "..", "docker-compose.yml"),
	"docker-compose.prod.yml":      filepath.Join("..", "..", "docker-compose.prod.yml"),
	"infra/main.bicep":             filepath.Join("..", "..", "infra", "main.bicep"),
	".github/workflows/deploy.yml": filepath.Join("..", "..", ".github", "workflows", "deploy.yml"),
}

// deployPassthroughKnobs: the env var name each artefact must mention, and
// the Bicep parameter deploy.yml must wire for it (a param the template
// declares but the CI deploy never passes is still unreachable from CI).
var deployPassthroughKnobs = []struct {
	envVar     string
	bicepParam string
}{
	{"PAD_AZURE_FOUNDRY_ALLOWED_HOSTS", "azureFoundryAllowedHosts="},
	{"PAD_AI_ALLOW_PRIVATE_EGRESS", "aiAllowPrivateEgress="},
	{"PAD_EMBEDDING_PROVIDER", "embeddingProvider="},
	{"PAD_EMBEDDING_DIM", "embeddingDim="},
}

// TestDeployPassthrough_FoundryKnobsReachEveryDeploymentPath keeps the two
// compose files, the Bicep template (the README's production runbook) and
// the CI deploy in sync for the four Azure Foundry / knowledge-base
// operator knobs: without the wiring an ACA operator cannot set any of them
// without forking the template.
func TestDeployPassthrough_FoundryKnobsReachEveryDeploymentPath(t *testing.T) {
	for name, path := range deployPassthroughFiles {
		t.Run(name, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			text := string(b)
			for _, knob := range deployPassthroughKnobs {
				if !strings.Contains(text, knob.envVar) {
					t.Errorf("%s does not pass %s through", name, knob.envVar)
				}
			}
			// The workflow must also wire the Bicep parameter names, not
			// just name the variables it reads.
			if name == ".github/workflows/deploy.yml" {
				for _, knob := range deployPassthroughKnobs {
					if !strings.Contains(text, knob.bicepParam) {
						t.Errorf("%s never passes the Bicep parameter %s", name, knob.bicepParam)
					}
				}
			}
		})
	}
}
