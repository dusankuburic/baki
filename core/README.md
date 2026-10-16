# pad-core

The reusable core library for PAD (Power Automate Desktop) flow analysis. Zero infrastructure dependencies (no HTTP, no database, no cloud SDKs): Go standard library plus `github.com/google/uuid`, `github.com/go-pdf/fpdf` and `github.com/hashicorp/golang-lru/v2` only. The server module (`pad-analyzer`) imports it via a Go workspace (`go.work`) and a `replace` directive.

| Package | Purpose |
|---|---|
| `models` | Domain types: FlowDocument, Block, Finding, AnalysisReport, etc. |
| `parser` | PAD export text parser (lexer + parser + tree builder) |
| `analyzer` | Static analysis rules engine, analysis cache, complexity metrics |
| `search` | In-memory block search index with fuzzy matching |
| `export` | PDF and Markdown report generators |
| `cache` | LRU cache (hashicorp/golang-lru) |
| `logger` | slog-based structured logger with panic recovery helpers |
| `ai/scrubber` | PII scrubber for AI prompt sanitization |
