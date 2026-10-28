# 45. Drift detectors for the provider wire and the prompt: fuzzed parsers, wire goldens, an opt-in live smoke and an eval set

Date: 2026-10-27. Status: accepted, implemented (AI roadmap drift detectors, after step 10).

## Context

The chat's provider parsers are hand-written state machines over four wire formats and over model-authored text. Every test of them was an inline string written from documentation, with no golden of what they emit. Nothing confirmed that a vendor still sends what those strings say. Nothing judged a prompt or tool-description change beyond "the loop still runs".

## Decision

1. **Fuzz the parsers and the tools.** There are six targets in `internal/ai`, seeded from the existing inline fixtures, which are read from the test files, so the seeds follow the tests. Each asserts:
   - no panic, and a return within 5 s;
   - at most one terminal chunk;
   - valid UTF-8;
   - a valid JSON replay record;
   - bounded tool results.

   They run nightly in `bench-fuzz.yml`, and their crash corpus is uploaded.
2. **Pin the wire.** `internal/ai/testdata/wire/<family>/<case>.sse` fixtures are each paired with a golden of the parsed chunk sequence (`-update-wire`).
   - The golden records the error class and the provider's message, never the wrapping prose.
   - Only the ids the parser mints are masked.
   - A vendor change is caught by capturing a stream, refreshing and reviewing the diff.
3. **Smoke the vendors on demand.** The live tests are `//go:build live` and gated per provider on `BAKI_LIVE_<ID>_KEY`.
   - Each provider gets one chat, one streamed tool call and one embed, through the real chain, with usage recorded in memory only.
   - The cap is in tokens and calls (ADR 0037 removed prices): 64 output tokens per call, plus a reasoning allowance with low effort for models that think by default.
   - The workflow runs nightly and on dispatch, never on a pull request. It publishes a redacted summary and uploads no logs.
4. **Grade the assistant against its promises.** JSON eval cases over the sample flows are replayed deterministically through the real tool loop in `go test ./...`. The grade covers:
   - tools called, not called and not offered;
   - ids mentioned, and their order;
   - links grounded in what the model was shown;
   - no claimed change without a successful apply.

   The same cases run live, gated like the smoke, as a pass-rate measurement, and can re-record their transcripts.
5. **Hand-built until captured.** With no credentials where this was written, the wire fixtures and the eval transcripts are hand-built from the vendors' documented formats. Their READMEs say so and give the capture and re-record recipes.

## Consequences

- A parser change that alters what the chat receives changes a golden. A prompt or loop change that breaks a promise fails an eval case. Fuzzing has already found and fixed real defects: a double Done billed twice, blocked prompts retried as truncated streams, swallowed vLLM errors, and blank tool arguments that broke the next request.
- The goldens are only as current as their fixtures. Until the fixtures are re-captured from live streams, they prove the parsers handle the documented formats, not today's wire. The nightly live smoke is the first signal of drift.
- The live workflow needs the provider secrets configured in the repository to do anything. Without them it reports every provider skipped.
- The eval transcripts are one plausible trajectory each, so they test the plumbing and the grader, not a model's choices, until they are re-recorded live.
