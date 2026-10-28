package ai

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// pendingToolCall accumulates a streamed tool call: the id/name arrive on the
// block-start event, the JSON arguments arrive as a series of fragments.
type pendingToolCall struct {
	id   string
	name string
	args strings.Builder
}

// assembleToolCalls flattens an index-keyed map of partial tool calls into []ToolCall, ordered by block index.
// Blank (empty or whitespace-only) argument buffers become "{}", as in the Responses parser: whitespace is not JSON,
// yet the chat service and ExecuteTool read it as "no arguments", so it was recorded verbatim and broke the replay.
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
		if strings.TrimSpace(input) == "" {
			input = "{}"
		}
		out = append(out, ToolCall{ID: p.id, Name: p.name, Input: json.RawMessage(input)})
	}
	return out
}

// errStreamTruncated wraps io.ErrUnexpectedEOF for SSE bodies that ended cleanly without a provider-specific
// terminal marker — a Done chunk synthesised there made a truncated response look complete.
func errStreamTruncated(provider string) error {
	return fmt.Errorf("%s stream truncated before terminal marker: %w", provider, io.ErrUnexpectedEOF)
}

// sseField returns the value of the SSE field line "<name>:<value>" and whether line is that field. The one space
// after the colon is optional (the SSE format drops a single leading space): "data:{…}" reads like "data: {…}".
func sseField(line []byte, name string) ([]byte, bool) {
	if len(line) <= len(name) || line[len(name)] != ':' || string(line[:len(name)]) != name {
		return nil, false
	}
	v := line[len(name)+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return v, true
}

// errStreamMalformed reports a stream that ended without a terminal marker AND had undecodable `data:`
// events — silently skipping them made a fully malformed stream look like a clean empty response.
func errStreamMalformed(provider string, n int) error {
	return fmt.Errorf("%s stream had %d undecodable event(s) and no terminal marker", provider, n)
}

// classifyClaudeStreamError maps a mid-stream `error` event onto the shared sentinels, mirroring the HTTP-status
// mapping (429 → ErrRateLimited, 5xx/overloaded → ErrProviderDown); unknown types stay plain (permanent).
func classifyClaudeStreamError(errType, msg string) error {
	switch errType {
	case "rate_limit_error":
		return fmt.Errorf("%w: claude stream error: %s", ErrRateLimited, msg)
	case "overloaded_error", "api_error", "timeout_error":
		// timeout_error is the API's 504 delivered after the 200: transient
		// like a 5xx, so a failure before any content is retried.
		return fmt.Errorf("%w: claude stream error: %s", ErrProviderDown, msg)
	}
	return fmt.Errorf("claude stream error: %s", msg)
}

// maxClaudeSSELine caps one Claude SSE line: a signature_delta or a redacted_thinking block's data arrives as a
// single event and grows with the thinking it encrypts. Same cap as the Responses parser.
const maxClaudeSSELine = 32 << 20

// claudeStreamBlock accumulates one streamed content block for the turn's replay record: the block as
// content_block_start opened it (every field, so an unknown type survives the replay) plus its deltas.
type claudeStreamBlock struct {
	typ    string
	fields map[string]json.RawMessage
	text   strings.Builder
	sig    strings.Builder
}

func (b *claudeStreamBlock) startString(key string) string {
	var s string
	_ = json.Unmarshal(b.fields[key], &s)
	return s
}

// record is the block as the non-streaming API would have returned it; ok is false when it cannot be rebuilt
// faithfully (an unknown type, a tool_use without its accumulator, invalid input JSON).
func (b *claudeStreamBlock) record(tool *pendingToolCall) (json.RawMessage, bool) {
	out := make(map[string]json.RawMessage, len(b.fields)+2)
	for k, v := range b.fields {
		out[k] = v
	}
	out["type"], _ = json.Marshal(b.typ)
	str := func(s string) json.RawMessage {
		raw, _ := json.Marshal(s)
		return raw
	}
	switch b.typ {
	case "text":
		out["text"] = str(b.startString("text") + b.text.String())
	case "thinking":
		// Kept even when the text is empty (display "omitted"): the
		// signature carries the encrypted reasoning the API reconstructs.
		out["thinking"] = str(b.startString("thinking") + b.text.String())
		out["signature"] = str(b.startString("signature") + b.sig.String())
	case "redacted_thinking":
		// Its encrypted data arrives whole on content_block_start.
	case "tool_use":
		if tool == nil {
			return nil, false
		}
		input := tool.args.String()
		if strings.TrimSpace(input) == "" {
			// Blank, as assembleToolCalls reads it: the call went out as {}, and the record must agree or the
			// whole record (and the thinking before the tool_use) is dropped from the replay.
			input = "{}"
			if start, ok := b.fields["input"]; ok && len(start) > 0 && string(start) != "null" {
				input = string(start)
			}
		}
		if !json.Valid([]byte(input)) {
			return nil, false
		}
		out["input"] = json.RawMessage(input)
	default:
		return nil, false
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// claudeStreamRecord assembles the turn's replay record from the streamed blocks in index order; nil when
// there are none or any block cannot be rebuilt — the turn is then replayed from its text and tool calls, without its thinking.
func claudeStreamRecord(blocks map[int]*claudeStreamBlock, tools map[int]*pendingToolCall, broken bool) json.RawMessage {
	if broken || len(blocks) == 0 {
		return nil
	}
	idxs := make([]int, 0, len(blocks))
	for i := range blocks {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	out := make([]json.RawMessage, 0, len(idxs))
	for _, i := range idxs {
		raw, ok := blocks[i].record(tools[i])
		if !ok {
			return nil
		}
		out = append(out, raw)
	}
	return claudeReplayRecord(out)
}

// Inline reasoning markers: DeepSeek-R1 and other open reasoning models on OpenAI-compatible chat completions
// (Foundry's /openai/v1, vLLM) put their chain of thought in the answer's own content, in a leading <think>…</think> block.
const (
	thinkOpenTag  = "<think>"
	thinkCloseTag = "</think>"
	// thinkSpace is the whitespace allowed before the opening tag and dropped between the closing tag and the answer.
	thinkSpace = " \t\r\n"
	// thinkHoldMax caps how much whitespace thinkStart holds: every delta "could" still precede "<think>", so
	// the pending buffer grew without bound; past the cap the whitespace is released as answer text.
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

// thinkSplitter separates a LEADING <think>…</think> block from the answer in streamed content, so the
// reasoning never becomes answer text. Tags may be split across deltas: content that can still become
// "<think>" is held; a "<think>" anywhere but at the start is answer text.
type thinkSplitter struct {
	state   thinkState
	pending string
}

// write consumes one content delta and returns the answer text and reasoning it releases.
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
			// Only whitespace so far: hold — no further once past thinkHoldMax.
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

// flush releases what is still held: held leading bytes are answer text; held bytes inside a block that never
// closed are reasoning — an answer cut off while the model was still thinking.
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

// splitLeadingThink splits complete (non-streamed) content into its answer and the reasoning of a leading <think> block, as the streaming path does.
func splitLeadingThink(content string) (text, reasoning string) {
	var s thinkSplitter
	text, reasoning = s.write(content)
	ft, fr := s.flush()
	return text + ft, reasoning + fr
}

// stripLeadingThink drops a closed leading <think>…</think> block from a stored assistant message (history
// saved before reasoning was split off). Content without a closed leading block, or with nothing after it, is unchanged.
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

// partialSuffixLen is the length of the longest suffix of s that is a proper prefix of tag (a tag possibly continuing in the next delta).
func partialSuffixLen(s, tag string) int {
	for n := min(len(tag)-1, len(s)); n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// Note on cancellation: every provider builds its request with http.NewRequestWithContext, so cancelling the
// caller's context aborts the body read and ends the scanner loop; no per-line context check is needed.

func parseClaudeSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxClaudeSSELine)

	var eventType string
	var doneSent bool
	var parseErrors int
	// tokensIn is captured from message_start and carried onto the Done chunk (and an error chunk): Claude
	// emits input_tokens on a separate metadata chunk, not on message_delta.
	var tokensIn int
	// cacheRead/cacheWrite are the cached slice OF tokensIn (message_start's cache_read_input_tokens /
	// cache_creation_input_tokens), carried next to it so the audited wrapper prices that slice at the cached rates.
	var cacheRead, cacheWrite int
	// servedModel is the model named by message_start, carried on every chunk after it: a Claude deployment on
	// Foundry is called by deployment name and priced by the model that answered, even mid-failure.
	var servedModel string
	// toolBlocks accumulates streamed tool_use blocks keyed by their content block index; finalized at message_delta.
	toolBlocks := map[int]*pendingToolCall{}
	// blocks accumulates EVERY content block by index for the turn's replay record on the Done chunk
	// (Chunk.ProviderItems): within a tool loop the API wants the assistant turn back exactly as produced.
	// recordBroken: a delta arrived for a block that never opened, so the record could not be faithful.
	blocks := map[int]*claudeStreamBlock{}
	recordBroken := false
	// blockFor returns the accumulator of a delta's block, opening a text or thinking block a server skipped the start for.
	blockFor := func(index int, typ string) *claudeStreamBlock {
		if b := blocks[index]; b != nil {
			return b
		}
		b := &claudeStreamBlock{typ: typ}
		blocks[index] = b
		return b
	}
	record := func() json.RawMessage { return claudeStreamRecord(blocks, toolBlocks, recordBroken) }

	// emitted records whether the current event delivered a chunk (see the keepalive below).
	var emitted bool
	emit := func(c Chunk) {
		emitted = true
		if c.ServedModel == "" {
			c.ServedModel = servedModel
		}
		onChunk(c)
	}

	// handle processes one data line of eventType; true ends the stream (a mid-stream error or a refusal, whose chunk was delivered).
	handle := func(data []byte) bool {
		// The stream ends once: after its Done, a second stop_reason, a refusal or an error event would be a second
		// terminal chunk (usage recorded twice, a delivered answer failed), so it is ignored like a read error is below.
		if doneSent && (eventType == "message_delta" || eventType == "error") {
			return false
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
			if err := json.Unmarshal(data, &parsed); err != nil {
				parseErrors++
				return false
			}
			// Cache tokens are billed separately (reads 0.025-0.1× by family, writes 1.25×) and are EXCLUDED
			// from input_tokens; folded into TokensIn as the total AND carried as the CacheRead/WriteTokens breakdown.
			if m := strings.TrimSpace(parsed.Message.Model); m != "" {
				servedModel = m
			}
			u := parsed.Message.Usage
			if total := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens; total > 0 {
				tokensIn = total
				cacheRead, cacheWrite = u.CacheReadInputTokens, u.CacheCreationInputTokens
				emit(Chunk{TokensIn: tokensIn, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
			}

		case "content_block_start":
			// Every block opens here — a tool_use with its id+name (its JSON arguments arrive as
			// input_json_delta fragments), a thinking block empty, a redacted_thinking block with its data whole.
			var parsed struct {
				Index        int             `json:"index"`
				ContentBlock json.RawMessage `json:"content_block"`
			}
			if err := json.Unmarshal(data, &parsed); err != nil {
				parseErrors++
				return false
			}
			var head struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			var fields map[string]json.RawMessage
			if len(parsed.ContentBlock) > 0 {
				_ = json.Unmarshal(parsed.ContentBlock, &head)
				_ = json.Unmarshal(parsed.ContentBlock, &fields)
			}
			blocks[parsed.Index] = &claudeStreamBlock{typ: head.Type, fields: fields}
			if head.Type == "tool_use" {
				toolBlocks[parsed.Index] = &pendingToolCall{
					id:   head.ID,
					name: head.Name,
				}
			}

		case "content_block_delta":
			var parsed struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					Signature   string `json:"signature"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(data, &parsed); err != nil {
				parseErrors++
				return false
			}
			switch parsed.Delta.Type {
			case "text_delta":
				blockFor(parsed.Index, "text").text.WriteString(parsed.Delta.Text)
				emit(Chunk{Text: parsed.Delta.Text})
			case "thinking_delta":
				// Recorded for the replay, never emitted: thinking is not answer text (the event still yields a keepalive below).
				blockFor(parsed.Index, "thinking").text.WriteString(parsed.Delta.Thinking)
			case "signature_delta":
				blockFor(parsed.Index, "thinking").sig.WriteString(parsed.Delta.Signature)
			case "input_json_delta":
				if t := toolBlocks[parsed.Index]; t != nil {
					t.args.WriteString(parsed.Delta.PartialJSON)
				} else {
					recordBroken = true
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
			if err := json.Unmarshal(data, &parsed); err != nil {
				parseErrors++
				return false
			}
			switch parsed.Delta.StopReason {
			case "":
			case "refusal":
				// Claude's safety classifiers declined: the text so far is not the answer. The error chunk carries the usage and model.
				doneSent = true
				emit(Chunk{Error: claudeRefusalError(), TokensIn: tokensIn, TokensOut: parsed.Usage.OutputTokens, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
				return true
			default:
				doneSent = true
				emit(Chunk{
					Done:             true,
					TokensIn:         tokensIn,
					TokensOut:        parsed.Usage.OutputTokens,
					CacheReadTokens:  cacheRead,
					CacheWriteTokens: cacheWrite,
					ToolCalls:        assembleToolCalls(toolBlocks),
					FinishReason:     parsed.Delta.StopReason,
					ProviderItems:    record(),
				})
			}

		case "error":
			var parsed struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(data, &parsed); err != nil {
				parseErrors++
				return false
			}
			emit(Chunk{Error: classifyClaudeStreamError(parsed.Error.Type, parsed.Error.Message), TokensIn: tokensIn, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite})
			return true
		}
		return false
	}

	// The line is read zero-copy, as parseResponsesSSE does: scanner.Bytes() aliases the scan buffer, valid
	// only until the next Scan; the eventType conversion is the one deliberate copy.
	doneMarker := []byte("[DONE]")
	for scanner.Scan() {
		line := scanner.Bytes()

		if ev, ok := sseField(line, "event"); ok {
			eventType = string(ev)
			continue
		}

		data, ok := sseField(line, "data")
		if !ok {
			continue
		}
		if bytes.Equal(data, doneMarker) {
			if !doneSent {
				onChunk(Chunk{Done: true, TokensIn: tokensIn, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite, ToolCalls: assembleToolCalls(toolBlocks), ServedModel: servedModel, ProviderItems: record()})
			}
			return nil
		}

		emitted = false
		stop := handle(data)
		eventType = ""
		if stop {
			return nil
		}
		// An event that delivered nothing still proves the provider is working: one keepalive per event feeds
		// the chat idle watchdog, so a long thinking phase is not cancelled as a stalled stream.
		if !emitted {
			onChunk(Chunk{ServedModel: servedModel})
		}
	}

	if err := scanner.Err(); err != nil {
		if doneSent {
			// The answer and its usage were delivered; a read error on the trailing message_stop loses nothing.
			return nil
		}
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
		// cacheRead is the cached slice OF tokensIn (usage.prompt_tokens_details.cached_tokens — Azure reports
		// cache hits there; prompt_tokens already includes them), carried so the audited wrapper prices it as cached input.
		cacheRead int
		// servedModel is the concrete model name the chunks report, riding on every content chunk and on Done,
		// not only on Done: a stream cut before Done is still attributed to the model that answered.
		servedModel string
		// toolBlocks accumulates streamed function calls keyed by their slot (see toolCallSlot); slotByID and
		// lastSlot resolve fragments that carry no `index`.
		toolBlocks = map[int]*pendingToolCall{}
		slotByID   = map[string]int{}
		lastSlot   = -1
		// think splits a leading <think>…</think> block off the content (thinkSplitter): reasoning travels as Chunk.Reasoning, never as Text.
		think thinkSplitter
		// filtered: the content filter stopped the completion; the error is emitted at the END of the stream —
		// the trailing usage chunk (stream_options.include_usage) must arrive first, and the think splitter may still hold bytes.
		filtered bool
	)

	// flushThink releases the bytes the splitter still holds once the content is over.
	flushThink := func() {
		if t, r := think.flush(); t != "" || r != "" {
			onChunk(Chunk{Text: t, Reasoning: r, ServedModel: servedModel})
		}
	}

	// Usage may arrive on the finish chunk or — with stream_options.include_usage — in a trailing chunk whose
	// `choices` is empty; stash whatever usage is seen and attach it to the single Done chunk.
	emitDone := func() {
		if doneSent {
			return
		}
		flushThink()
		doneSent = true
		onChunk(Chunk{
			Done:            true,
			TokensIn:        tokensIn,
			TokensOut:       tokensOut,
			CacheReadTokens: cacheRead,
			ToolCalls:       assembleToolCalls(toolBlocks),
			FinishReason:    finishReason,
			ServedModel:     servedModel,
		})
	}

	// emitFiltered is the content-filter stop's terminal chunk: an error carrying whatever usage and model the stream reported.
	emitFiltered := func() {
		flushThink()
		onChunk(Chunk{
			Error:           fmt.Errorf("%w: the response was stopped by the content filter", ErrContentFiltered),
			TokensIn:        tokensIn,
			TokensOut:       tokensOut,
			CacheReadTokens: cacheRead,
			ServedModel:     servedModel,
		})
	}

	// Zero-copy lines as parseResponsesSSE reads them; json.Unmarshal or WriteString copies everything kept.
	doneMarker := []byte("[DONE]")
	for scanner.Scan() {
		data, ok := sseField(scanner.Bytes(), "data")
		if !ok {
			continue
		}
		if bytes.Equal(data, doneMarker) {
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
					Content string `json:"content"`
					// ReasoningContent is the separate reasoning channel of DeepSeek, GLM, Grok mini and
					// other open reasoning models on Foundry's /openai/v1 (and vLLM): reasoning, never answer text.
					ReasoningContent string `json:"reasoning_content"`
					ToolCalls        []struct {
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
				// prompt_tokens_details.cached_tokens is the cached slice of prompt_tokens (Azure's prompt caching): a breakdown, never an addition.
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
			// A server error after the 200 arrives as its own event. Code and type stay raw: vLLM sends the
			// HTTP status as a number ("code":400), which a string field failed to decode — the event was then
			// skipped as undecodable and the [DONE] after it closed the stream as a clean, empty completion.
			Error *struct {
				Code    json.RawMessage `json:"code"`
				Type    json.RawMessage `json:"type"`
				Message string          `json:"message"`
			} `json:"error"`
		}

		if err := json.Unmarshal(data, &parsed); err != nil {
			parseErrors++
			continue
		}

		if parsed.Usage != nil {
			tokensIn = parsed.Usage.PromptTokens
			tokensOut = parsed.Usage.CompletionTokens
			if d := parsed.Usage.PromptTokensDetails; d != nil {
				// A slice of prompt_tokens, never more: the upstream's numbers are not trusted to be consistent.
				cacheRead = min(d.CachedTokens, parsed.Usage.PromptTokens)
			}
		}
		if parsed.Model != "" {
			servedModel = parsed.Model
		}
		if parsed.Error != nil && (parsed.Error.Message != "" || rawText(parsed.Error.Code) != "") {
			if sawTerminal || filtered {
				// After the terminal finish_reason the answer is complete (or the filter verdict stands): an error
				// event before [DONE] would fail a delivered answer, so it is ignored, as the Claude and Gemini
				// parsers ignore events after their terminal chunk. A usage chunk after it still counts.
				continue
			}
			// `data: {"error":{…}}` mid-stream used to decode as an empty event and end "truncated" with the
			// message lost; classified, a failure before any content is retried and the breaker counts an outage.
			flushThink()
			onChunk(Chunk{
				Error:           classifyOpenAIStreamError(rawText(parsed.Error.Code), rawText(parsed.Error.Type), parsed.Error.Message),
				TokensIn:        tokensIn,
				TokensOut:       tokensOut,
				CacheReadTokens: cacheRead,
				ServedModel:     servedModel,
			})
			return nil
		}

		emitted := false
		for _, choice := range parsed.Choices {
			if choice.Delta.ReasoningContent != "" {
				// Chunk.Reasoning only: not shown as the answer, not stored, not re-sent as history (it still feeds the idle watchdog).
				emitted = true
				onChunk(Chunk{Reasoning: choice.Delta.ReasoningContent, ServedModel: servedModel})
			}
			if choice.Delta.Content != "" {
				if t, r := think.write(choice.Delta.Content); t != "" || r != "" {
					emitted = true
					onChunk(Chunk{Text: t, Reasoning: r, ServedModel: servedModel})
				}
			}
			// Tool calls stream as fragments: the first carries id+name, the rest append argument text.
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
				// Azure OpenAI / Foundry end a filter-cut completion with finish_reason "content_filter": a
				// deliberate stop, but the text so far is not the answer (error chunk at the end of the stream).
				if *choice.FinishReason == "content_filter" {
					filtered = true
					emitted = true // the stop emits only its error, at the end
					continue
				}
				sawTerminal = true
				finishReason = *choice.FinishReason
			}
		}
		// An event that produced no chunk still proves the provider is working: one keepalive per event feeds
		// the idle watchdog, carrying only the served model (so a stream stopped in a silent reasoning phase is
		// still priced by the model that took the request).
		if !emitted && !filtered {
			onChunk(Chunk{ServedModel: servedModel})
		}
	}

	if err := scanner.Err(); err != nil {
		if filtered {
			emitFiltered() // the verdict is known; usage may be missing
			return nil
		}
		if sawTerminal {
			// The answer was complete: a read error before the usage chunk or [DONE] loses only the usage.
			emitDone()
			return nil
		}
		flushThink()
		return fmt.Errorf("reading openai SSE stream: %w", err)
	}

	if filtered {
		emitFiltered()
		return nil
	}
	// A clean end is either an explicit [DONE] (handled above) or a terminal finish_reason for servers that omit [DONE]. Anything else is a truncation.
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

// toolCallSlot picks a fragment's accumulator: the OpenAI contract keys fragments by `index`, but some
// compatible servers omit it and rely on `id` — a missing index decoded as 0 merged parallel calls into one.
// Resolution: `index`, else the slot the `id` was first seen in (a new id takes the next free slot), else the last slot.
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

// classifyOpenAIStreamError maps a mid-stream `error` event onto the shared sentinels via
// classifyResponsesError's table, then a numeric code as the HTTP status it is (vLLM), with chat-completions
// wording for the rest.
func classifyOpenAIStreamError(code, typ, msg string) error {
	err := classifyResponsesError(code, typ, msg)
	if errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderDown) || errors.Is(err, ErrContentFiltered) {
		return err
	}
	if msg == "" {
		msg = code
	}
	if status, convErr := strconv.Atoi(code); convErr == nil {
		// Routed as handleStreamError routes the same HTTP status (without the provider's 401/403 hooks).
		switch {
		case status == http.StatusUnauthorized:
			return fmt.Errorf("%w: openai stream error: %s", ErrApiKeyInvalid, msg)
		case status == http.StatusForbidden:
			return NewAccessDeniedError(cmp.Or(vendorMessage(msg), "the provider answered 403"))
		case status == http.StatusTooManyRequests:
			return fmt.Errorf("%w: openai stream error: %s", ErrRateLimited, msg)
		case status >= http.StatusInternalServerError:
			return fmt.Errorf("%w: openai stream error: %s", ErrProviderDown, msg)
		}
		if err := detectContextLimitError(status, msg); err != nil {
			return err
		}
		if err := detectToolsUnsupportedError(status, msg); err != nil {
			return err
		}
	}
	return fmt.Errorf("openai stream error: %s", msg)
}

// geminiFilteredFinish lists Gemini finishReason values meaning the response was blocked or filtered
// server-side — treating these as a clean Done made a blocked answer indistinguishable from a completion.
var geminiFilteredFinish = map[string]bool{
	"SAFETY":             true,
	"RECITATION":         true,
	"BLOCKLIST":          true,
	"PROHIBITED_CONTENT": true,
	// SPII: "the content potentially contains Sensitive Personally Identifiable Information".
	"SPII": true,
}

// parseGeminiSSE reads Gemini's streamGenerateContent SSE. Like the Claude and OpenAI parsers it emits a
// keepalive for an event that delivers nothing (usage only, safety ratings, a functionCall collected for Done),
// so the idle watchdog sees the stream alive; carries the served model (modelVersion) on its chunks; keeps the
// reported usage on a filtered finish; and turns an error event after the 200 into a classified error chunk
// with the usage seen so far, ending the stream with nil like the other parsers (R-streaming-1).
func parseGeminiSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		doneSent            bool
		parseErrors         int
		servedModel         string
		tokensIn, tokensOut int
		cacheRead           int
		// turn collects the function calls and the replay record (thought signatures, issued call ids).
		turn geminiTurn
	)

	// Zero-copy lines as parseResponsesSSE reads them; json.Unmarshal copies what is retained.
	for scanner.Scan() {
		data, ok := sseField(scanner.Bytes(), "data")
		if !ok {
			continue
		}

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
				// ThoughtsTokenCount is Gemini's billing for thinking tokens; candidatesTokenCount EXCLUDES it,
				// so 2.5-series models systematically undercounted output without it.
				ThoughtsTokenCount int `json:"thoughtsTokenCount"`
				// CachedContentTokenCount is the cached slice OF promptTokenCount (implicit or explicit caching).
				CachedContentTokenCount int `json:"cachedContentTokenCount"`
			} `json:"usageMetadata"`
			ModelVersion string `json:"modelVersion"`
			// Error is an error event the server sends after the 200 (overload, quota).
			Error *geminiAPIError `json:"error"`
			// PromptFeedback.BlockReason set: the prompt was blocked and no candidates come — a 200 whose only
			// event carries it. Read as an empty event, it ended the stream "truncated" (transient, retried).
			PromptFeedback *struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
		}

		if err := json.Unmarshal(data, &parsed); err != nil {
			parseErrors++
			continue
		}
		if parsed.ModelVersion != "" {
			servedModel = parsed.ModelVersion
		}
		if u := parsed.UsageMetadata; u != nil {
			tokensIn = u.PromptTokenCount
			tokensOut = u.CandidatesTokenCount + u.ThoughtsTokenCount
			// A slice of the prompt, never more: the upstream's numbers are not trusted to be consistent.
			cacheRead = min(u.CachedContentTokenCount, u.PromptTokenCount)
		}
		// After the finish the stream has ended: a later error event would be a second terminal chunk.
		if parsed.Error != nil && !doneSent {
			err := classifyGeminiAPIError(parsed.Error.Code, *parsed.Error, 0)
			onChunk(Chunk{Error: err, TokensIn: tokensIn, TokensOut: tokensOut, ServedModel: servedModel})
			return nil
		}
		if pf := parsed.PromptFeedback; pf != nil && pf.BlockReason != "" && !doneSent {
			onChunk(Chunk{
				Error:       fmt.Errorf("%w: gemini blocked the prompt (blockReason=%s)", ErrContentFiltered, pf.BlockReason),
				TokensIn:    tokensIn,
				TokensOut:   tokensOut,
				ServedModel: servedModel,
			})
			return nil
		}

		emitted := false
		for _, cand := range parsed.Candidates {
			for _, part := range cand.Content.Parts {
				if text := turn.part(part); text != "" {
					onChunk(Chunk{Text: text, ServedModel: servedModel})
					emitted = true
				}
			}
			if cand.FinishReason != "" && !doneSent {
				doneSent = true
				emitted = true
				if cand.FinishReason == geminiMissingSignatureFinish {
					// The history lacks a thought signature (documented finish reason, not a captured one): a
					// deterministic refusal, not an empty answer.
					onChunk(Chunk{
						Error:       fmt.Errorf("%w (finishReason %s)", ErrThoughtSignatureMissing, cand.FinishReason),
						TokensIn:    tokensIn,
						TokensOut:   tokensOut,
						ServedModel: servedModel,
					})
					return nil
				}
				if geminiFilteredFinish[cand.FinishReason] {
					// Terminal (no truncation error — the stream ended deliberately) but an error chunk, so the
					// client sees the block instead of an empty answer; the usage Gemini priced rides along.
					onChunk(Chunk{
						Error:       fmt.Errorf("%w: gemini stopped the response (finishReason=%s)", ErrContentFiltered, cand.FinishReason),
						TokensIn:    tokensIn,
						TokensOut:   tokensOut,
						ServedModel: servedModel,
					})
					return nil
				}
				onChunk(Chunk{
					Done:            true,
					TokensOut:       tokensOut,
					TokensIn:        tokensIn,
					CacheReadTokens: cacheRead,
					ToolCalls:       turn.calls,
					FinishReason:    cand.FinishReason,
					ServedModel:     servedModel,
					ProviderItems:   turn.items(),
				})
			}
		}
		if !emitted {
			// Nothing to deliver, but the upstream is alive: feed the idle watchdog.
			onChunk(Chunk{ServedModel: servedModel})
		}
	}

	if err := scanner.Err(); err != nil {
		if doneSent {
			// The answer and its usage were delivered, as in the Claude parser: a read error after the finish loses nothing.
			return nil
		}
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
