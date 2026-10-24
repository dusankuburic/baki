package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/rag"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/ai/scrubber"
	"pad-core/models"
)

// turnReadsBackend is the cloud backend of one org flow: u1 is a member of
// org-A, with a settings row. It counts the per-turn reads the turn memo
// collapses.
type turnReadsBackend struct {
	*testutil.FakeBackend
	settingsReads atomic.Int32
	orgRoleReads  atomic.Int32
}

func (b *turnReadsBackend) LoadUserSettings(_ context.Context, _ string) (*storageif.AppSettings, error) {
	b.settingsReads.Add(1)
	return &storageif.AppSettings{AI: storageif.AISettings{SaveConversationHistory: true, SystemPromptSuffix: "Be brief."}}, nil
}

func (b *turnReadsBackend) OrgMemberRole(_ context.Context, orgID, userID string) (auth.Role, bool, error) {
	b.orgRoleReads.Add(1)
	if orgID != "org-A" {
		return "", false, storageif.ErrNotFound
	}
	if userID != "u1" {
		return "", false, nil
	}
	return auth.RoleMember, true, nil
}

// One chat turn read the caller's settings row twice before the provider
// was dialled (the start-of-stream persist's history toggle, the custom
// instructions) and listed every org of the caller twice (the credential
// org, the knowledge-base membership gate). Each is now read once per turn;
// the answer's persist at the end still reads the settings afresh.
func TestStreamTurn_ReadsSettingsAndOrgRoleOncePerTurn(t *testing.T) {
	registerFakeProvider()
	backend := &turnReadsBackend{FakeBackend: testutil.NewFakeBackend()}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:      &testutil.CountingNotifier{},
		configDir:     t.TempDir(),
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		backend:       backend,
		mode:          config.ModeCloud,
		chatCtxCache:  newChatContextCache(),
		knowledge:     rag.NewKnowledgeService(nil, factory, nil),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1", OrganizationID: "org-A"}

	id, err := svc.StreamChatMessage(asUser("u1"), "u1", doc, nil, models.ChatRequest{
		Provider: "fake", Model: "fake-model", UserMessage: "hello", ClientStreamID: "12345678-1234-5678-1234-5678123456aa",
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	ctl := waitStreamFinished(t, svc, id)
	if snap := ctl.snapshot(); !snap.Done {
		t.Fatalf("turn did not finish: %+v", snap)
	}
	if got, _ := backend.LoadConversation(context.Background(), "f1", "flow"); len(got) != 2 {
		t.Errorf("stored %d messages, want the question and the answer", len(got))
	}
	// Start-of-turn read (shared by the persist and the instructions) plus
	// the end-of-turn persist's own fresh read.
	if n := backend.settingsReads.Load(); n != 2 {
		t.Errorf("LoadUserSettings ran %d times in one turn, want 2", n)
	}
	if n := backend.orgRoleReads.Load(); n != 1 {
		t.Errorf("OrgMemberRole ran %d times in one turn, want 1", n)
	}
}

// orgStoreBackend is a chat backend counting membership reads and full
// listings of the caller's organisations; readErr fails the membership reads.
type orgStoreBackend struct {
	*testutil.FakeBackend
	readErr  error
	reads    atomic.Int32
	listings atomic.Int32
}

func (b *orgStoreBackend) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	b.reads.Add(1)
	if b.readErr != nil {
		return "", false, b.readErr
	}
	return b.FakeBackend.OrgMemberRole(ctx, orgID, userID)
}
func (b *orgStoreBackend) ListOrgsForUser(ctx context.Context, userID string) ([]*storageif.Organisation, error) {
	b.listings.Add(1)
	return b.FakeBackend.ListOrgsForUser(ctx, userID)
}

// The caller's role in ONE org is that org's membership row, not a listing of
// every org the caller belongs to; an unknown org or a non-member is "not a
// member", and a failed read is an error.
func TestLookupOrgRole_SingleOrgLookup(t *testing.T) {
	backend := &orgStoreBackend{FakeBackend: testutil.NewFakeBackend()}
	if err := backend.SaveOrg(context.Background(), &storageif.Organisation{ID: "org-1", Name: "Contoso", OwnerID: "u-admin",
		Members: []storageif.OrgMember{{UserID: "u-admin", Role: auth.RoleAdmin}}}); err != nil {
		t.Fatal(err)
	}
	svc := &ChatService{backend: backend}

	role, ok, err := svc.orgRoleCached(context.Background(), "org-1", "u-admin")
	if err != nil || !ok || role != auth.RoleAdmin {
		t.Fatalf("admin: %q %v %v", role, ok, err)
	}
	if _, ok, err := svc.orgRoleCached(context.Background(), "org-1", "stranger"); ok || err != nil {
		t.Errorf("non-member: ok=%v err=%v, want not a member", ok, err)
	}
	if _, ok, err := svc.orgRoleCached(context.Background(), "no-such-org", "u-admin"); ok || err != nil {
		t.Errorf("unknown org: ok=%v err=%v, want not a member", ok, err)
	}
	if n := backend.listings.Load(); n != 0 {
		t.Errorf("ListOrgsForUser ran %d times, want 0 (single-org lookups)", n)
	}
	if n := backend.reads.Load(); n != 3 {
		t.Errorf("OrgMemberRole ran %d times, want 3", n)
	}

	backend.readErr = errors.New("db down")
	if _, _, err := svc.orgRoleCached(context.Background(), "org-1", "u-admin"); err == nil {
		t.Error("a failed membership read must be an error, not \"not a member\"")
	}
	if svc.isOrgMember(context.Background(), "org-1", "u-admin") {
		t.Error("a failed membership read must fail the membership gate closed")
	}
}

// Within one turn the role is read once whoever asks (credential org,
// registry credential org, membership gate); a failed read is not memoised.
func TestOrgRoleCached_OncePerTurn(t *testing.T) {
	backend := &turnReadsBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := &ChatService{backend: backend}
	ctx := withTurnMemo(context.Background())

	if got := svc.credentialOrg(ctx, "org-A", "u1"); got != "org-A" {
		t.Fatalf("credentialOrg = %q", got)
	}
	if got, err := svc.registryCredentialOrg(ctx, "org-A", "u1"); got != "org-A" || err != nil {
		t.Fatalf("registryCredentialOrg = %q, %v", got, err)
	}
	if !svc.isOrgMember(ctx, "org-A", "u1") {
		t.Fatal("member gate refused a member")
	}
	if n := backend.orgRoleReads.Load(); n != 1 {
		t.Errorf("org store read %d times in one turn, want 1", n)
	}
	// Another turn reads again.
	svc.isOrgMember(withTurnMemo(context.Background()), "org-A", "u1")
	if n := backend.orgRoleReads.Load(); n != 2 {
		t.Errorf("a new turn reused the previous turn's role (reads=%d, want 2)", n)
	}

	failing := &ChatService{backend: &roleBackend{readErr: errors.New("db down")}}
	fctx := withTurnMemo(context.Background())
	if _, _, err := failing.orgRoleCached(fctx, "org-A", "u-admin"); err == nil {
		t.Fatal("want the store error")
	}
	failing.backend = &roleBackend{}
	if role, ok, err := failing.orgRoleCached(fctx, "org-A", "u-admin"); err != nil || !ok || role != auth.RoleAdmin {
		t.Errorf("after a failed read the next reader must read again: %q %v %v", role, ok, err)
	}
}

// historyChatTurn is a history of n anchor-bearing messages (each long
// enough for the memo) plus a fresh question.
func historyChatTurn(n int) models.ChatRequest {
	req := models.ChatRequest{UserMessage: "and what about the token refresh? " + strings.Repeat("detail ", 60)}
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		req.Messages = append(req.Messages, models.ChatMessage{Role: role, Content: strings.Repeat("the token is rotated by the vault, password policy applies; ", 8) + string(rune('a'+i))})
	}
	return req
}

// The memo is per scope: keyed by content alone, a hit (measurably faster
// on a large, anchor-heavy text) would tell one user that another had
// recently sent that exact text.
func TestScrubMemo_IsPerScope(t *testing.T) {
	var calls atomic.Int32
	memo := newScrubMemo(1024, 1<<20)
	memo.scrub = func(s string) string { calls.Add(1); return scrubber.ScrubText(s) }
	text := strings.Repeat("the password policy for the api token applies here; ", 8)

	a1 := memo.scrubText("user-a", text)
	a2 := memo.scrubText("user-a", text)
	if n := calls.Load(); n != 1 {
		t.Fatalf("the same scope scrubbed the same text %d times, want 1 (memoised)", n)
	}
	b := memo.scrubText("user-b", text)
	if n := calls.Load(); n != 2 {
		t.Errorf("another scope reused user-a's entry (%d scrubs, want 2)", n)
	}
	if a1 != a2 || a1 != b || a1 != scrubber.ScrubText(text) {
		t.Error("scoped results must all equal ScrubText")
	}
}

// Every turn re-scrubbed the whole stored history with the secret regexes
// (~350 ms at the 1 MiB cap once anchor words appear). Each message is now
// scrubbed once; the next turn's history (the previous question included)
// reuses the result.
func TestBuildMessages_HistoryScrubbedOncePerMessage(t *testing.T) {
	var calls atomic.Int32
	memo := newScrubMemo(1024, 1<<20)
	memo.scrub = func(s string) string { calls.Add(1); return scrubber.ScrubText(s) }
	orig := historyScrubs
	historyScrubs = memo
	t.Cleanup(func() { historyScrubs = orig })

	req := historyChatTurn(6)
	first := buildScopedMessages("", req, "", "")
	if n := calls.Load(); n != 7 {
		t.Fatalf("first turn scrubbed %d texts, want 7 (6 history + question)", n)
	}
	// Next turn: the same history plus the previous question, and a new one.
	next := req
	next.Messages = append(append([]models.ChatMessage{}, req.Messages...), models.ChatMessage{Role: "user", Content: req.UserMessage})
	next.UserMessage = "one more question about rotating the api_key " + strings.Repeat("x", 300)
	second := buildScopedMessages("", next, "", "")
	if n := calls.Load() - 7; n != 1 {
		t.Errorf("second turn scrubbed %d texts, want 1 (only the new question)", n)
	}
	for i := range first[:len(first)-1] {
		if first[i].Role != second[i].Role || first[i].Content != second[i].Content {
			t.Errorf("message %d differs between turns", i)
		}
	}
}

// scrubVectors are the scrubber package's own secret samples (its
// stream_leak_test.go), assembled so no credential scanner matches the source.
var scrubVectors = []string{
	"config: password=SuperSecret123 done",
	"header: Bearer abcdefghijklmnopqrstuvwxyz012345 done",
	"key: " + "sk_" + "live_" + strings.Repeat("abcdefgh", 3) + " done",
	"tok: ghp_" + "123456789012345678901234567890123456 done",
	"dsn: Server=db1;User=sa;Password=P@ssw0rd123; done",
	"creds: AKIA" + "IOSFODNN7EXAMPLE done",
	"key: AIza" + "SyC1234567890abcdefghijklmnopqrstuv done",
	"tok: xoxb-" + "1234567890-abcdefghij done",
	"jwt: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N done",
	"-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEowIBAAKCAQEA1234567890\n-----END RSA " + "PRIVATE KEY-----",
}

// The memo is exactly ScrubText: cold and warm, for texts with and without
// secrets, short and long, under eviction pressure.
func TestScrubMemo_MatchesScrubFreeText(t *testing.T) {
	long := strings.Repeat("plain prose about the flow and its loops. ", 20)
	inputs := []string{
		"",
		"short",
		long,
		long + " password=SuperSecret123 " + long,
		"api_key: " + strings.Repeat("A1b2C3d4", 5) + " " + long,
		long + "Bearer abcdefghijklmnopqrstuvwxyz0123456789 and more " + long,
		long + "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA " + "PRIVATE KEY-----" + long,
		strings.Repeat("token ", 100),
		long + "Server=db;User Id=sa;Password=hunter2hunter2;Database=x\n" + long,
		long + "AKIA" + "IOSFODNN7EXAMPLE " + long,
		long + "xoxb-123456789012-abcdefghij " + long,
		long + "paſſword=SuperSecret123 done " + long,
	}
	for _, in := range scrubVectors {
		inputs = append(inputs, in, long+in+long)
	}
	memo := newScrubMemo(4, 4096) // tiny: entries are evicted between rounds
	for round := 0; round < 3; round++ {
		for i, in := range inputs {
			if got, want := memo.scrubText("", in), scrubber.ScrubFreeText(in); got != want {
				t.Fatalf("round %d input %d: memo = %q, ScrubFreeText = %q", round, i, got, want)
			}
		}
	}
	if memo.len() > 4 {
		t.Errorf("memo holds %d entries, bound is 4", memo.len())
	}
	memo.mu.Lock()
	if memo.bytes > 4096 {
		t.Errorf("memo holds %d bytes, bound is 4096", memo.bytes)
	}
	memo.mu.Unlock()
}

// The cached context's system prompt and context text are scrubbed once,
// when the entry is built — not again on every turn that reuses it.
func TestBuildStreamRequest_ContextScrubbedOncePerCacheEntry(t *testing.T) {
	var calls atomic.Int32
	orig := contextScrub
	contextScrub = func(s string) string { calls.Add(1); return scrubber.ScrubText(s) }
	t.Cleanup(func() { contextScrub = orig })

	registerFakeProvider()
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	provider, err := factory.For(context.Background(), "u1", "fake")
	if err != nil {
		t.Fatal(err)
	}
	svc := &ChatService{settings: nilSettings{}, chatCtxCache: newChatContextCache()}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	req := models.ChatRequest{Provider: "fake", Model: "fake-model", UserMessage: "hi"}
	ts := turnSetup{provider: provider, model: newTurnModel(req, nil)}

	for turn := 0; turn < 3; turn++ {
		aiReq, _, ok, err := svc.buildStreamRequest(context.Background(), ts, "u1", doc, nil, req)
		if err != nil || !ok || aiReq.SystemPrompt == "" {
			t.Fatalf("turn %d: %v %v %q", turn, ok, err, aiReq.SystemPrompt)
		}
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("context scrubbed %d times over 3 turns, want 2 (system prompt + context, once)", n)
	}
}

// tokenCountingProvider is a provider whose token estimate counts its calls.
type tokenCountingProvider struct {
	ai.Provider
	calls atomic.Int32
}

func (c *tokenCountingProvider) ID() string { return "counting" }
func (c *tokenCountingProvider) EstimateTokens(s string) int {
	c.calls.Add(1)
	return len(s) / 4
}

// Past 80% of the window every tool-loop iteration's truncation
// re-tokenised the whole (unchanged) history. One memo per loop: the next
// iteration estimates only what was appended — with identical results.
func TestTruncateForContextWindow_MemoAcrossIterations(t *testing.T) {
	p := &tokenCountingProvider{}
	msgs := []ai.Message{}
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, ai.Message{Role: role, Content: strings.Repeat("history text ", 60) + string(rune('a'+i))})
	}
	const ctxLimit = 10000 // 12 × ~800 bytes + the 4000 reserve: past the byte bound, inside the estimate
	build := func(extra int) ai.Request {
		m := append([]ai.Message{}, msgs...)
		for i := 0; i < extra; i++ {
			m = append(m, ai.Message{Role: "assistant", Content: "tool step " + string(rune('A'+i))})
		}
		return ai.Request{SystemPrompt: "you are a flow assistant", Messages: m}
	}

	memo := newTokenMemo()
	req := build(0)
	if _, err := truncateForContextWindowMemo(context.Background(), p, &req, ctxLimit, len(msgs)-1, memo); err != nil {
		t.Fatal(err)
	}
	first := p.calls.Load()
	if first == 0 {
		t.Fatal("the request never reached the estimate (fast path): resize the test")
	}
	req = build(1) // the next iteration: one appended exchange
	if _, err := truncateForContextWindowMemo(context.Background(), p, &req, ctxLimit, len(msgs)-1, memo); err != nil {
		t.Fatal(err)
	}
	if n := p.calls.Load() - first; n != 1 {
		t.Errorf("second iteration estimated %d texts, want 1 (only the appended message)", n)
	}

	// Same verdict as the unmemoised estimate, including when it drops.
	for _, limit := range []int{ctxLimit, 6500, 5200} {
		a, b := build(2), build(2)
		pa, ea := truncateForContextWindowMemo(context.Background(), p, &a, limit, len(msgs)-1, newTokenMemo())
		pb, eb := truncateForContextWindow(context.Background(), p, &b, limit, len(msgs)-1)
		if pa != pb || (ea == nil) != (eb == nil) || len(a.Messages) != len(b.Messages) {
			t.Errorf("limit %d: memo (%d, %v, %d msgs) != plain (%d, %v, %d msgs)", limit, pa, ea, len(a.Messages), pb, eb, len(b.Messages))
		}
	}
}

// countingResumeStore counts backplane writes.
type countingResumeStore struct {
	mu       sync.Mutex
	saves    []resumeSnapshot
	failNext int // the next failNext saves report failure
}

func (c *countingResumeStore) enabled() bool { return true }
func (c *countingResumeStore) Save(_ context.Context, _ string, snap resumeSnapshot, _ time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saves = append(c.saves, snap)
	if c.failNext > 0 {
		c.failNext--
		return false
	}
	return true
}
func (c *countingResumeStore) Load(context.Context, string) (resumeSnapshot, bool) {
	return resumeSnapshot{}, false
}
func (c *countingResumeStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.saves)
}

// A failed backplane write must be retried on the next tick even when the
// stream's state has not moved since: the mark used to advance regardless, so
// after a Redis blip a quiet stream (a model thinking, a tool running) left
// the backplane empty or stale until its state next changed. The initial
// save's failure is carried in as unpublishedMark the same way.
func TestMirrorStream_RetriesAFailedWrite(t *testing.T) {
	orig := resumeMirrorInterval
	resumeMirrorInterval = 5 * time.Millisecond
	t.Cleanup(func() { resumeMirrorInterval = orig })

	for _, initialFailed := range []bool{false, true} {
		store := &countingResumeStore{}
		svc := &ChatService{resume: store}
		ctl := &streamCtl{ownerID: "u1"}
		_, mark := ctl.snapshotMarked()
		if initialFailed {
			mark = unpublishedMark
		} else {
			store.failNext = 1
			ctl.mu.Lock()
			ctl.buffer.WriteString("partial answer")
			ctl.mu.Unlock()
		}
		finished, exited := make(chan struct{}), make(chan struct{})
		go func() {
			svc.mirrorStream(context.Background(), "s1", ctl, finished, mark)
			close(exited)
		}()
		time.Sleep(80 * time.Millisecond)
		close(finished)
		<-exited
		// initialFailed: one retry write, then the final one. Otherwise: the
		// failed write, its retry, then the final one.
		want := 2
		if !initialFailed {
			want = 3
		}
		if n := store.count(); n != want {
			t.Errorf("initial save failed=%v: %d writes, want %d (a failed write must be retried once, then the state is unchanged)", initialFailed, n, want)
		}
	}
}

// The resume mirror re-encoded and re-SET the full buffer and journal every
// tick even when nothing had changed (a model thinking, a tool running). A
// tick now writes only when the state moved; the final write always lands.
func TestMirrorStream_SkipsUnchangedTicks(t *testing.T) {
	orig := resumeMirrorInterval
	resumeMirrorInterval = 5 * time.Millisecond
	t.Cleanup(func() { resumeMirrorInterval = orig })

	store := &countingResumeStore{}
	svc := &ChatService{resume: store}
	ctl := &streamCtl{ownerID: "u1"}
	_, mark := ctl.snapshotMarked() // what the initial save published
	finished := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		svc.mirrorStream(context.Background(), "s1", ctl, finished, mark)
		close(exited)
	}()

	time.Sleep(60 * time.Millisecond)
	if n := store.count(); n != 0 {
		t.Fatalf("%d writes for a stream whose state never changed, want 0", n)
	}
	ctl.mu.Lock()
	ctl.buffer.WriteString("partial answer")
	ctl.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if n := store.count(); n != 1 {
		t.Fatalf("%d writes after one change, want 1", n)
	}
	ctl.mu.Lock()
	ctl.recordJournal("tool", map[string]interface{}{"name": "search_flow"})
	ctl.mu.Unlock()
	time.Sleep(60 * time.Millisecond)
	if n := store.count(); n != 2 {
		t.Fatalf("%d writes after a journal event, want 2", n)
	}

	ctl.markDone(1, 2, "")
	close(finished)
	<-exited
	store.mu.Lock()
	last := store.saves[len(store.saves)-1]
	store.mu.Unlock()
	if !last.Done || last.Text != "partial answer" || len(last.Events) != 1 {
		t.Errorf("final snapshot = %+v", last)
	}
}
