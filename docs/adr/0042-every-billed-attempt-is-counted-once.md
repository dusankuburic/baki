# 42. Every billed attempt is counted once, and a coded error keeps its code

Date: 2026-10-26. Status: accepted, implemented (AI roadmap step 7). Narrows ADR 0037's usage records: token counts only, now complete.

## Context

Baki keeps token counts for its usage views (ADR 0037 removed budgets and prices). They under-counted in three ways: the retry wrapper kept only the last attempt's usage although the provider bills every attempt; the no-usage estimate left out the tool schemas sent on every tool-loop iteration and the tool calls a turn ends with, and estimated nothing for a side the provider reported as zero; and a usage row whose save failed was dropped without a second try or a count. On the client, a server error code that was not a provider error lost its code, so a context overflow offered only a Retry that fails the same way.

## Decision

1. **A discarded attempt is a row of its own.** The retry wrapper reports a Stream attempt it discards as an internal `attemptDiscarded` chunk once the next attempt starts (or on the error, when the retry never runs); the audited wrapper records it and resets the next attempt's counts, evidence and estimate. Chat adds the discarded attempts' usage to the response, or to the final error. Each attempt is counted once on every path.
2. **The estimate counts what was sent and what came back.** The request estimate includes the tool definitions with the context check's per-tool framing; the output estimate includes the tool calls; each side left at zero is estimated on its own.
3. **A failed save gets one more try.** After 2 s, without holding a record slot; `pad_usage_save_failed_total{outcome}` counts retried and lost rows.
4. **A coded failure keeps its code on the client.** `CONTEXT_LIMIT` offers Compact and a hint beside Retry; `TOOL_LOOP_LIMIT` a hint; the rest the plain Retry.

## Consequences

- Usage views show more input tokens for turns that were retried and for tool-heavy turns on providers that report no usage; that is the correction.
- A lost usage row is alertable instead of silent.
- The marker chunk is internal to the provider chain: the audited wrapper, the outermost, never forwards it.
