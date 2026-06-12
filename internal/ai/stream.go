package ai

import (
	"bufio"
	"encoding/json"
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
				onChunk(Chunk{Done: true, ToolCalls: assembleToolCalls(toolBlocks)})
			}
			return nil
		}

		switch eventType {
		case "message_start":
			var parsed struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			if parsed.Message.Usage.InputTokens > 0 {
				onChunk(Chunk{TokensIn: parsed.Message.Usage.InputTokens})
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
					TokensOut:    parsed.Usage.OutputTokens,
					ToolCalls:    assembleToolCalls(toolBlocks),
					FinishReason: parsed.Delta.StopReason,
				})
			}

		case "error":
			var parsed struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(data), &parsed); err != nil {
				parseErrors++
				continue
			}
			onChunk(Chunk{Error: fmt.Errorf("claude stream error: %s", parsed.Error.Message)})
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
		// toolBlocks accumulates streamed function calls keyed by their position
		// in the tool_calls array; finalized onto the Done chunk.
		toolBlocks = map[int]*pendingToolCall{}
	)

	// Usage may arrive on the finish chunk or — when stream_options.include_usage
	// is set — in a trailing chunk whose `choices` array is empty (which the old
	// code dropped). Stash whatever usage we see and attach it to the single Done
	// chunk emitted at [DONE]/stream end, so the audited provider can record cost.
	emitDone := func() {
		if doneSent {
			return
		}
		doneSent = true
		onChunk(Chunk{
			Done:         true,
			TokensIn:     tokensIn,
			TokensOut:    tokensOut,
			ToolCalls:    assembleToolCalls(toolBlocks),
			FinishReason: finishReason,
		})
	}

	for scanner.Scan() {
		line := scanner.Text()

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			emitDone()
			return nil
		}

		var parsed struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
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
		}

		if err := json.Unmarshal([]byte(data), &parsed); err != nil {
			parseErrors++
			continue
		}

		if parsed.Usage != nil {
			tokensIn = parsed.Usage.PromptTokens
			tokensOut = parsed.Usage.CompletionTokens
		}

		for _, choice := range parsed.Choices {
			if choice.Delta.Content != "" {
				onChunk(Chunk{Text: choice.Delta.Content})
			}
			// Tool calls stream as fragments: the first carries id+name, the rest
			// append argument text. Accumulate by index.
			for _, tc := range choice.Delta.ToolCalls {
				p := toolBlocks[tc.Index]
				if p == nil {
					p = &pendingToolCall{}
					toolBlocks[tc.Index] = p
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
				sawTerminal = true
				finishReason = *choice.FinishReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading openai SSE stream: %w", err)
	}

	// A clean end is either an explicit [DONE] (handled above) or a terminal
	// finish_reason for servers that omit [DONE]. Anything else is a truncation.
	if sawTerminal {
		emitDone()
		return nil
	}
	if !doneSent {
		if parseErrors > 0 {
			return errStreamMalformed("openai", parseErrors)
		}
		return errStreamTruncated("openai")
	}
	return nil
}

func parseGeminiSSE(body io.Reader, onChunk func(Chunk)) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		doneSent     bool
		parseErrors  int
		toolCallsIdx int
		toolCalls    []ToolCall
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
					args, _ := json.Marshal(part.FunctionCall.Args)
					if len(args) == 0 {
						args = []byte("{}")
					}
					toolCalls = append(toolCalls, ToolCall{
						ID:    fmt.Sprintf("call_%d", toolCallsIdx),
						Name:  part.FunctionCall.Name,
						Input: json.RawMessage(args),
					})
					toolCallsIdx++
				}
			}
			if cand.FinishReason != "" {
				if !doneSent {
					doneSent = true
					tokensOut, tokensIn := 0, 0
					if parsed.UsageMetadata != nil {
						tokensOut = parsed.UsageMetadata.CandidatesTokenCount
						tokensIn = parsed.UsageMetadata.PromptTokenCount
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
