# Provider wire fixtures

`TestWireGolden` (`internal/ai/wire_golden_test.go`) runs every `<family>/<case>.sse` through that family's SSE
parser and compares what comes out with `<case>.golden`:

| family | parser | provider routes it covers |
| --- | --- | --- |
| `anthropic` | `parseClaudeSSE` | Claude direct, and Claude deployments on Azure AI Foundry |
| `openai` | `parseOpenAISSE` | chat completions: Foundry `/openai/v1`, GitHub Models, Copilot, xAI, GLM, local/vLLM/Ollama |
| `responses` | `parseResponsesSSE` | the Responses API on Azure AI Foundry (models and agents) |
| `gemini` | `parseGeminiSSE` | Gemini `streamGenerateContent?alt=sse` |

Every family must have `plain`, `tool-call`, `error`, `content-filter` and `truncated`, plus its thinking phase
(`thinking` for anthropic and gemini, `reasoning` for openai and responses). The test fails when one is missing or
when a golden has no fixture. Extra cases are fine.

## Provenance: these fixtures are hand-built, not captures

**None of the current fixtures was captured from a live stream.** There were no provider credentials on the
machine where they were written (2026-10-27). Each one was built by hand from the vendor's documented streaming
format (event names, field names, envelope shapes) and from the inline fixtures in the existing parser tests. Ids,
signatures and encrypted content are made up and contain `WIRE`. Each `.sse` starts with an SSE comment line
(`: …`) that says so. Every parser skips lines that are not `event:` or `data:`, as SSE readers must.

Two fixtures pin defects that writing them exposed (fixed, with unit tests in `wire_parser_defects_test.go`):

- `gemini/prompt-blocked`: a blocked prompt is a 200 whose only event has `promptFeedback.blockReason`. It used to
  end as a truncated stream.
- `openai/error-numeric-code`: vLLM's mid-stream error has an integer `code`. That event failed to decode, so the
  `[DONE]` after it closed the stream as a clean, empty answer.

Fixtures added with R-provider-32 and R-provider-33 (tests in `gemini_thought_signature_test.go` and
`wire_parser_gaps_test.go`):

- `gemini/tool-call` gives its first call a `functionCall.id` and a `thoughtSignature`. Both now appear in the
  golden: as that call's id, and in the turn's `items=` replay record.
- `gemini/missing-signature` is the documented `MISSING_THOUGHT_SIGNATURE` finish reason. The 400 shape of the
  same refusal is unit-tested only, because it is not a stream.
- `gemini/plain` and `gemini/thinking` carry `cachedContentTokenCount`, which is now `cacheRead`.
- `openai/no-space` is `openai/plain` with `data:` written without the optional space, and parses identically.
  `TestWireGolden_DataWithoutSpaceParsesTheSame` checks the same for every fixture.

Replacing a hand-built fixture with a real capture is the point of the recipe below. A capture that changes a
golden is a finding: the documentation and the wire disagree.

## Capture recipe

Use a throwaway key, with a prompt and flow that contain no customer data. Send the request the provider would
build. The bodies below are minimal; for the exact shape, log the body `buildBody` / `modelRequest` produces in a
test.

```sh
# anthropic (Messages API)
curl -sN https://api.anthropic.com/v1/messages \
  -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01" -H "content-type: application/json" \
  -d '{"model":"claude-sonnet-4-5","max_tokens":512,"stream":true,"messages":[{"role":"user","content":"Say hi"}]}' \
  > anthropic/plain.sse

# openai-compatible chat completions (Foundry /openai/v1 shown; usage needs stream_options)
curl -sN "$FOUNDRY_ENDPOINT/openai/v1/chat/completions" \
  -H "api-key: $FOUNDRY_KEY" -H "content-type: application/json" \
  -d '{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"Say hi"}]}' \
  > openai/plain.sse

# responses (Foundry)
curl -sN "$FOUNDRY_ENDPOINT/openai/v1/responses" \
  -H "api-key: $FOUNDRY_KEY" -H "content-type: application/json" \
  -d '{"model":"gpt-4.1","stream":true,"input":"Say hi"}' \
  > responses/plain.sse

# gemini (alt=sse is the stream flag; without it the body is one JSON array)
curl -sN "https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:streamGenerateContent?alt=sse" \
  -H "x-goog-api-key: $GEMINI_API_KEY" -H "content-type: application/json" \
  -d '{"contents":[{"role":"user","parts":[{"text":"Say hi"}]}]}' \
  > gemini/plain.sse
```

Getting each case:

- **tool-call**: offer one or two tools from `testdata/tools.golden` and ask for something only a tool can
  answer.
- **thinking / reasoning**: use a thinking model with thinking on (Claude `thinking`, Gemini `thinkingConfig`, an
  o-series or gpt-5 model with `reasoning`, DeepSeek-R1 for `reasoning_content`).
- **content-filter**: a prompt the provider's filter stops mid-answer. Azure content filters, Claude
  `stop_reason: "refusal"` and Gemini `finishReason: "SAFETY"` all do this; Gemini's `promptFeedback` blocks the
  prompt itself.
- **error**: mid-stream errors are rare on demand. Keep the hand-built fixture until a real one turns up in a log.
- **truncated**: cut a real capture after a text delta, before the terminal event.

Before you commit a capture, scrub it:

1. Remove anything that identifies the account: `id`/`responseId`/`request_id`/`system_fingerprint` values,
   organisation names in error messages, and endpoint hosts. Replace them with `WIRE…` placeholders of the same
   shape.
2. Replace signatures, `encrypted_content` and redacted-thinking `data` with fake values. Real ones are opaque
   blobs bound to the account.
3. Check for keys: `grep -iE 'sk-|api[-_]?key|bearer|AIza' <file>` must find nothing.
4. Keep the bytes otherwise as they came: event order, whitespace inside JSON, `event:` lines, pings, `[DONE]`. The
   parsers are tested against exactly those. Line endings may be LF (`.gitattributes` pins them). Replace the
   provenance comment on line 1 with `: captured <date> from <model>, scrubbed`.

Then refresh the goldens and review the diff:

```sh
go test ./internal/ai/ -run TestWireGolden -update-wire
git diff internal/ai/testdata/wire/
```

## What a golden records

There is one line per emitted chunk, numbered, with the non-zero fields in a fixed order:

- `done`, and `error=<class>`. The class is the sentinel the error wraps (`errorClass`, the same names as the
  `ai.error_class` span attribute), plus `truncated` for `io.ErrUnexpectedEOF` and a rate limit's `retryAfter`.
  Then `msg=`: the provider's own message, which is the fixture's `message` string that the error text contains
  (whitespace-normalised), or `msg=none`. The wrapping prose is not recorded, so rewording it does not churn the
  goldens, but a parser that loses the provider's message does.
- `text`, `reasoning`, `finish`, and the usage: `in`, `out`, `cacheRead`, `cacheWrite`.
- `tool{id name input}`, with the input compacted, or marked `invalid:` when it is not JSON. An id that appears in
  the fixture is recorded as is, whatever its shape. An id the parser invented is recorded as `<minted N>` in
  stream order, because `parseGeminiSSE` mints process-wide ids for calls the API sent without a
  `functionCall.id`.
- `toolEvent{…}` without its wall-clock duration; `notice{…}`, `serverStep`, `localKeepalive`, `silentStep=±1`.
- `items=`: the replay record (`ProviderItems`) on Done.
- `served=`: the served model.

A chunk that has nothing but a served model is written as `keepalive`; the idle watchdog depends on these. After
the chunks come the parser's `return`, then a summary line with the chunk and keepalive counts and the
concatenated text and reasoning.

The comparison normalises `\r\n` like `providers.golden`.
