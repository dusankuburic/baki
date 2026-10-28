# The PAD assistant's eval set (R-testing-12)

Each `<case>.json` here is one (flow, question) pair with what the answer must do; `transcripts/<case>.json` is the
provider's side of that turn, the `testutil.FakeProvider` script the offline runner replays.

**The transcripts are hand-authored.** No live capture has been made yet (the repository's CI has no provider
credentials). Each one is a plausible model trajectory written against the real tool output for its flow: tool calls
with valid inputs naming real block and finding ids, then a final answer. Their `recorded` field says
`hand-authored …`; a re-recorded transcript says `live <provider>/<model> <date>`.

## A case

```json
{
  "why": "what the case exercises (not graded)",
  "flow": "basic_flow",
  "selectedBlock": "B7",
  "question": "What does the selected block do?",
  "access": "fixes | read-only",
  "decision": "approve | decline",
  "expect": {
    "toolsSubset": ["get_variable_lineage"],
    "mustNotCall": ["apply_fix"],
    "mustNotOffer": ["apply_fix"],
    "mustMention": ["B7", "unhandled-error"],
    "mentionOrder": true,
    "mustLink": ["block:", "finding:"],
    "mustNotClaimChange": true
  }
}
```

- `flow` is a file of `core/parser/testdata/samples/` without `.txt`. Block and subflow ids are renamed to document
  order before the turn (`B1`… depth-first, `S1`…), as the request snapshots do, so a case can name them; finding ids
  are the analyzer's content fingerprints (`rule:hash`), stable across parses.
- `selectedBlock` (optional) is the block the question is asked on; `access` `read-only` makes the caller a viewer
  (the fix tools are not offered, the read-only system prompt is used); `decision` answers an approval card.
- `expect`: `toolsSubset` must all be in the tool trail; `mustNotCall` must not be; `mustNotOffer` must not be offered
  (graded when tools are native); `mustMention` items appear in the answer as whole tokens (in order with
  `mentionOrder`: the most important issue first); `mustLink` schemes are used at least once each;
  `mustNotClaimChange` forbids any claim of a change.

Every case is also held to the system prompt's promises, whatever its `expect`: the turn finishes; every tool call
succeeds; every `block:`/`finding:` link names an id the model was shown, exactly as shown (the flow context, the
question and the tool results — not the system prompt's examples, not its own earlier text; a `block:` id must be a
block of the flow, a `finding:` id the full `rule:hash` key, not the rule id or the hash alone); the only link form is
a bare `[text](block:<id>)` or `[text](finding:<key>)` — any other scheme, an autolink, a reference definition, a
spaced, titled or angle-bracketed target fails; and a change is claimed only after `apply_fix` reported `APPLIED` or
`apply_fixes` `Batch result: applied|partial`. A claim ("I've applied", "has been fixed", "Applied the fix",
"B4 now reads …") is excused only where it stands: a negator among the four words before its verb in the same clause
("nothing has been changed"), a modal right before it, or a sentence that opens with a condition ("Once applied, …",
"If you approve, …"). `TestChatEval_GraderCatchesEvasions` pins these rules.

## Running

- Offline (in `go test ./...`): `go test ./internal/service/ -run TestChatEval -count=1`. It replays each transcript
  through the real chat stream (`StreamChatMessage`: context build, scrubber, tool gate, native tool loop, the real
  tools over the real analysis, the approval applier with an apply hook that removes the finding's block) and grades
  the answer and the tool trail. The apply hook deletes the finding's block instead of running the real fixer, so an
  approved apply always reports `APPLIED`: the eval grades the loop, the applier's reporting and the answer, not the
  fixer (core/analyzer's autofix tests do). It also fails when the loop makes more or fewer provider calls than the transcript
  scripts. A failure means the loop or the prompt plumbing changed a graded outcome — or the case needs updating.
- Live (`-tags live`, gated on the same `BAKI_LIVE_<PROVIDER>_KEY` as the live smoke):
  `go test -tags live ./internal/service -run TestLiveEval -count=1 -v` prints a pass rate per model and, for each
  failing case, the failed checks as want/got. See `docs/architecture/ai-providers.md`.

## Re-recording a transcript

```
BAKI_LIVE_CLAUDE_KEY=… go test -tags live ./internal/service -run 'TestLiveEval/claude' -count=1 -v -record-evals
```

`-record-evals` rewrites `transcripts/<case>.json` for every case the live model **passed** (the live chain makes one
attempt per call, so no retried attempt is recorded), on a provider that takes
tools natively (a marker-mode transcript would not replay as native tool calls). Record one provider at a time, review
the diff, then run the offline replay: a recorded transcript must pass it unchanged. A case the model fails keeps its
current transcript — fix the case or the prompt, not the recording.

## Adding a case

Write `<case>.json`, then its transcript by hand (or record it). To find ids, run the offline replay with a draft
transcript and read the failures, or look at `internal/service/testdata/request_snapshots/` for what the context
shows. Keep at least 12 cases over at least 8 corpus flows (`TestChatEval_Offline` checks both).
