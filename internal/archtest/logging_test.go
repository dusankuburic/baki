package archtest

import (
	"path"
	"strings"
	"testing"
)

// TestArch_BareChatLogCallsRatchet: a chat or AI-provider line logged through the package-level logger.Info/Warn/
// Error/Debug carries no request_id, user_id, trace_id or stream_id, so it cannot be joined to the request, the
// trace or the turn it belongs to (R-observability-2). Lines go through logger.FromContext(ctx); the calls left have
// no context to hand, are pinned per file and only go down.
func TestArch_BareChatLogCallsRatchet(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		dir, base := path.Split(f.path)
		chat := dir == "internal/service/" && strings.HasPrefix(base, "chat")
		if !chat && dir != "internal/ai/" {
			continue
		}
		names := importNames(f.ast, func(p string) (string, bool) { return "logger", p == "pad-core/logger" })
		n := countSelectors(f.ast, names, func(_, sel string) bool {
			switch sel {
			case "Info", "Warn", "Error", "Debug":
				return true
			}
			return false
		})
		if n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "bare logger calls in the chat and AI code (log through logger.FromContext(ctx))", got, bareChatLogCalls)
}

// bareChatLogCalls is the number of package-level logger calls per file, each without a context to hand.
var bareChatLogCalls = map[string]int{
	"internal/ai/azure_foundry_config.go": 1, // azureHTTPClient: the process-wide transport, built once
	"internal/ai/azure_registry.go":       1, // sortedAzureRegistry: the registry provider's constructor
	"internal/ai/local_provider.go":       1, // ParseLocalConfig: a config parser
	"internal/ai/responses.go":            2, // replayTurn: the pure request builder, Debug only
	"internal/ai/tokens.go":               1, // getTokenizer: the process-wide tokenizer's one-time init
	"internal/service/chat_context.go":    2, // buildScopedMessages: a pure message builder, Debug only
	"internal/service/chat_replicas.go":   1, // the replica's liveness fence: no request or stream
}
