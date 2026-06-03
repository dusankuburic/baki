package ai

import "os"

// providerURL returns the value of envKey if set in the environment,
// otherwise defaultURL. Provider base URL vars use this at package init
// time so operators can redirect to proxy endpoints or mock servers without
// recompiling:
//
//	CLAUDE_API_URL, OPENAI_API_URL, GEMINI_API_URL,
//	GLM_API_URL, XAI_API_URL, GITHUB_MODELS_API_URL, COPILOT_API_URL
func providerURL(envKey, defaultURL string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	return defaultURL
}
