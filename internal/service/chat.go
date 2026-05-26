package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/analyzer"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"
	"github.com/google/uuid"
)

// streamCtl tracks the lifecycle of a single streaming response.
type streamCtl struct {
	cancel  context.CancelFunc
	started chan struct{}
	buf     []ai.Chunk
	mu      sync.Mutex
	live    bool
}

// aiOrDefault returns val if positive, otherwise def.
func aiOrDefault(val, def int) int {
	if val <= 0 {
		return def
	}
	return val
}

// ChatService owns active streams and all conversational AI operations.
type ChatService struct {
	ctx           context.Context
	notifier      Notifier
	configDir     string
	flow          *FlowService
	analysis      *AnalysisService
	settings      *storage.SettingsStore
	factory       *ai.ProviderFactory
	demoLimiter   *ai.DemoLimiter
	activeStreams sync.Map
}

func NewChatService(
	ctx context.Context,
	notifier Notifier,
	configDir string,
	flow *FlowService,
	analysis *AnalysisService,
	settings *storage.SettingsStore,
	factory *ai.ProviderFactory,
	demoLimiter *ai.DemoLimiter,
) *ChatService {
	return &ChatService{
		ctx:         ctx,
		notifier:    notifier,
		configDir:   configDir,
		flow:        flow,
		analysis:    analysis,
		settings:    settings,
		factory:     factory,
		demoLimiter: demoLimiter,
	}
}

func (s *ChatService) StreamChatMessage(req models.ChatRequest) (streamID string, err error) {
	defer logger.Guard("App.StreamChatMessage", &err)

	streamID = uuid.NewString()
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Minute)
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
	s.activeStreams.Store(streamID, ctl)

	emit := func(eventType string, data map[string]interface{}) {
		s.notifier.Emit("chat:event",
			map[string]interface{}{"streamId": streamID, "type": eventType, "data": data})
	}

	go func() {
		defer s.activeStreams.Delete(streamID)
		defer cancel()

		provider, err := s.factory.For(req.Provider)
		if err != nil {
			<-ctl.started
			emit("error", map[string]interface{}{"error": err.Error(), "code": "provider_unavailable"})
			return
		}

		var findings []models.Finding
		report := s.analysis.LastReport()
		if report != nil {
			findings = report.Findings
		}
		selectedBlock := s.flow.FindBlockByID(req.ContextBlockID)

		docSnapshot := s.flow.CurrentDoc()

		var rawSources map[string]string
		if docSnapshot != nil && len(req.SelectedSourceFiles) > 0 {
			rawSources, _ = s.flow.ReadSourceFiles(req.SelectedSourceFiles)
		}

		varEvents := buildVariableEvents(docSnapshot, selectedBlock)
		systemPromptSuffix := ""
		if s.settings != nil {
			systemPromptSuffix = s.settings.Get().AI.SystemPromptSuffix
		}

		sys, ctxText := ai.BuildContext(ai.ContextRequest{
			Flow:               docSnapshot,
			SelectedBlock:      selectedBlock,
			SelectedSubflow:    s.flow.FindSubflowForBlock(req.ContextBlockID),
			Findings:           findings,
			RawSourceFiles:     rawSources,
			VariableEvents:     varEvents,
			TokenBudget:        provider.ContextLimit() / 2,
			Provider:           provider,
			SystemPromptSuffix: systemPromptSuffix,
		})

		var messages []ai.Message
		for _, m := range req.Messages {
			messages = append(messages, ai.Message{Role: m.Role, Content: m.Content})
		}
		messages = append(messages, ai.Message{
			Role: "user", Content: ctxText + "\n\n---\n\n" + req.UserMessage,
		})

		<-ctl.started
		ctl.mu.Lock()
		for _, c := range ctl.buf {
			emit("chunk", map[string]interface{}{"text": c.Text})
		}
		ctl.buf = nil
		ctl.live = true
		ctl.mu.Unlock()

		var totalOut, totalIn int
		streamErr := provider.Stream(ctx, ai.Request{
			Model:        req.Model,
			Messages:     messages,
			SystemPrompt: sys,
			Temperature:  req.Temperature,
			MaxTokens:    aiOrDefault(req.MaxTokens, 4096),
		}, func(chunk ai.Chunk) {
			if chunk.Error != nil {
				emit("error", map[string]interface{}{"error": chunk.Error.Error()})
				return
			}
			totalOut += chunk.TokensOut
			totalIn += chunk.TokensIn
			ctl.mu.Lock()
			if ctl.live {
				ctl.mu.Unlock()
				if chunk.Text != "" {
					emit("chunk", map[string]interface{}{"text": chunk.Text})
				}
			} else {
				ctl.buf = append(ctl.buf, chunk)
				ctl.mu.Unlock()
			}
			if chunk.Done {
				emit("done", map[string]interface{}{"totalTokens": totalOut, "tokensIn": totalIn, "finishReason": "stop"})
			}
		})

		if streamErr != nil && !errors.Is(streamErr, context.Canceled) {
			emit("error", map[string]interface{}{"error": streamErr.Error()})
		}
	}()

	return streamID, nil
}

func (s *ChatService) BeginStream(streamID string) {
	if v, ok := s.activeStreams.Load(streamID); ok {
		if ctl, ok := v.(*streamCtl); ok {
			select {
			case <-ctl.started:
			default:
				close(ctl.started)
			}
		}
	}
}

func (s *ChatService) CancelStream(streamID string) {
	if v, ok := s.activeStreams.Load(streamID); ok {
		if ctl, ok := v.(*streamCtl); ok {
			ctl.cancel()
		}
	}
}

func (s *ChatService) GetConversation(flowID string, blockId string) (conv *models.ConversationFile, err error) {
	defer logger.Guard("App.GetConversation", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return &models.ConversationFile{Messages: []models.ChatMessage{}}, nil
	}

	scope := blockId
	if scope == "" {
		scope = "flow"
	}

	return ai.LoadConversation(s.configDir, curDoc.FilePath, scope)
}

func (s *ChatService) SaveConversation(flowID string, blockId string, messages []models.ChatMessage) (err error) {
	defer logger.Guard("App.SaveConversation", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return fmt.Errorf("no flow loaded")
	}

	scope := blockId
	if scope == "" {
		scope = "flow"
	}

	return ai.SaveConversation(s.configDir, curDoc.FilePath, scope, messages)
}

func (s *ChatService) ClearConversation(flowID string, blockId string) (err error) {
	defer logger.Guard("App.ClearConversation", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return nil
	}

	scope := blockId
	if scope == "" {
		scope = "flow"
	}

	return ai.SaveConversation(s.configDir, curDoc.FilePath, scope, []models.ChatMessage{})
}

func (s *ChatService) ExportConversation(flowID string, blockId string, path string) (err error) {
	defer logger.Guard("App.ExportConversation", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return fmt.Errorf("no flow loaded")
	}

	scope := blockId
	if scope == "" {
		scope = "flow"
	}

	conv, loadErr := ai.LoadConversation(s.configDir, curDoc.FilePath, scope)
	if loadErr != nil {
		return fmt.Errorf("load conversation: %w", loadErr)
	}
	if len(conv.Messages) == 0 {
		return fmt.Errorf("no messages to export")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Chat Export — %s\n\n", curDoc.Name)
	if blockId != "" {
		fmt.Fprintf(&b, "**Block:** %s\n\n", blockId)
	}
	fmt.Fprintf(&b, "**Exported:** %s\n\n---\n\n", time.Now().Format("2006-01-02 15:04"))

	for _, m := range conv.Messages {
		role := "User"
		if m.Role == "assistant" {
			role = "AI"
		} else if m.Role == "system" {
			role = "System"
		}
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", role, m.Content)
	}

	if err = os.WriteFile(path, []byte(b.String()), 0644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func (s *ChatService) GetDemoRemaining() (remaining int, err error) {
	defer logger.Guard("App.GetDemoRemaining", &err)
	return s.demoLimiter.Remaining()
}

func (s *ChatService) PreviewContext(req models.ChatRequest) (result *models.ContextPreview, err error) {
	defer logger.Guard("App.PreviewContext", &err)

	provider, provErr := s.factory.For(req.Provider)
	if provErr != nil {
		return nil, fmt.Errorf("get provider: %w", provErr)
	}

	var findings []models.Finding
	report := s.analysis.LastReport()
	if report != nil {
		findings = report.Findings
	}

	selectedBlock := s.flow.FindBlockByID(req.ContextBlockID)

	docSnapshot := s.flow.CurrentDoc()

	var rawSources map[string]string
	if docSnapshot != nil && len(req.SelectedSourceFiles) > 0 {
		rawSources, _ = s.flow.ReadSourceFiles(req.SelectedSourceFiles)
	}

	varEvents := buildVariableEvents(docSnapshot, selectedBlock)
	systemPromptSuffix := ""
	if s.settings != nil {
		systemPromptSuffix = s.settings.Get().AI.SystemPromptSuffix
	}

	sys, ctxText := ai.BuildContext(ai.ContextRequest{
		Flow:               docSnapshot,
		SelectedBlock:      selectedBlock,
		SelectedSubflow:    s.flow.FindSubflowForBlock(req.ContextBlockID),
		Findings:           findings,
		RawSourceFiles:     rawSources,
		VariableEvents:     varEvents,
		TokenBudget:        provider.ContextLimit() / 2,
		Provider:           provider,
		SystemPromptSuffix: systemPromptSuffix,
	})

	userContent := ctxText + "\n\n---\n\n" + req.UserMessage
	estimatedTokens := provider.EstimateTokens(sys) + provider.EstimateTokens(userContent)
	for _, m := range req.Messages {
		estimatedTokens += provider.EstimateTokens(m.Content)
	}

	return &models.ContextPreview{
		SystemPrompt:    sys,
		ContextText:     ctxText,
		UserMessage:     req.UserMessage,
		EstimatedTokens: estimatedTokens,
		ContextLimit:    provider.ContextLimit(),
	}, nil
}

func (s *ChatService) GetSuggestedPrompts(hasBlock bool, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)

	if hasBlock && hasFindings {
		return ai.SuggestedPromptsBlockWithFindings, nil
	}
	if hasBlock {
		return ai.SuggestedPromptsBlock, nil
	}
	return ai.SuggestedPromptsFlow, nil
}

// buildVariableEvents returns variable event histories for the variables
// referenced by the selected block. Returns nil when no block or doc.
func buildVariableEvents(doc *models.FlowDocument, block *models.Block) map[string][]models.VariableEvent {
	if doc == nil || block == nil || len(block.Variables) == 0 {
		return nil
	}
	events := make(map[string][]models.VariableEvent, len(block.Variables))
	for _, varName := range block.Variables {
		h := analyzer.BuildVariableLineage(doc, varName)
		if h != nil && len(h.Events) > 0 {
			events[varName] = h.Events
		}
	}
	if len(events) == 0 {
		return nil
	}
	return events
}
