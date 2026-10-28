# AI roadmap: Organisation knowledge

Items of category `knowledge` from the [AI assistant roadmap](../ai-roadmap.md), ordered by tier. Evidence is `file:line` as of the roadmap's byline; re-read the cited lines before starting an item.

### R-knowledge-13

**Only .txt/.md plain text can be indexed; SOPs in DOCX/PDF/HTML have no ingestion path** · tier 2

**Problem.** The panel's file input accepts `.txt` and `.md`, and the hint says so. The upload API takes a JSON body with a `content` string, so the server never sees bytes it could extract text from. RPA teams keep organisation guidelines, coding standards and SOPs overwhelmingly as Word, PDF or wiki exports. Today an admin must convert them by hand.

**Evidence.**
- `frontend/src/components/settings/KnowledgeBasePanel.tsx:312` — `accept=".txt,.md"`
- `frontend/src/i18n/en.settings.ts:425` — "Currently supports .txt and .md files up to 1MB."
- `internal/api/org_knowledge.go:81` — the upload body is `{filename, content string}`, decoded as JSON (81-87)

**Change.** On the server, accept multipart uploads of `.docx` (unzip and read the `document.xml` text), `.pdf` (pure-Go text extraction) and `.html` (strip tags, keep headings), each with its own size cap. Normalise the result to Markdown-like text, keeping headings for chunker v2, then reuse `AddDocument`. Answer 415 for unsupported types. Keep the JSON path for the existing client. In the same change, fix the swagger annotation on the handler: it says `@Accept multipart/form-data` (`internal/api/org_knowledge.go:65`), but the handler only decodes JSON (81-87).

**Touches.** `internal/api/org_knowledge.go`, `internal/rag/extract/` (new), `frontend/src/components/settings/KnowledgeBasePanel.tsx`, `frontend/src/i18n/en.settings.ts`, `docs/swagger.yaml`

**Done when.**
- Uploading a fixture `.docx` yields a document whose chunks contain its paragraph text; a `.pdf` fixture does the same.
- An `.exe` upload is refused with 415 before any embedding call.
- Extraction runs under the existing 1 MiB text cap after conversion (413 otherwise).
