package ai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// pendingToolCall accumulates a streamed tool call: the id/name arrive on the
// block-start event, the JSON arguments arrive as a series of fragments.
type pendingToolCall struct {
	id   string
	name string
	args strings.Builder
}

// assembleToolCalls flattens an index-keyed map of partial tool calls into the
// neutral []ToolCall, ordered by block index for determinism. Empty argument
// buffers become "{}" so providers always receive valid JSON input.
func assembleToolCalls(byIndex map[int]*pendingToolCall) []ToolCall {
	if len(byIndex) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(byIndex))
	for i := range byIndex {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]ToolCall, 0, len(idxs))
	for _, i := range idxs {
		p := byIndex[i]
		input := p.args.String()
		if input == "" {
			input = "{}"
		}
		out = append(out, ToolCall{ID: p.id, Name: p.name, Input: json.RawMessage(input)})
	}
	return out
}

// errStreamTruncated wraps io.ErrUnexpectedEOF for SSE bodies that ended
// cleanly without sending a provider-specific terminal marker (Claude's
// `message_stop` / `[DONE]`, OpenAI's `[DONE]` / `finish_reason`, Gemini's
// non-empty `finishReason`). Previously each parser synthesized a `Done`
// chunk in this case, so a network-truncated response was indistinguishable
// from a complete one. Returning an error here lets the chat service emit
// a stream-error event so the client can show "incomplete response."
func errStreamTruncated(provider string) error {
	return fmt.Errorf("%s stream truncated before terminal marker: %w", provider, io.ErrUnexpectedEOF)
}

// errStreamMalformed reports a stream that ended without a terminal marker AND
// had one or more undecodable `data:` events. Previously each parser silently
// `continue`d past JSON-unmarshal failures, so a fully malformed stream looked
// like a clean empty response; surfacing it lets the chat service show an error.
func errStreamMalformed(provider string, n int) error {
	return fmt.Errorf("%s stream had %d undecodable event(s) and no terminal marker", provider, n)
}

// classifyClaudeStreamError maps a mid-stream `error` event onto the shared
// sentinel errors, mirroring how ClaudeProvider maps HTTP statuses on the
// initial response (429 → ErrRateLimited, 5xx/overloaded → ErrProviderDown).
// Without this the retry and circuit-breaker wrappers see only an opaque
// error and treat an overloaded provider as a permanent, non-tripping failure.
// Unknown types stay plain errors (permanent — surfaced, never retried).
func classifyClaudeStreamError(errType, msg string) error {
	switch errType {
	case "rate_limit_error":
		return fmt.Errorf("%w: claude stream error: %s", ErrRateLimited, msg)
	case "overloaded_error", "api_error":
		return fmt.Errorf("%w: claude stream error: %s", ErrProviderDown, msg)
	}
	return fmt.Errorf("claude stream error: %s", msg)
}

// Inline reasoning markers. DeepSeek-R1 (and other open reasoning models served
// through OpenAI-compatible chat completions — Azure AI Foundry's /openai/v1,
// vLLM) put their chain of thought in the answer's own content, wrapped in a
// leading <think>…</think> block, followed by the final answer.
const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
	// thinkSpace is the whitespace allowed before the opening tag and dropped
	// between the closing tag and the answer.
	thinkSpace = " \t\r\n"
	// thinkHoldMax caps how much whitespace thinkStart holds. While only
	// whitespace has arrived, every delta "could" still precede "<think>"
	// (strings.HasPrefix(thinkOpenTag, "") is always true), so the pending
	// buffer grew without bound on a whitespace-only stream. Past the cap the
	// whitespace is released as answer text (exactly what the non-streaming
	// split does for content that never opens a tag) and the splitter passes
	// through; real opening tags do not arrive after 1 KiB of silence.
	thinkHoldMax = 1 << 10
)

type thinkState int

const (
	// thinkStart: no non-whitespace content yet — the content may still open
	// a think block, so leading whitespace and a prefix of "<think>" are held.
	thinkStart thinkState = iota
	// thinkInside: inside the leading block — content is reasoning.
	thinkInside
	// thinkAfterClose: the block closed — whitespace before the answer is dropped.
	thinkAfterClose
	// thinkPassthrough: everything is answer text.
	thinkPassthrough
)

// thinkSplitter separates a LEADING <think>…</think> block from the answer in
// streamed content, so the reasoning never becomes answer text: it is not
// shown, not stored as the assistant message and not re-sent as history.
// Microsoft's DeepSeek-R1 guidance for multi-turn chat is the same — keep only
// the final answer in the conversation.
//
// Tags may be split across deltas: while the content can still become
// "<think>" (after optional leading whitespace) it is held, so content that
// does not start with the tag is released on the first delta that proves it
// (an answer starting "<div>" is held for at most the "<" delta) and passes
// through untouched. Inside the block, only a suffix that could begin
// "</think>" is held. A "<think>" anywhere but at the start is answer text.
type thinkSplitter struct {
	state   thinkState
	pending string
}

// write consumes one content delta and returns the answer text and reasoning
// it releases (either may be empty while bytes are held).
func (s *thinkSplitter) write(delta string) (text, reasoning string) {
	switch s.state {
	case thinkStart:
		s.pending += delta
		trimmed := strings.TrimLeft(s.pending, thinkSpace)
		switch {
		case strings.HasPrefix(trimmed, thinkOpenTag):
			s.state = thinkInside
			s.pending = ""
			return s.write(trimmed[len(thinkOpenTag):])
		case trimmed == "":
			// Only whitespace so far: hold — but no further once past
			// thinkHoldMax (see the constant).
			if len(s.pending) > thinkHoldMax {
				s.state = thinkPassthrough
				text, s.pending = s.pending, ""
				return text, ""
			}
			return "", ""
		case strings.HasPrefix(thinkOpenTag, trimmed):
			// The start of the opening tag: hold.
			return "", ""
		}
		s.state = thinkPassthrough
		text, s.pending = s.pending, ""
		return text, ""
	case thinkInside:
		buf := s.pending + delta
		if end := strings.Index(buf, thinkCloseTag); end >= 0 {
			s.state = thinkAfterClose
			s.pending = ""
			text, _ = s.write(buf[end+len(thinkCloseTag):])
			return text, buf[:end]
		}
		hold := partialSuffixLen(buf, thinkCloseTag)
		s.pending = buf[len(buf)-hold:]
		return "", buf[:len(buf)-hold]
	case thinkAfterClose:
		text = strings.TrimLeft(delta, thinkSpace)
		if text != "" {
			s.state = thinkPassthrough
		}
		return text, ""
	}
	return delta, ""
}

// flush releases whatever is still held at the end of the content: held
// leading bytes are answer text (they never became "<think>"); held bytes
// inside a block that never closed are reasoning — an answer cut off while the
// model was still thinking (finish reason "length"), which the chat service
// reports as an exhausted output budget rather than as a reply.
func (s *thinkSplitter) flush() (text, reasoning string) {
	held := s.pending
	s.pending = ""
	switch s.state {
	case thinkStart:
		return held, ""
	case thinkInside:
		return "", held
	}
	return "", ""
}

// splitLeadingThink splits complete (non-streamed) content into its answer and
// the reasoning of a leading <think> block, exactly as the streaming path does.
func splitLeadingThink(content string) (text, reasoning string) {
	var s thinkSplitter
	text, reasoning = s.write(content)
	ft, fr := s.flush()
	return text + ft, reasoning + fr
}

// stripLeadingThink drops a closed leading <think>…</think> block from a
// stored assistant message (history saved before reasoning was split off).
// Content without a closed leading block, or with nothing after it, is
// returned unchanged.
func stripLeadingThink(content string) string {
	trimmed := strings.TrimLeft(content, thinkSpace)
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return content
	}
	end := strings.Index(trimmed, thinkCloseTag)
	if end < 0 {
		return content
	}
	if answer := strings.TrimLeft(trimmed[end+len(thinkCloseTag):], thinkSpace); answer != "" {
		return answer
	}
	return content
}

// partialSuffixLen is the length of the longest suffix of s that is a proper
// prefix of tag (a tag possibly continuing in the next delta).
func partialSuffixLen(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// Note on cancellation: every provider builds its request with
// http.NewRequestWithContext, so cancelling the caller's context aborts the body
// read and ends the scanner loop (surfacing via scanner.Err()). No explicit
// per-line context check is needed in these parsers.

func parseClaudeSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var eventType string
	var doneSent bool
	var parseErrors int
	// tokensIn is captured from the message_start event and carried onto the
	// Done chunk. Claude emits input_tokens on a separate metadata chunk (not
	// on message_delta), so without this the Done chunk carries TokensIn=0 and
	// the client-visible token accounting is wrong.
	var tokensIn int
	// servedModel is the model named by message_start ("model"), carried
	// onto the Done chunk: a Claude deployment on Azure AI Foundry is called
	// by its deployment name and priced by the model that answered.
	var servedModel string
	// toolBlocks accumulates streamed tool_use blocks keyed by their content
	// block index; finalized onto the Done chunk at message_delta.
	toolBlocks := map[int]*pendingToolCall{}

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			eventType = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if !doneSent {
				onChunk(Chunk{Done: true, TokensIn: tokensIn, ToolCalls: assembleToolCalls(toolBlocks), ServedModel: servedModel})
			}
			return nil
		}

		switch eventType {
		case "message_start":
			var parsed struct {
				Message struct {
					Model string `json:"model"`
					Usage struct {
						InputTokens              int `json:"input_tokens"`
						CacheReadInputTokens     int `json:"cache_read_input_tokens"`
						CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			// Cache tokens are billed separately (reads ~0.1×, writes ~1.25×)
			// and are EXCLUDED from input_tokens — not counting them made
			// prompt-cached tool loops (the norm on long conversations)
			// systematically understate usage against the daily budget.
			// Counting both at full rate errs toward overstatement, the safe
			// direction for a spend cap.
			if m := strings.TrimSpace(parsed.Message.Model); m != "" {
				servedModel = m
			}
			u := parsed.Message.Usage
			if total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens; total > 0 {
				tokensIn = total
				onChunk(Chunk{TokensIn: tokensIn})
			}

		case "content_block_start":
			// A tool_use block opens here with its id+name; its JSON arguments
			// arrive as input_json_delta fragments on subsequent deltas.
			var parsed struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			if parsed.ContentBlock.Type == "tool_use" {
				toolBlocks[parsed.Index] = &pendingToolCall{
					id:   parsed.ContentBlock.ID,
					name: parsed.ContentBlock.Name,
				}
			}

		case "content_block_delta":
			var parsed struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			switch parsed.Delta.Type {
			case "text_delta":
				onChunk(Chunk{Text: parsed.Delta.Text})
			case "input_json_delta":
				if t := toolBlocks[parsed.Index]; t != nil {
					t.args.WriteString(parsed.Delta.PartialJSON)
				}
			}

		case "message_delta":
			var parsed struct {
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			if parsed.Delta.StopReason != "" {
				doneSent = true
				onChunk(Chunk{
					Done:         true,
					TokensIn:     tokensIn,
					TokensOut:    parsed.Usage.OutputTokens,
					ToolCalls:    assembleToolCalls(toolBlocks),
					FinishReason: parsed.Delta.StopReason,
					ServedModel:  servedModel,
				})
			}

		case "error":
			var parsed struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			onChunk(Chunk{Error: classifyClaudeStreamError(parsed.Error.Type, parsed.Error.Message)})
			return nil
		}

		eventType = ""
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading claude SSE stream: %w", err)
	}

	if !doneSent {
		if parseErrors > 0 {
			return errStreamMalformed("claude", parseErrors)
		}
		return errStreamTruncated("claude")
	}
	return nil
}

func parseOpenAISSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		doneSent     bool
		sawTerminal  bool
		tokensIn     int
		tokensOut    int
		parseErrors  int
		finishReason string
		// servedModel is the concrete model name the chunks report ("model").
		// It rides on every content chunk and on Done, not only on Done: a
		// stream cut before Done (Stop, idle watchdog, truncation) is still
		// priced by the model that answered instead of by the ceiling.
		servedModel string
		// toolBlocks accumulates streamed function calls keyed by their
		// slot (see toolCallSlot); finalized onto the Done chunk. slotByID
		// and lastSlot resolve fragments that carry no `index`.
		toolBlocks = map[int]*pendingToolCall{}
		slotByID   = map[string]int{}
		lastSlot   = -1
		// think splits a leading <think>…</think> block off the content (see
		// thinkSplitter): reasoning travels as Chunk.Reasoning, never as Text.
		think thinkSplitter
		// filtered: the content filter stopped the completion. The error is
		// emitted at the END of the stream: with stream_options.include_usage
		// the usage-only chunk follows the finish chunk, and returning at the
		// finish billed the blocked completion by estimate and dropped bytes
		// the think splitter still held.
		filtered bool
	)

	// flushThink releases the bytes the splitter still holds once the content
	// is over (clean end, truncation or a read error).
	flushThink := func() {
		if t, r := think.flush(); t != "" || r != "" {
			onChunk(Chunk{Text: t, Reasoning: r, ServedModel: servedModel})
		}
	}

	// Usage may arrive on the finish chunk or — when stream_options.include_usage
	// is set — in a trailing chunk whose `choices` array is empty (which the old
	// code dropped). Stash whatever usage we see and attach it to the single Done
	// chunk emitted at [DONE]/stream end, so the audited provider can record cost.
	emitDone := func() {
		if doneSent {
			return
		}
		flushThink()
		doneSent = true
		onChunk(Chunk{
			Done:         true,
			TokensIn:     tokensIn,
			TokensOut:    tokensOut,
			ToolCalls:    assembleToolCalls(toolBlocks),
			FinishReason: finishReason,
			ServedModel:  servedModel,
		})
	}

	// emitFiltered is the content-filter stop's terminal chunk: an error
	// carrying whatever usage and model the stream reported (the audited
	// wrapper reads them from any chunk; the retry wrapper re-attaches them
	// through heldUsageError when the chunk is pre-content).
	emitFiltered := func() {
		flushThink()
		onChunk(Chunk{
			Error:       fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered),
			TokensIn:    tokensIn,
			TokensOut:   tokensOut,
			ServedModel: servedModel,
		})
	}

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			if filtered {
				emitFiltered()
				return nil
			}
			emitDone()
			return nil
		}

		var parsed struct {
			Model   string `json:"model"`
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			// A server error after the 200 arrives as its own event.
			Error *responsesError `json:"error"`
		}

		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			parseErrors++
			continue
		}

		if parsed.Usage != nil {
			tokensIn = parsed.Usage.PromptTokens
			tokensOut = parsed.Usage.CompletionTokens
		}
		if parsed.Model != "" {
			servedModel = parsed.Model
		}
		if parsed.Error != nil && (parsed.Error.Message != "" || parsed.Error.Code != "") {
			// `data: {"error":{…}}` mid-stream used to decode as an empty
			// event and end as a "truncated" response with the message lost;
			// classified, a failure before any content is retried and the
			// breaker counts an outage, and the provider's text reaches the
			// user.
			flushThink()
			onChunk(Chunk{
				Error:       classifyOpenAIStreamError(parsed.Error.Code, parsed.Error.Type, parsed.Error.Message),
				TokensIn:    tokensIn,
				TokensOut:   tokensOut,
				ServedModel: servedModel,
			})
			return nil
		}

		emitted := false
		for _, choice := range parsed.Choices {
			if choice.Delta.Content != "" {
				if t, r := think.write(choice.Delta.Content); t != "" || r != "" {
					emitted = true
					onChunk(Chunk{Text: t, Reasoning: r, ServedModel: servedModel})
				}
			}
			// Tool calls stream as fragments: the first carries id+name, the rest
			// append argument text. Accumulate by slot.
			for _, tc := range choice.Delta.ToolCalls {
				idx := toolCallSlot(tc.Index, tc.ID, slotByID, toolBlocks, lastSlot)
				lastSlot = idx
				if tc.ID != "" {
					slotByID[tc.ID] = idx
				}
				p := toolBlocks[idx]
				if p == nil {
					p = &pendingToolCall{}
					toolBlocks[idx] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				// Azure OpenAI / Foundry end a stream whose completion the
				// content filter cut with finish_reason "content_filter" — a
				// deliberate stop, not a truncation, but the text so far is
				// not the answer. Surface it as an error chunk (the Gemini
				// SAFETY path does the same) so the client explains the block
				// instead of presenting a half answer as complete — at the end
				// of the stream, once the trailing usage chunk has arrived.
				if *choice.FinishReason == "content_filter" {
					filtered = true
					emitted = true // the stop emits only its error, at the end
					continue
				}
				sawTerminal = true
				finishReason = *choice.FinishReason
			}
		}
		// An event that produced no chunk — reasoning deltas
		// (delta.reasoning_content: DeepSeek, GLM, Grok mini, Cohere on
		// Foundry), tool-call argument fragments, content the think splitter
		// holds, usage-only and filter-annotation events — still proves the
		// provider is working: one keepalive per event feeds the chat idle
		// watchdog (as parseResponsesSSE's progress keepalives do), so a long
		// reasoning phase is not cancelled as a stalled stream. It carries
		// only the served model (consumers key on Text/Done/Error/ToolEvent),
		// so a stream stopped during a silent reasoning phase is still priced
		// by the model that took the request. After the choices loop, so a
		// content_filter stop emits only its error.
		if !emitted && !filtered {
			onChunk(Chunk{ServedModel: servedModel})
		}
	}

	if err := scanner.Err(); err != nil {
		if filtered {
			emitFiltered() // the verdict is known; usage may be missing
			return nil
		}
		flushThink()
		return fmt.Errorf("reading openai SSE stream: %w", err)
	}

	if filtered {
		emitFiltered()
		return nil
	}
	// A clean end is either an explicit [DONE] (handled above) or a terminal
	// finish_reason for servers that omit [DONE]. Anything else is a truncation.
	if sawTerminal {
		emitDone()
		return nil
	}
	if !doneSent {
		// The partial answer still reaches the caller before the error.
		flushThink()
		if parseErrors > 0 {
			return errStreamMalformed("openai", parseErrors)
		}
		return errStreamTruncated("openai")
	}
	return nil
}

// toolCallSlot picks the accumulator for a streamed tool-call fragment. The
// OpenAI contract keys fragments by `index`; some compatible servers behind
// a deployment name or an env-overridden base URL omit it and rely on `id` —
// decoding a missing index as 0 merged every parallel call into one and
// concatenated their argument JSON. Resolution: `index` when present, else
// the slot the `id` was first seen in (a new id takes the next free slot),
// else the last slot used (an id-less argument continuation).
func toolCallSlot(index *int, id string, byID map[string]int, blocks map[int]*pendingToolCall, last int) int {
	if index != nil {
		return *index
	}
	if id != "" {
		if s, ok := byID[id]; ok {
			return s
		}
		next := 0
		for i := range blocks {
			if i >= next {
				next = i + 1
			}
		}
		return next
	}
	if last >= 0 {
		return last
	}
	return 0
}

// classifyOpenAIStreamError maps a mid-stream `error` event of an
// OpenAI-compatible chat-completions stream onto the shared sentinels
// (rate limit, provider down, content filter — the table classifyResponsesError
// keeps for the Responses API), with chat-completions wording for the rest.
func classifyOpenAIStreamError(code, typ, msg string) error {
	err := classifyResponsesError(code, typ, msg)
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderDown) || errors.Is(err, ErrContentFiltered) {
		return err
	}
	if msg == "" {
		msg = code
	}
	return fmt.Errorf("openai stream error: %s", msg)
}

// geminiFilteredFinish lists Gemini finishReason values meaning the response
// was blocked or filtered server-side rather than completed. Treating these as
// a clean Done makes an empty/blocked answer indistinguishable from a normal
// completion — the user just sees nothing. STOP and MAX_TOKENS (and unknown
// reasons) stay clean completions.
var geminiFilteredFinish = map[string]bool{
	"SAFETY":             true,
	"RECITATION":         true,
	"BLOCKLIST":          true,
	"PROHIBITED_CONTENT": true,
}

func parseGeminiSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		doneSent    bool
		parseErrors int
		toolCalls   []ToolCall
	)

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		var parsed struct {
			Candidates []struct {
				Content struct {
					Parts []geminiPart `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				PromptTokenCount     int `json:"promptTokenCount"`
				CandidatesTokenCount int `json:"candidatesTokenCount"`
				// ThoughtsTokenCount is Gemini's billing for thinking tokens;
				// candidatesTokenCount EXCLUDES it, so 2.5-series models
				// systematically undercounted output without it.
				ThoughtsTokenCount int `json:"thoughtsTokenCount"`
			} `json:"usageMetadata"`
		}

		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			parseErrors++
			continue
		}

		for _, cand := range parsed.Candidates {
			for _, part := range cand.Content.Parts {
				if part.Text != "" {
					onChunk(Chunk{Text: part.Text})
				}
				if part.FunctionCall != nil {
					// A nil Args map marshals to the literal `null`, which is not a
					// valid tool-arguments object; send `{}` like the OpenAI
					// provider and the non-streaming Gemini path (gemini.go).
					args := []byte("{}")
					if part.FunctionCall.Args != nil {
						args, _ = json.Marshal(part.FunctionCall.Args)
					}
					// Process-unique ID (see geminiCallSeq): a per-stream counter
					// collides across tool-loop iterations.
					toolCalls = append(toolCalls, ToolCall{
						ID:    nextGeminiCallID(),
						Name:  part.FunctionCall.Name,
						Input: json.RawMessage(args),
					})
				}
			}
			if cand.FinishReason != "" {
				if !doneSent {
					doneSent = true
					tokensOut, tokensIn := 0, 0
					if parsed.UsageMetadata != nil {
						tokensOut = parsed.UsageMetadata.CandidatesTokenCount + parsed.UsageMetadata.ThoughtsTokenCount
						tokensIn = parsed.UsageMetadata.PromptTokenCount
					}
					if geminiFilteredFinish[cand.FinishReason] {
						// Terminal (no truncation error — the stream ended
						// deliberately), but surfaced as an error chunk so the
						// client explains the block instead of showing an
						// empty answer. Mirrors the Claude `error`-event path.
						onChunk(Chunk{Error: fmt.Errorf("gemini stopped the response (finishReason=%s): the response was blocked by a content filter", cand.FinishReason)})
						return nil
					}
					onChunk(Chunk{
						Done:         true,
						TokensOut:    tokensOut,
						TokensIn:     tokensIn,
						ToolCalls:    toolCalls,
						FinishReason: cand.FinishReason,
					})
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading gemini SSE stream: %w", err)
	}

	if !doneSent {
		if parseErrors > 0 {
			return errStreamMalformed("gemini", parseErrors)
		}
		return errStreamTruncated("gemini")
	}
	return nil
}
