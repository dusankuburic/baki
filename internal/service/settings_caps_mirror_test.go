package service

// R-config-2: the settings caps the server enforces are the ones the frontend's fields stop at. Each pair is a Go
// constant and the export of frontend/src/lib/constraints.ts that mirrors it.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestSettingsCaps_MirrorTheFrontendConstraints(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "frontend", "src", "lib", "constraints.ts"))
	if err != nil {
		t.Fatalf("read constraints.ts: %v", err)
	}
	exported := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^export const ([A-Z0-9_]+) = ([0-9_]+)\s*$`).FindAllStringSubmatch(strings.ReplaceAll(string(raw), "\r", ""), -1) {
		n, err := strconv.Atoi(strings.ReplaceAll(m[2], "_", ""))
		if err != nil {
			t.Fatalf("%s = %q: %v", m[1], m[2], err)
		}
		exported[m[1]] = n
	}
	for name, want := range map[string]int{
		"PROFILE_BIO_MAX_LENGTH":          profileBioMaxChars,
		"PROFILE_JOB_TITLE_MAX_LENGTH":    profileJobTitleMaxChars,
		"EMBEDDING_MODEL_MAX_LENGTH":      maxEmbeddingModelChars,
		"CONTEXT_TOKEN_BUDGET_MAX":        MaxContextTokenBudget,
		"SYSTEM_PROMPT_SUFFIX_MAX_LENGTH": MaxSystemPromptSuffixChars,
		"AI_PROMPTS_PER_LIST_MAX":         MaxPromptsPerList,
		"AI_PROMPT_MAX_LENGTH":            MaxPromptChars,
		"MAX_TOKENS_SETTING_MAX":          MaxOutputTokensSetting,
		"API_KEY_MAX_LENGTH":              MaxAPIKeyBytes,
	} {
		got, ok := exported[name]
		if !ok {
			t.Errorf("constraints.ts exports no %s (a plain integer literal)", name)
			continue
		}
		if got != want {
			t.Errorf("constraints.ts %s = %d, the server's cap is %d", name, got, want)
		}
	}
}
