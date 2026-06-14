# pad-core

The reusable core library for PAD (Power Automate Desktop) flow analysis.

## Packages

| Package | Purpose |
|---------|---------|
| `models` | Domain types: FlowDocument, Block, Finding, AnalysisReport, etc. |
| `parser` | PAD export text parser (lexer + parser + tree builder) |
| `analyzer` | Static analysis rules engine, analysis cache, complexity metrics |
| `search` | In-memory block search index with fuzzy matching |
| `export` | PDF and Markdown report generators |
| `cache` | LRU cache (hashicorp/golang-lru) |
| `logger` | slog-based structured logger with panic recovery helpers |
| `ai/scrubber` | PII scrubber for AI prompt sanitization |

## Usage

```go
import (
    "pad-core/parser"
    "pad-core/analyzer"
    "pad-core/models"
)

doc, err := parser.ParseText(rawText, "MyFlow.txt", int64(len(rawText)))
if err != nil {
    return err
}

report := analyzer.RunAnalysis(doc, analyzer.AllRules(), nil, nil)
for _, f := range report.Findings {
    fmt.Printf("[%s] %s\n", f.Severity, f.Title)
}
```

## Module Boundaries

This module has **zero infrastructure dependencies** — no HTTP, no database, no cloud SDKs.
It depends only on the Go standard library plus three external packages:
- `github.com/google/uuid`
- `github.com/go-pdf/fpdf`
- `github.com/hashicorp/golang-lru/v2`

The server module (`pad-analyzer`) imports `pad-core` via a Go workspace (`go.work`)
and a `replace` directive for local development.
