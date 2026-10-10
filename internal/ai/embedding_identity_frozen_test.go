package ai

import "testing"

// TestEmbeddingIdentity_FormatFrozen freezes the embedding identity strings: knowledge_chunks.embedding_identity
// (Phase 4b, RAG) stores them, and a search matches chunks by equality — a provider that reports its identity in
// another form strands every corpus embedded under the old one. The OpenAI-compatible family reports
// "label|baseURL|model|dims" (dims as requested, 0 when none), Gemini "gemini|model". Changing either needs a
// stamped rename of the stored strings, never a silent edit here.
func TestEmbeddingIdentity_FormatFrozen(t *testing.T) {
	t.Run("openai-compatible family", func(t *testing.T) {
		cases := []struct {
			name string
			p    Provider
			want string
		}{
			{"openai", NewOpenAIProvider("k"), "openai|" + openAIBaseURL + "|text-embedding-3-small|0"},
			{"glm", NewGLMProvider("k"), "glm|" + glmBaseURL + "|" + NewGLMProvider("k").EmbeddingModel() + "|0"},
			// The label is the provider's wire label, not its id: "github models" is what every stored chunk carries.
			{"github-models", NewGitHubModelsProvider("k"), "github models|" + githubModelsBaseURL + "|text-embedding-3-small|0"},
		}
		for _, tc := range cases {
			if got := EmbeddingIdentity(tc.p); got != tc.want {
				t.Errorf("%s: identity = %q, want %q", tc.name, got, tc.want)
			}
		}
		// The requested width is part of the identity; the model override replaces the model segment.
		o := NewOpenAIProvider("k")
		o.setEmbeddingModel("text-embedding-3-large")
		o.setEmbeddingDimensions(1024)
		if got, want := EmbeddingIdentity(o), "openai|"+openAIBaseURL+"|text-embedding-3-large|1024"; got != want {
			t.Errorf("override: identity = %q, want %q", got, want)
		}
		// A width the model cannot take is not requested, so it is not part of the identity either.
		o.setEmbeddingModel("text-embedding-ada-002")
		if got, want := EmbeddingIdentity(o), "openai|"+openAIBaseURL+"|text-embedding-ada-002|0"; got != want {
			t.Errorf("fixed-width model: identity = %q, want %q", got, want)
		}
	})
	t.Run("gemini", func(t *testing.T) {
		g := NewGeminiProvider("k")
		if got, want := EmbeddingIdentity(g), "gemini|"+geminiEmbedModelDefault; got != want {
			t.Errorf("identity = %q, want %q", got, want)
		}
		g.setEmbeddingModel("text-embedding-004")
		if got, want := EmbeddingIdentity(g), "gemini|text-embedding-004"; got != want {
			t.Errorf("override: identity = %q, want %q", got, want)
		}
	})
	t.Run("chat-only providers report none", func(t *testing.T) {
		for _, p := range []Provider{NewClaudeProvider("k"), NewXAIProvider("k"), NewCopilotProvider("k")} {
			if got := EmbeddingIdentity(p); got != "" {
				t.Errorf("%s: identity = %q, want none", p.ID(), got)
			}
		}
	})
}
