package ai

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// Parser defects found while hand-building the wire fixtures (R-testing-10, testdata/wire/README.md).

func gatherGeminiChunks(input string) ([]Chunk, error) {
	var out []Chunk
	err := parseGeminiSSE(strings.NewReader(input), func(c Chunk) { out = append(out, c) })
	return out, err
}

func gatherOpenAIChunks(input string) ([]Chunk, error) {
	var out []Chunk
	err := parseOpenAISSE(strings.NewReader(input), func(c Chunk) { out = append(out, c) })
	return out, err
}

// lastChunkError is the last chunk's error, or nil when no chunk carried one.
func lastChunkError(chunks []Chunk) error {
	for i := len(chunks) - 1; i >= 0; i-- {
		if chunks[i].Error != nil {
			return chunks[i].Error
		}
	}
	return nil
}

// TestParseGeminiSSE_BlockedPromptIsContentFiltered: Gemini answers a prompt its safety filters block with a 200
// whose only event carries promptFeedback.blockReason and no candidates ("If set, the prompt was blocked and no
// candidates are returned"). Read as an event with nothing in it, the stream ended "truncated" — a transient
// io.ErrUnexpectedEOF the retry wrapper replays — instead of the deterministic content-filter stop it is.
func TestParseGeminiSSE_BlockedPromptIsContentFiltered(t *testing.T) {
	input := `data: {"promptFeedback": {"blockReason": "SAFETY", "safetyRatings": [{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "probability": "HIGH", "blocked": true}]}, "usageMetadata": {"promptTokenCount": 21, "totalTokenCount": 21}, "modelVersion": "gemini-2.5-flash"}` + "\n\n"
	chunks, err := gatherGeminiChunks(input)
	if err != nil {
		t.Fatalf("a blocked prompt is a deliberate stop, not a failed read: got %v", err)
	}
	cerr := lastChunkError(chunks)
	if !errors.Is(cerr, ErrContentFiltered) {
		t.Fatalf("want an ErrContentFiltered chunk, got %v (chunks %+v)", cerr, chunks)
	}
	last := chunks[len(chunks)-1]
	if last.TokensIn != 21 || last.ServedModel != "gemini-2.5-flash" {
		t.Errorf("the error chunk must carry the priced prompt and the model: tokensIn=%d served=%q", last.TokensIn, last.ServedModel)
	}
	if !strings.Contains(cerr.Error(), "SAFETY") {
		t.Errorf("the error should name the block reason: %v", cerr)
	}
}

// TestParseGeminiSSE_SPIIFinishIsContentFiltered: finishReason SPII ("the content potentially contains Sensitive
// Personally Identifiable Information") is a filter stop like SAFETY, not a clean completion.
func TestParseGeminiSSE_SPIIFinishIsContentFiltered(t *testing.T) {
	input := `data: {"candidates": [{"content": {"parts": [{"text": "The number is"}], "role": "model"}}], "modelVersion": "gemini-2.5-pro"}` + "\n\n" +
		`data: {"candidates": [{"content": {"parts": [{"text": ""}], "role": "model"}, "finishReason": "SPII"}], "usageMetadata": {"promptTokenCount": 9, "candidatesTokenCount": 3, "totalTokenCount": 12}, "modelVersion": "gemini-2.5-pro"}` + "\n\n"
	chunks, err := gatherGeminiChunks(input)
	if err != nil {
		t.Fatalf("got %v", err)
	}
	for _, c := range chunks {
		if c.Done {
			t.Fatalf("an SPII stop must not end as a clean Done: %+v", c)
		}
	}
	if cerr := lastChunkError(chunks); !errors.Is(cerr, ErrContentFiltered) {
		t.Fatalf("want ErrContentFiltered, got %v", cerr)
	}
}

// TestParseOpenAISSE_NumericErrorCodeIsNotSwallowed: vLLM (a local provider and a Foundry serving stack) sends a
// failure after the 200 as {"error":{"object":"error","message":…,"type":…,"param":null,"code":<HTTP status>}}
// followed by [DONE]. The numeric code failed to decode into the string field, the event counted as undecodable,
// and [DONE] then closed the stream as a clean, empty completion: the error vanished.
func TestParseOpenAISSE_NumericErrorCodeIsNotSwallowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
		typ  string
		want error // nil: a plain (permanent) error
	}{
		{"bad request", "400", "BadRequestError", nil},
		{"rate limited", "429", "RateLimitError", ErrRateLimited},
		{"server error", "500", "InternalServerError", ErrProviderDown},
		{"unavailable", "503", "ServiceUnavailableError", ErrProviderDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"Qwen/Qwen3-8B","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"},"finish_reason":null}]}` + "\n\n" +
				`data: {"error": {"object": "error", "message": "upstream failed", "type": "` + tc.typ + `", "param": null, "code": ` + tc.code + `}}` + "\n\n" +
				"data: [DONE]\n\n"
			chunks, err := gatherOpenAIChunks(input)
			if err != nil {
				t.Fatalf("an error event ends the stream with nil, the error rides on its chunk: got %v", err)
			}
			for _, c := range chunks {
				if c.Done {
					t.Fatalf("the error was swallowed: the stream ended with a clean Done %+v", c)
				}
			}
			cerr := lastChunkError(chunks)
			if cerr == nil {
				t.Fatalf("no error chunk (chunks %+v)", chunks)
			}
			if tc.want != nil && !errors.Is(cerr, tc.want) {
				t.Errorf("want %v, got %v", tc.want, cerr)
			}
			if tc.want == nil && (errors.Is(cerr, ErrRateLimited) || errors.Is(cerr, ErrProviderDown)) {
				t.Errorf("a 400 is permanent, got %v", cerr)
			}
			if !strings.Contains(cerr.Error(), "upstream failed") {
				t.Errorf("the message should survive: %v", cerr)
			}
		})
	}
}

// Review findings on the drift detector (R-testing-10 follow-up).

// TestParseClaudeSSE_WhitespaceToolArgsKeepTheReplayRecord: a tool_use whose streamed input is only whitespace is
// sent as {} (assembleToolCalls), and the replay record must agree. It tested input == "" only, so json.Valid
// failed on " ", the whole ProviderItems was dropped, and the replay lost the thinking block before the tool_use
// (Anthropic rejects a tool-use turn replayed without its thinking).
func TestParseClaudeSSE_WhitespaceToolArgsKeepTheReplayRecord(t *testing.T) {
	input := sseEvent("message_start", `{"type":"message_start","message":{"model":"claude-opus-4-1","usage":{"input_tokens":30}}}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"List them."}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-1"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseEvent("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"list_findings","input":{}}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":" "}}`) +
		sseEvent("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\n"}}`) +
		sseEvent("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		sseEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`) +
		sseEvent("message_stop", `{"type":"message_stop"}`)
	var done *Chunk
	if err := parseClaudeSSE(strings.NewReader(input), func(c Chunk) {
		if c.Done {
			done = &c
		}
	}); err != nil {
		t.Fatal(err)
	}
	if done == nil || len(done.ToolCalls) != 1 || string(done.ToolCalls[0].Input) != "{}" {
		t.Fatalf("want one tool call with input {}, got %+v", done)
	}
	if done.ProviderItems == nil {
		t.Fatal("the replay record was dropped: the thinking block before the tool_use is lost")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(done.ProviderItems, &items); err != nil {
		t.Fatalf("record is not a block list: %v (%s)", err, done.ProviderItems)
	}
	if len(items) != 2 || string(items[0]["type"]) != `"thinking"` || string(items[1]["type"]) != `"tool_use"` || string(items[1]["input"]) != "{}" {
		t.Errorf("want [thinking, tool_use{input:{}}], got %s", done.ProviderItems)
	}
}

// openAIFinished is a complete chat-completions answer up to (not including) its usage chunk and [DONE].
const openAIFinished = `data: {"model":"gpt-4.1","choices":[{"index":0,"delta":{"content":"All done."},"finish_reason":null}]}` + "\n\n" +
	`data: {"model":"gpt-4.1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"

// assertCleanDone fails unless the stream ended with nil, exactly one Done (finish stop, wantIn input tokens) and no error chunk.
func assertCleanDone(t *testing.T, chunks []Chunk, err error, wantIn int) {
	t.Helper()
	if err != nil {
		t.Fatalf("a finished answer must not fail: %v", err)
	}
	if cerr := lastChunkError(chunks); cerr != nil {
		t.Fatalf("an error after the finish failed a complete answer: %v", cerr)
	}
	var dones int
	for _, c := range chunks {
		if c.Done {
			dones++
			if c.FinishReason != "stop" || c.TokensIn != wantIn {
				t.Errorf("Done: finish=%q in=%d, want stop/%d", c.FinishReason, c.TokensIn, wantIn)
			}
		}
	}
	if dones != 1 {
		t.Fatalf("want exactly one Done, got %d (%+v)", dones, chunks)
	}
}

// TestParseOpenAISSE_ErrorAfterFinishIsIgnored: like the Claude and Gemini parsers, nothing after the terminal
// finish_reason fails the answer: an error event before [DONE] is ignored, and the usage chunk after it still counts.
func TestParseOpenAISSE_ErrorAfterFinishIsIgnored(t *testing.T) {
	input := openAIFinished +
		`data: {"error":{"message":"The server had an error while processing your request.","type":"server_error","code":null}}` + "\n\n" +
		`data: {"model":"gpt-4.1","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	chunks, err := gatherOpenAIChunks(input)
	assertCleanDone(t, chunks, err, 12)
}

// TestParseOpenAISSE_ReadErrorAfterFinishIsDone: a connection reset after the finish_reason loses nothing.
func TestParseOpenAISSE_ReadErrorAfterFinishIsDone(t *testing.T) {
	var chunks []Chunk
	body := io.MultiReader(strings.NewReader(openAIFinished), iotest.ErrReader(errors.New("connection reset by peer")))
	err := parseOpenAISSE(body, func(c Chunk) { chunks = append(chunks, c) })
	assertCleanDone(t, chunks, err, 0)
}

// TestParseOpenAISSE_ErrorAfterContentFilterKeepsTheFilterVerdict: the filter stop is terminal too.
func TestParseOpenAISSE_ErrorAfterContentFilterKeepsTheFilterVerdict(t *testing.T) {
	input := `data: {"model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Here"},"finish_reason":null}]}` + "\n\n" +
		`data: {"model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n" +
		`data: {"error":{"message":"boom","type":"server_error"}}` + "\n\n"
	chunks, err := gatherOpenAIChunks(input)
	if err != nil {
		t.Fatal(err)
	}
	if cerr := lastChunkError(chunks); !errors.Is(cerr, ErrContentFiltered) || errors.Is(cerr, ErrProviderDown) {
		t.Fatalf("want the content-filter verdict, got %v", cerr)
	}
}

// TestParseOpenAISSE_NumericErrorCodeRoutesLikeTheHTTPStatus: a numeric code is the HTTP status, so it is classified
// as handleStreamError classifies that status: 401 the key, 403 access denied, a 400 context-length message
// ErrContextLimit (with the window it states), a 400 tools rejection ErrToolsUnsupported.
func TestParseOpenAISSE_NumericErrorCodeRoutesLikeTheHTTPStatus(t *testing.T) {
	for _, tc := range []struct {
		name, code, msg string
		want            error
	}{
		{"unauthorized", "401", "Invalid API key", ErrApiKeyInvalid},
		{"forbidden", "403", "This deployment is not available in your region", ErrProviderAccessDenied},
		{"context length", "400", "This model's maximum context length is 32768 tokens. However, you requested 40000 tokens.", ErrContextLimit},
		{"tools", "400", `"auto" tool choice requires --enable-auto-tool-choice and --tool-call-parser to be set`, ErrToolsUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg, _ := json.Marshal(tc.msg)
			input := `data: {"error": {"object": "error", "message": ` + string(msg) + `, "type": "BadRequestError", "param": null, "code": ` + tc.code + `}}` + "\n\n" +
				"data: [DONE]\n\n"
			chunks, err := gatherOpenAIChunks(input)
			if err != nil {
				t.Fatal(err)
			}
			cerr := lastChunkError(chunks)
			if !errors.Is(cerr, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, cerr)
			}
			var cl *ContextLimitError
			if errors.Is(tc.want, ErrContextLimit) && (!errors.As(cerr, &cl) || cl.Limit != 32768) {
				t.Errorf("want the stated window 32768, got %v", cerr)
			}
		})
	}
}
