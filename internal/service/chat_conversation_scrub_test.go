package service

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	storagedb "pad-analyzer/internal/storage/database"
	tu "pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// The store holds the scrubbed copy of every message, the copy the provider sees: a question pasting a connection
// string or a client save carrying a Bearer token is masked before it reaches the row or the file, so a reload, a
// viewer holding the thread's key and the data-subject export all get the masked text.

const (
	secretQuestion = "it fails to connect with Password=hunter2secret from the desktop"
	secretValue    = "hunter2secret"
	bearerValue    = "abcdefghijklmnopqrstuvwxyz0123456789"
	bearerText     = "the header I send is Authorization: Bearer " + bearerValue + " and it is refused"
)

// assertMasked fails when content still carries secret, or carries no mask at all.
func assertMasked(t *testing.T, what, content, secret string) {
	t.Helper()
	if strings.Contains(content, secret) {
		t.Errorf("%s holds the secret in plain text: %q", what, content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("%s carries no mask: %q", what, content)
	}
}

// findMessage is the message with id in list, failing when it is not there.
func findMessage(t *testing.T, what string, list []models.ChatMessage, id string) models.ChatMessage {
	t.Helper()
	for _, m := range list {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("%s does not hold message %s: %+v", what, id, list)
	return models.ChatMessage{}
}

// normalizeConversation masks every message's content and leaves plain text alone; run again on its own output it
// changes nothing, which is what the writers' no-change detection (the mutation's changed flag, the answer dedupe)
// relies on.
func TestNormalizeConversation_StoresTheScrubbedCopyAndIsIdempotent(t *testing.T) {
	in := []models.ChatMessage{
		{ID: "q", Role: "user", Content: secretQuestion},
		{ID: "a", Role: "assistant", Content: bearerText},
		{ID: "p", Role: "user", Content: "a plain question about the loop"},
	}
	once := normalizeConversation("", in)
	if len(once) != 3 {
		t.Fatalf("normalised list has %d messages, want 3", len(once))
	}
	assertMasked(t, "the stored question", once[0].Content, secretValue)
	assertMasked(t, "the stored answer", once[1].Content, bearerValue)
	if once[2].Content != in[2].Content {
		t.Errorf("plain text changed: %q", once[2].Content)
	}
	if in[0].Content != secretQuestion {
		t.Errorf("the caller's list was written to: %q", in[0].Content)
	}
	twice := normalizeConversation("", once)
	if !reflect.DeepEqual(once, twice) {
		t.Errorf("a second normalisation changed the stored list:\n once: %+v\ntwice: %+v", once, twice)
	}
}

// fileChat is a chat service on the file store (local mode) answering with provider.
func fileChat(t *testing.T, provider ai.Provider) (*ChatService, string) {
	t.Helper()
	name := "cv-scrub-" + uuid.NewString()
	ai.RegisterProvider(name, func(string) ai.Provider { return provider })
	svc := &ChatService{
		notifier: &tu.CountingNotifier{}, configDir: t.TempDir(), flowCache: &FlowService{}, analysisCache: &AnalysisService{},
		factory: keyedFactory(), settings: nilSettings{}, chatCtxCache: newChatContextCache(), mode: config.ModeLocal,
	}
	t.Cleanup(svc.CancelAll)
	return svc, name
}

// A streamed turn whose question carries Password=... is stored masked in the file store: in the file itself, and
// in what a reload reads back.
func TestStreamedTurn_QuestionIsStoredMasked_FileStore(t *testing.T) {
	provider := fakeAnswer("the answer")
	svc, name := fileChat(t, provider)
	doc := &models.FlowDocument{ID: "flow-scrub", Name: "flow-scrub"}
	req := models.ChatRequest{
		Provider: name, Model: "fake-model", ConversationID: uuid.NewString(), UserMessage: secretQuestion,
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
	}
	ctx := context.Background()
	id, err := svc.StreamChatMessage(ctx, "u1", doc, nil, req)
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	if ctl := waitStreamFinished(t, svc, id); !ctl.done || ctl.errMsg != "" {
		t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
	}

	path, err := convFilePath(svc.configDir, req.ConversationKey(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatalf("read the conversation file: %v", err)
	}
	if strings.Contains(string(data), secretValue) {
		t.Errorf("the conversation file holds the secret in plain text:\n%s", data)
	}
	var conv models.ConversationFile
	if err := json.Unmarshal(data, &conv); err != nil {
		t.Fatal(err)
	}
	assertMasked(t, "the question in the file", findMessage(t, "the file", conv.Messages, req.UserMessageID).Content, secretValue)

	read, err := svc.ReadConversation(ctx, "u1", doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatalf("ReadConversation: %v", err)
	}
	assertMasked(t, "the question a reload gets", findMessage(t, "the reload", read.Messages, req.UserMessageID).Content, secretValue)

	// The provider saw the same masked question: the store now holds the provider's copy, not more and not less.
	reqs := provider.Requests()
	if len(reqs) == 0 {
		t.Fatal("the provider was not called")
	}
	last := reqs[0].Messages[len(reqs[0].Messages)-1]
	assertMasked(t, "the provider's user turn", last.Content, secretValue)
}

// forEachCloudStore runs fn against the fake backend and, with DATABASE_URL, a migrated Postgres database of its
// own (skipped without it).
func forEachCloudStore(t *testing.T, fn func(t *testing.T, b chatStore)) {
	t.Run("fake", func(t *testing.T) { fn(t, tu.NewFakeBackend()) })
	t.Run("postgres", func(t *testing.T) {
		pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
		if err != nil {
			t.Fatalf("open the test database: %v", err)
		}
		t.Cleanup(func() { _ = pg.Close() })
		fn(t, pg)
	})
}

// The same turn in cloud mode: the row holds the masked question, and so does a versioned read.
func TestStreamedTurn_QuestionIsStoredMasked_CloudStore(t *testing.T) {
	forEachCloudStore(t, func(t *testing.T, b chatStore) {
		h := newStreamHarness(t, b, fakeAnswer("the answer"))
		req, ctl := h.turn(t, secretQuestion)
		if !ctl.done || ctl.errMsg != "" {
			t.Fatalf("stream did not finish cleanly: done=%v err=%q", ctl.done, ctl.errMsg)
		}
		ctx := context.Background()
		stored, _, err := b.LoadConversationVersioned(ctx, h.doc.ID, h.key, "")
		if err != nil {
			t.Fatalf("LoadConversationVersioned: %v", err)
		}
		assertMasked(t, "the stored question", findMessage(t, "the store", toModelMessages(stored), req.UserMessageID).Content, secretValue)
		read, err := h.svc.ReadConversation(ctx, "u1", h.doc.ID, h.key)
		if err != nil {
			t.Fatalf("ReadConversation: %v", err)
		}
		assertMasked(t, "the question a reload gets", findMessage(t, "the reload", read.Messages, req.UserMessageID).Content, secretValue)
	})
}

// A client save (POST /api/chat/save) carrying a Bearer token stores it masked, through every path the save takes:
// the compare-and-swap with baseVersion, the blind save without it, and the file store.
func TestSaveClientConversation_StoresSecretsMasked(t *testing.T) {
	ctx := context.Background()
	doc := &models.FlowDocument{ID: "flow-scrub-save"}
	key := uuid.NewString()
	msgs := []models.ChatMessage{{ID: "m1", Role: "user", Content: bearerText}}
	zero := int64(0)
	cases := []struct {
		name string
		svc  func(t *testing.T) *ChatService
		base *int64
	}{
		{"cloud with baseVersion", func(*testing.T) *ChatService { return cloudChat(tu.NewFakeBackend()) }, &zero},
		{"cloud without baseVersion", func(*testing.T) *ChatService { return cloudChat(tu.NewFakeBackend()) }, nil},
		{"file store", func(t *testing.T) *ChatService { return &ChatService{configDir: t.TempDir()} }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := tc.svc(t)
			if _, err := svc.SaveClientConversation(ctx, doc.ID, key, tc.base, msgs, true); err != nil {
				t.Fatalf("SaveClientConversation: %v", err)
			}
			var stored []models.ChatMessage
			if svc.useBackendConversations() {
				raw, err := svc.backend.LoadConversation(ctx, doc.ID, key)
				if err != nil {
					t.Fatalf("LoadConversation: %v", err)
				}
				stored = toModelMessages(raw)
			} else {
				var err error
				if stored, err = svc.GetConversation(ctx, doc.ID, key); err != nil {
					t.Fatalf("GetConversation: %v", err)
				}
			}
			assertMasked(t, "the saved message", findMessage(t, "the store", stored, "m1").Content, bearerValue)
		})
	}
}

// yamlAnswer is an answer in a shape the stream scrubber lets through to the client whole and the text scrub masks
// (a `password:` value under a YAML key): the resume buffer the server persists holds it in plain text.
const yamlAnswer = "the connection block it needs is:\ndb:\n  user: sa\n  password: " + secretValue + "\n"

// The server's write of the finished answer stores the write's scrubbed copy of the resume buffer, so a shape the
// stream scrubber let through to the client is still masked in the store.
func TestPersistAssistantTurn_StoresTheScrubbedCopyOfTheBuffer(t *testing.T) {
	ctx := context.Background()
	svc := cloudChat(tu.NewFakeBackend())
	doc := &models.FlowDocument{ID: "flow-scrub-answer"}
	req := models.ChatRequest{ConversationID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	svc.persistAssistantTurn(ctx, doc, req, finishedAnswer(yamlAnswer), "fake-model", "")
	stored, err := svc.GetConversation(ctx, doc.ID, req.ConversationKey())
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	assertMasked(t, "the stored answer", findMessage(t, "the store", stored, req.AssistantMessageID).Content, secretValue)
}

// A client from before AssistantMessageID saves the answer it received under its own id before the server's write
// lands, and the server's write can tell the two apart by content alone. It compares the store's copy: an answer
// the write masks (the stream scrubber let it through, the text scrub does not) is recognised and stored once.
func TestPersistAssistantTurn_DedupesAgainstTheStoredCopy(t *testing.T) {
	ctx := context.Background()
	svc := cloudChat(tu.NewFakeBackend())
	doc := &models.FlowDocument{ID: "flow-scrub-dedupe"}
	convID := uuid.NewString()
	key := models.ChatRequest{ConversationID: convID}.ConversationKey()
	saved := []models.ChatMessage{
		{ID: uuid.NewString(), Role: "user", Content: "what does the connection block look like?"},
		{ID: uuid.NewString(), Role: "assistant", Content: yamlAnswer},
	}
	if _, err := svc.SaveClientConversation(ctx, doc.ID, key, nil, saved, true); err != nil {
		t.Fatalf("SaveClientConversation: %v", err)
	}
	svc.persistAssistantTurn(ctx, doc, models.ChatRequest{ConversationID: convID}, finishedAnswer(yamlAnswer), "fake-model", "")
	stored, err := svc.GetConversation(ctx, doc.ID, key)
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("the thread holds %d messages after the server's write, want 2 (the client's copy of the answer, once): %+v", len(stored), stored)
	}
	assertMasked(t, "the stored answer", stored[1].Content, secretValue)
}
