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
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"

	"github.com/google/uuid"
)

const maxChatStreamDuration = 5 * time.Minute

type streamCtl struct {
	cancel  context.CancelFunc
	started chan struct{}
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
	activeStreams sync.Map // map[streamID]*streamCtl
}

func NewChatService(
	notifier Notifier,
	configDir string,
	flowCache *FlowService,
	analysisCache *AnalysisService,
	settings *storage.SettingsStore,
	factory *ai.ProviderFactory,
	demoLimiter *ai.DemoLimiter,
) *ChatService {
	return &ChatService{
		notifier:      notifier,
		configDir:     configDir,
		flowCache:     flowCache,
		analysisCache: analysisCache,
		settings:      settings,
		factory:       factory,
		demoLimiter:   demoLimiter,
	}
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
		defer s.activeStreams.Delete(streamID)
		defer cancel()

		provider, err := s.factory.For(scope, req.Provider)
		if err != nil {
			if !awaitStart() {
				return
			}
			emit("error", map[string]interface{}{"message": err.Error()})
			return
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

		// Prepare AI context
		ctxReq := ai.ContextRequest{
			Flow:               doc,
			Findings:           nil, // could pass report findings
			TokenBudget:        4000,
			Provider:           provider,
			SystemPromptSuffix: req.SystemPrompt,
		}
		if report != nil {
			ctxReq.Findings = report.Findings
		}
		if req.ContextBlockID != "" {
			ctxReq.SelectedBlock = s.flowCache.FindBlockByID(doc, req.ContextBlockID)
			ctxReq.SelectedSubflow = s.flowCache.FindSubflowForBlock(doc, req.ContextBlockID)
		}
		
		sysPrompt, contextText := ai.BuildContext(ctxReq)
		
		messages := make([]ai.Message, len(req.Messages))
		for i, m := range req.Messages {
			messages[i] = ai.Message{Role: m.Role, Content: m.Content}
		}
		
		// Add context and user message
		if contextText != "" {
			messages = append(messages, ai.Message{Role: "user", Content: "Context:\n" + contextText})
		}
		messages = append(messages, ai.Message{Role: "user", Content: req.UserMessage})

		aiReq := ai.Request{
			SystemPrompt: sysPrompt,
			Messages:     messages,
			Model:        req.Model,
			Temperature:  req.Temperature,
			MaxTokens:    req.MaxTokens,
		}

		err = provider.Stream(ctx, aiReq, func(chunk ai.Chunk) {
			if !awaitStart() {
				return
			}
			if chunk.Error != nil {
				emit("error", map[string]interface{}{"message": chunk.Error.Error()})
				return
			}
			if chunk.Done {
				emit("done", map[string]interface{}{
					"tokensIn":     chunk.TokensIn,
					"tokensOut":    chunk.TokensOut,
				})
				return
			}
			emit("chunk", map[string]interface{}{"content": chunk.Text})
		})
		
		if err != nil {
			if !awaitStart() {
				return
			}
			emit("error", map[string]interface{}{"message": err.Error()})
		}
	}()

	return streamID, nil
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

func (lsb *ChatService) GetConversation(doc *models.FlowDocument, provider string) ([]models.ChatMessage, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	path := filepath.Join(lsb.configDir, "conversations", provider, doc.ID+".json")
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
	path := filepath.Join(lsb.configDir, "conversations", provider, doc.ID+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
	}

	conv := models.ConversationFile{
		Version:   1,
		FlowKey:   doc.ID,
		Scope:     provider,
		UpdatedAt: time.Now(),
		Messages:  messages,
	}

	data, err := json.MarshalIndent(conv, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (lsb *ChatService) ClearConversation(doc *models.FlowDocument, provider string) error {
	if doc == nil {
		return fmt.Errorf("no flow loaded")
	}
	path := filepath.Join(lsb.configDir, "conversations", provider, doc.ID+".json")
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

	ctxReq := ai.ContextRequest{
		Flow:               doc,
		Findings:           nil,
		TokenBudget:        4000,
		Provider:           provider,
		SystemPromptSuffix: req.SystemPrompt,
	}
	if report != nil {
		ctxReq.Findings = report.Findings
	}
	if req.ContextBlockID != "" {
		ctxReq.SelectedBlock = s.flowCache.FindBlockByID(doc, req.ContextBlockID)
		ctxReq.SelectedSubflow = s.flowCache.FindSubflowForBlock(doc, req.ContextBlockID)
	}
	
	sysPrompt, contextText := ai.BuildContext(ctxReq)

	total := provider.EstimateTokens(sysPrompt + contextText + req.UserMessage)

	return &models.ContextPreview{
		SystemPrompt:    sysPrompt,
		ContextText:     contextText,
		UserMessage:     req.UserMessage,
		EstimatedTokens: total,
		ContextLimit:    provider.ContextLimit(),
	}, nil
}

func (s *ChatService) GetSuggestedPrompts(hasBlock, hasFindings bool) (prompts []string, err error) {
	defer logger.Guard("App.GetSuggestedPrompts", &err)

	if hasFindings && hasBlock {
		prompts = append(prompts, ai.SuggestedPromptsBlockWithFindings...)
	} else if hasFindings {
		prompts = append(prompts, ai.SuggestedPromptsFinding...)
	} else if hasBlock {
		prompts = append(prompts, ai.SuggestedPromptsBlock...)
	} else {
		prompts = append(prompts, ai.SuggestedPromptsFlow...)
	}

	return prompts, nil
}
