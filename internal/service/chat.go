package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/ai/scrubber"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"

	"github.com/google/uuid"
)

const maxChatStreamDuration = 5 * time.Minute

// contextReserve is the number of tokens kept free for the prompt/context when
// clamping a caller-supplied MaxTokens against the model's context window.
const contextReserve = 4000

// normalizeChatParams clamps caller-supplied generation params into safe ranges
// so an out-of-range value can't reach the provider (some reject Temperature
// outside [0,2] or a MaxTokens above the model's limits with a 400). It is pure
// so it can be unit-tested independently of provider wiring.
//
// maxTokens is an *output* cap, so it must be bounded by the model's output
// ceiling (maxOutput) — not the input context window. We still keep the older
// ctxLimit-contextReserve guard as a backstop for models whose output ceiling
// is unknown (maxOutput == 0). A value of 0 for either limit means "unknown" and
// that particular clamp is skipped.
func normalizeChatParams(temperature float64, maxTokens, ctxLimit, maxOutput int) (float64, int) {
	if temperature < 0 {
		temperature = 0
	} else if temperature > 2 {
		temperature = 2
	}
	if maxTokens < 0 {
		maxTokens = 0
	}
	// Prefer the model's real output ceiling when known.
	if maxOutput > 0 && maxTokens > maxOutput {
		maxTokens = maxOutput
	}
	// Backstop: never let MaxTokens approach the input window (only meaningful
	// when the output ceiling is unknown, but harmless otherwise).
	if ctxLimit > 0 {
		cap := ctxLimit - contextReserve
		if cap < 0 {
			cap = 0
		}
		if maxTokens > cap {
			maxTokens = cap
		}
	}
	return temperature, maxTokens
}

// dailyUsage returns today's AI spend used for the budget check. The storage
// backend is nil in local/desktop mode (which has no usage store), so this
// returns 0 there — leaving the budget effectively unenforced rather than
// dereferencing a nil backend and panicking. Mirrors the nil-backend guard
// convention used across SystemService and the API handlers.
func (s *ChatService) dailyUsage(ctx context.Context, scope, orgID string) float64 {
	if s.backend == nil {
		return 0
	}
	usage, _ := s.backend.GetDailyUsage(ctx, scope, orgID)
	return usage
}

// resumeRetention is how long a finished stream's buffer is kept so a client
// that was disconnected when the stream ended can still fetch the final text
// (and done/error) on reconnect via ResumeStream.
const resumeRetention = 2 * time.Minute

type streamCtl struct {
	cancel    context.CancelFunc
	started   chan struct{}
	mu        sync.Mutex
	buffer    strings.Builder
	done      bool   // stream finished successfully
	errMsg    string // non-empty if the stream ended with an error
	tokensIn  int
	tokensOut int
}

// ResumeResult is the buffered state of a stream returned to a reconnecting client.
type ResumeResult struct {
	Text      string `json:"text"`
	Done      bool   `json:"done"`
	Error     string `json:"error"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
}

// ChatService owns chat stream state and operations.
type ChatService struct {
	notifier      Notifier
	configDir     string
	flowCache     *FlowService
	analysisCache *AnalysisService
	settings      *storage.SettingsStore
	factory       *ai.ProviderFactory
	demoLimiter   *ai.DemoLimiter
	backend       storageif.StorageBackend
	knowledge     *rag.KnowledgeService
	activeStreams sync.Map // map[streamID]*streamCtl — in-flight streams
	// finishedStreams holds recently-completed streams for a short grace period
	// (resumeRetention) so a client that reconnects after a stream ended can
	// still fetch its final buffer via ResumeStream.
	finishedStreams sync.Map // map[streamID]*streamCtl
}

func NewChatService(
	notifier Notifier,
	configDir string,
	flowCache *FlowService,
	analysisCache *AnalysisService,
	settings *storage.SettingsStore,
	factory *ai.ProviderFactory,
	demoLimiter *ai.DemoLimiter,
	backend storageif.StorageBackend,
) *ChatService {
	return &ChatService{
		notifier:      notifier,
		configDir:     configDir,
		flowCache:     flowCache,
		analysisCache: analysisCache,
		settings:      settings,
		factory:       factory,
		demoLimiter:   demoLimiter,
		backend:       backend,
	}
}

func (s *ChatService) SetKnowledgeService(ks *rag.KnowledgeService) {
	s.knowledge = ks
}

func (s *ChatService) GetAuthorizedFlow(ctx context.Context, flowID, userID, minPerm string) (*models.FlowDocument, error) {
	return s.flowCache.GetAuthorized(ctx, flowID, userID, minPerm)
}

func (s *ChatService) StreamChatMessage(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)

	streamID = uuid.NewString()
	ctx, cancel := context.WithTimeout(ctx, maxChatStreamDuration)
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{}, 1)}
	s.activeStreams.Store(streamID, ctl)

	emit := func(eventType string, data map[string]interface{}) {
		s.notifier.Emit("chat:event",
			map[string]interface{}{"streamId": streamID, "type": eventType, "data": data})
	}

	awaitStart := func() bool {
		select {
		case <-ctl.started:
			return true
		case <-ctx.Done():
			return false
		}
	}

	go func() {
		// On exit, remove the stream from the active set (so cancel/begin and
		// leak-detection see it as gone) and move it to the finished set for a
		// short grace period, so a client that reconnects after completion can
		// still fetch the final buffer via ResumeStream instead of hanging.
		defer func() {
			cancel()
			s.activeStreams.Delete(streamID)
			s.finishedStreams.Store(streamID, ctl)
			time.AfterFunc(resumeRetention, func() { s.finishedStreams.Delete(streamID) })
		}()

		provider, err := s.factory.For(scope, req.Provider)
		if err != nil {
			if !awaitStart() {
				return
			}
			emit("error", map[string]interface{}{"message": err.Error()})
			return
		}

		// Org-scoped flows attribute usage and enforce the daily budget at the
		// org level; personal flows fall back to the per-user total.
		orgID := ""
		if doc != nil {
			orgID = doc.OrganizationID
		}

		// Enforce daily budget.
		settings := s.settings.Get()
		if settings != nil && settings.AI.DailyBudget > 0 {
			if usage := s.dailyUsage(ctx, scope, orgID); usage >= settings.AI.DailyBudget {
				if !awaitStart() {
					return
				}
				emit("error", map[string]interface{}{"message": fmt.Sprintf("daily AI budget exceeded ($%.2f / $%.2f)", usage, settings.AI.DailyBudget)})
				return
			}
		}

		if req.Provider == "demo" && s.demoLimiter != nil {
			if _, err := s.demoLimiter.ReserveForDisplay(); err != nil {
				if !awaitStart() {
					return
				}
				emit("error", map[string]interface{}{"message": err.Error()})
				return
			}
		}

		// Scrub the document before preparing context
		scrubbedDoc, err := scrubber.ScrubDocument(doc)
		if err != nil {
			logger.Error("Failed to scrub document", map[string]interface{}{"error": err})
			scrubbedDoc = doc // fallback to unscrubbed if copy fails, though regex will catch later
		}

		// Prepare AI context
		ctxReq := ai.ContextRequest{
			Flow:               scrubbedDoc,
			Findings:           nil, // could pass report findings
			TokenBudget:        4000,
			Provider:           provider,
			SystemPromptSuffix: req.SystemPrompt,
		}
		if report != nil {
			ctxReq.Findings = report.Findings
		}
		if req.ContextBlockID != "" && scrubbedDoc != nil && scrubbedDoc.BlocksByID != nil {
			ctxReq.SelectedBlock = scrubbedDoc.BlocksByID[req.ContextBlockID]
			ctxReq.SelectedSubflow = scrubbedDoc.BlockSubflow[req.ContextBlockID]
		}
		
		sysPrompt, contextText := ai.BuildContext(ctxReq)

		// RAG: Add relevant organizational guidelines
		if s.knowledge != nil && doc.OrganizationID != "" {
			guidelines, err := s.knowledge.Search(ctx, scope, doc.OrganizationID, req.UserMessage)
			if err == nil && guidelines != "" {
				sysPrompt += "\n\n" + guidelines
			}
		}

		sysPrompt = scrubber.ScrubText(sysPrompt)
		contextText = scrubber.ScrubText(contextText)

		messages := make([]ai.Message, len(req.Messages))
		for i, m := range req.Messages {
			messages[i] = ai.Message{Role: m.Role, Content: scrubber.ScrubText(m.Content)}
		}
		
		// Add context and user message
		if contextText != "" {
			messages = append(messages, ai.Message{Role: "user", Content: "Context:\n" + contextText})
		}
		messages = append(messages, ai.Message{Role: "user", Content: scrubber.ScrubText(req.UserMessage)})

		temperature, maxTokens := normalizeChatParams(req.Temperature, req.MaxTokens, provider.ContextLimit(), ai.ModelMaxOutputTokens(provider, req.Model))
		aiReq := ai.Request{
			SystemPrompt: sysPrompt,
			Messages:     messages,
			Model:        req.Model,
			Temperature:  temperature,
			MaxTokens:    maxTokens,
			OrgID:        orgID,
		}

		// When the caller opted into tools and the provider supports them, run the
		// read-only agentic tool loop (streamed turns + tool status events). It is
		// fully self-contained — emits chunk/tool/done/error and updates ctl — so the
		// normal streaming path below is skipped entirely (zero regression when off).
		if req.UseTools && provider.SupportsTools() {
			s.runToolLoop(ctx, provider, aiReq, scrubbedDoc, ctl, awaitStart, emit)
			return
		}

		err = provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			if !awaitStart() {
				return
			}
			if chunk.Error != nil {
				ctl.mu.Lock()
				ctl.errMsg = chunk.Error.Error()
				ctl.mu.Unlock()
				emit("error", map[string]interface{}{"message": chunk.Error.Error()})
				return
			}
			if chunk.Done {
				ctl.mu.Lock()
				ctl.done = true
				ctl.tokensIn = chunk.TokensIn
				ctl.tokensOut = chunk.TokensOut
				ctl.mu.Unlock()
				emit("done", map[string]interface{}{
					"tokensIn":  chunk.TokensIn,
					"tokensOut": chunk.TokensOut,
				})
				return
			}

			ctl.mu.Lock()
			ctl.buffer.WriteString(chunk.Text)
			ctl.mu.Unlock()

			emit("chunk", map[string]interface{}{"content": chunk.Text})
		})

		if err != nil {
			ctl.mu.Lock()
			ctl.errMsg = err.Error()
			ctl.mu.Unlock()
			if !awaitStart() {
				return
			}
			emit("error", map[string]interface{}{"message": err.Error()})
		}
	}()

	return streamID, nil
}

// maxToolIterations bounds the read-only agentic loop so a misbehaving model
// that keeps requesting tools can't run forever.
const maxToolIterations = 6

// runToolLoop drives the read-only tool/agent loop: it offers the registered
// tools to the model, executes any tool calls against the (already scrubbed)
// doc + analysis cache, feeds results back, and repeats until the model returns
// a final text answer (or the iteration cap is hit). It is self-contained —
// emits "tool"/"chunk"/"done"/"error" events and updates ctl — so the caller
// does not run its own streaming/error handling for this path.
//
// Token consumption of ctl.started is gated through a single cached
// ensureStarted so it is consumed at most once (the channel only holds one
// token), regardless of how many turns/events the loop produces.
func (s *ChatService) runToolLoop(
	ctx context.Context,
	provider ai.Provider,
	base ai.Request,
	doc *models.FlowDocument,
	ctl *streamCtl,
	awaitStart func() bool,
	emit func(string, map[string]interface{}),
) {
	base.Tools = ai.ToolDefinitions()
	msgs := base.Messages
	tctx := ai.ToolContext{Ctx: ctx, Doc: doc, Analysis: s.analysisCache}

	started := false
	ensureStarted := func() bool {
		if !started && awaitStart() {
			started = true
		}
		return started
	}
	fail := func(msg string) {
		ctl.mu.Lock()
		ctl.errMsg = msg
		ctl.mu.Unlock()
		if ensureStarted() {
			emit("error", map[string]interface{}{"message": msg})
		}
	}

	var totalIn, totalOut int
	for i := 0; i < maxToolIterations; i++ {
		turn := base
		turn.Messages = msgs

		// Stream the turn: forward assistant text to the client as it arrives, and
		// capture the tool calls / token usage from the terminal (Done) chunk.
		var (
			turnText  strings.Builder
			turnCalls []ai.ToolCall
			turnIn    int
			turnOut   int
			chunkErr  error
		)
		streamErr := provider.Stream(ctx, turn, func(c ai.Chunk) {
			switch {
			case c.Error != nil:
				chunkErr = c.Error
			case c.Done:
				turnIn = c.TokensIn
				turnOut = c.TokensOut
				turnCalls = c.ToolCalls
			case c.Text != "":
				if !ensureStarted() {
					return
				}
				turnText.WriteString(c.Text)
				ctl.mu.Lock()
				ctl.buffer.WriteString(c.Text)
				ctl.mu.Unlock()
				emit("chunk", map[string]interface{}{"content": c.Text})
			}
		})
		if streamErr != nil {
			fail(streamErr.Error())
			return
		}
		if chunkErr != nil {
			fail(chunkErr.Error())
			return
		}
		totalIn += turnIn
		totalOut += turnOut

		// No tool calls → this turn is the final answer (its text already
		// streamed above, so there's nothing more to emit but the done event).
		if len(turnCalls) == 0 {
			if !ensureStarted() {
				return
			}
			ctl.mu.Lock()
			ctl.done = true
			ctl.tokensIn = totalIn
			ctl.tokensOut = totalOut
			ctl.mu.Unlock()
			emit("done", map[string]interface{}{"tokensIn": totalIn, "tokensOut": totalOut})
			return
		}

		// Record the assistant turn that issued the calls (including any streamed
		// preamble text), then run each tool and append its result so the next
		// turn can see them.
		msgs = append(msgs, ai.Message{Role: "assistant", Content: turnText.String(), ToolCalls: turnCalls})
		for _, tc := range turnCalls {
			if !ensureStarted() {
				return
			}
			emit("tool", map[string]interface{}{"name": tc.Name, "label": ai.ToolLabel(tc.Name)})
			result := ai.ExecuteTool(tc.Name, tc.Input, tctx)
			msgs = append(msgs, ai.Message{Role: "tool", Content: result, ToolCallID: tc.ID})
		}
	}

	fail(fmt.Sprintf("tool loop exceeded %d iterations without a final answer", maxToolIterations))
}

func (s *ChatService) BeginStream(streamID string) {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		select {
		case ctl.started <- struct{}{}:
		default:
		}
	}
}

func (s *ChatService) CancelStream(streamID string) {
	if val, ok := s.activeStreams.Load(streamID); ok {
		ctl := val.(*streamCtl)
		ctl.cancel()
	}
}

func (s *ChatService) CancelAll() {
	s.activeStreams.Range(func(key, value interface{}) bool {
		ctl := value.(*streamCtl)
		ctl.cancel()
		return true
	})
}

func (s *ChatService) ResumeStream(streamID string) (*ResumeResult, error) {
	val, ok := s.activeStreams.Load(streamID)
	if !ok {
		val, ok = s.finishedStreams.Load(streamID)
	}
	if !ok {
		return nil, fmt.Errorf("stream not found or already completed")
	}
	ctl := val.(*streamCtl)
	ctl.mu.Lock()
	defer ctl.mu.Unlock()
	return &ResumeResult{
		Text:      ctl.buffer.String(),
		Done:      ctl.done,
		Error:     ctl.errMsg,
		TokensIn:  ctl.tokensIn,
		TokensOut: ctl.tokensOut,
	}, nil
}

func (lsb *ChatService) GetConversation(doc *models.FlowDocument, provider string) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []models.ChatMessage{}, nil
		}
		return nil, fmt.Errorf("failed to read conversation file: %w", err)
	}

	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}
	return conv.Messages, nil
}

func (lsb *ChatService) SaveConversation(doc *models.FlowDocument, provider string, messages []models.ChatMessage) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
	}

	conv := models.ConversationFile{
		Version:   1,
		FlowKey:   doc.ID,
		Scope:     provider,
		UpdatedAt: time.Now(),
		Messages:  evictConvMessages(messages),
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	// Atomic write at 0600: an interrupted write leaves the prior conversation
	// intact rather than truncating it, and the file isn't world-readable.
	return atomicWriteConv(dir, path, data)
}

func (lsb *ChatService) ClearConversation(doc *models.FlowDocument, provider string) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	path, err := convFilePath(lsb.configDir, provider, doc.ID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete conversation file: %w", err)
	}
	return nil
}

func (lsb *ChatService) ExportConversation(doc *models.FlowDocument, provider string, path string) error {
	msgs, err := lsb.GetConversation(doc, provider)
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&b, "### %s (%s)\n\n%s\n\n", m.Role, m.Timestamp.Format(time.RFC3339), m.Content)
	}
	return os.WriteFile(path, []byte(b.String()), 0644)
}

func (s *ChatService) GetDemoRemaining() (int, error) {
	if s.demoLimiter == nil {
		return 0, nil
	}
	rem, _ := s.demoLimiter.Remaining()
	return rem, nil
}

func (s *ChatService) PreviewContext(ctx context.Context, scope string, doc *models.FlowDocument, report *models.AnalysisReport, req models.ChatRequest) (res *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)

	provider, err := s.factory.For(scope, req.Provider)
	if err != nil {
		return nil, err
	}

	scrubbedDoc, err := scrubber.ScrubDocument(doc)
	if err != nil {
		logger.Error("Failed to scrub document for preview", map[string]interface{}{"error": err})
		scrubbedDoc = doc
	}

	ctxReq := ai.ContextRequest{
		Flow:               scrubbedDoc,
		Findings:           nil,
		TokenBudget:        4000,
		Provider:           provider,
		SystemPromptSuffix: req.SystemPrompt,
	}
	if report != nil {
		ctxReq.Findings = report.Findings
	}
	if req.ContextBlockID != "" && scrubbedDoc != nil && scrubbedDoc.BlocksByID != nil {
		ctxReq.SelectedBlock = scrubbedDoc.BlocksByID[req.ContextBlockID]
		ctxReq.SelectedSubflow = scrubbedDoc.BlockSubflow[req.ContextBlockID]
	}
	
	sysPrompt, contextText := ai.BuildContext(ctxReq)

	// RAG: Include relevant guidelines in preview
	if s.knowledge != nil && doc.OrganizationID != "" {
		guidelines, err := s.knowledge.Search(ctx, scope, doc.OrganizationID, req.UserMessage)
		if err == nil && guidelines != "" {
			sysPrompt += "\n\n" + guidelines
		}
	}

	sysPrompt = scrubber.ScrubText(sysPrompt)
	contextText = scrubber.ScrubText(contextText)
	scrubbedUserMsg := scrubber.ScrubText(req.UserMessage)

	total := provider.EstimateTokens(sysPrompt + contextText + scrubbedUserMsg)

	return &models.ContextPreview{
		SystemPrompt:    sysPrompt,
		ContextText:     contextText,
		UserMessage:     scrubbedUserMsg,
		EstimatedTokens: total,
		ContextLimit:    provider.ContextLimit(),
	}, nil
}

func (s *ChatService) GetSuggestedPrompts(hasBlock, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)

	settings := s.settings.Get()
	if settings == nil {
		return nil, fmt.Errorf("settings not found")
	}

	p := settings.AI.Prompts

	if hasBlock && hasFindings {
		prompts = append(prompts, p.BlockWithFindings...)
	} else if hasFindings {
		prompts = append(prompts, p.Finding...)
	} else if hasBlock {
		prompts = append(prompts, p.Block...)
	} else {
		prompts = append(prompts, p.Flow...)
	}

	return prompts, nil
}
