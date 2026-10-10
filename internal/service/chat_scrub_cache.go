package service

import (
	"container/list"
	"crypto/sha256"
	"sync"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
	"pad-core/cache"
)

// The chat's process-wide text memos (ADR 0017). Their keys are built through the specs, and the maps hold each
// key's SHA-256: the history is up to 1 MiB, and an entry should not hold its text a second time.
var (
	// historyScrubs' entries are per caller: a hit is measurably faster, so content-only keys would tell one user
	// that another had recently sent that exact text.
	historyScrubSpec = cache.Declare("chat.history-scrubs",
		cache.Keys(cache.KindTenant, cache.KindContent),
		cache.ReplicaLocal("a pure function of the caller's text; another replica scrubs it again"))
	// tokenCounts' entries are shared: a count reveals that someone sent the text only through timing, and the
	// texts it mostly counts (system prompts, tool schemas) repeat across users.
	tokenCountSpec = cache.Declare("chat.token-counts",
		cache.Keys(cache.KindParam, cache.KindContent),
		cache.Global("a token count is a function of the provider's tokenizer and the text alone, shared across users like the system prompts it mostly counts"),
		cache.ReplicaLocal("a pure function of the text; another replica counts it again"))
)

// memoKey is the map key of a memo entry, the SHA-256 of spec's key for parts; false when the key is refused.
func memoKey(spec *cache.Spec, parts ...cache.Part) (key [sha256.Size]byte, ok bool) {
	k, err := spec.Key(parts...)
	if err != nil {
		return key, false
	}
	return sha256.Sum256([]byte(k.String())), true
}

// textDigest is the content part of a memo key: the text's SHA-256.
func textDigest(text string) cache.Part {
	sum := sha256.Sum256([]byte(text))
	return cache.Content(string(sum[:]))
}

// scrubMemo memoises scrubber.ScrubText for the chat history: the secret regexes run at
// ~3 MB/s, so re-scrubbing the stored conversation every turn cost ~350 ms at the 1 MiB
// history cap. Results are keyed by the caller's scope and the content's SHA-256
// (historyScrubSpec) — never the text itself — bounded by entries and by the bytes of
// redacted texts held, LRU evicted; short texts are scrubbed directly (the hash costs
// about as much as the prefiltered scrub).
type scrubMemo struct {
	// scrub is scrubber.ScrubText (tests count it).
	scrub      func(string) string
	mu         sync.Mutex
	ll         *list.List // front = most recently used; values are *scrubMemoEntry
	m          map[[sha256.Size]byte]*list.Element
	bytes      int
	maxBytes   int
	maxEntries int
}

type scrubMemoEntry struct {
	key      [sha256.Size]byte
	same     bool   // ScrubText returned the text unchanged
	scrubbed string // the redacted text when !same
	cost     int
}

const (
	// scrubMemoMinBytes: texts below it are scrubbed directly.
	scrubMemoMinBytes = 256
	// scrubMemoEntryOverhead approximates one entry's fixed cost.
	scrubMemoEntryOverhead = 96
)

func newScrubMemo(maxEntries, maxBytes int) *scrubMemo {
	c := &scrubMemo{scrub: scrubber.ScrubText, ll: list.New(), m: make(map[[sha256.Size]byte]*list.Element), maxBytes: maxBytes, maxEntries: maxEntries}
	cache.Track(historyScrubSpec, c)
	return c
}

// historyScrubs is the process-wide memo buildScopedMessages scrubs through.
var historyScrubs = newScrubMemo(16384, 32<<20)

// scrubText is scrubber.ScrubText(text), memoised per scope for texts of
// scrubMemoMinBytes and up.
func (c *scrubMemo) scrubText(scope, text string) string {
	if len(text) < scrubMemoMinBytes {
		return c.scrub(text)
	}
	key, ok := memoKey(historyScrubSpec, callerTenant(scope), textDigest(text))
	if !ok {
		return c.scrub(text)
	}

	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*scrubMemoEntry)
		c.mu.Unlock()
		historyScrubSpec.RecordHit()
		if e.same {
			return text
		}
		return e.scrubbed
	}
	c.mu.Unlock()
	historyScrubSpec.RecordMiss()

	out := c.scrub(text)
	e := &scrubMemoEntry{key: key, same: out == text, cost: scrubMemoEntryOverhead}
	if !e.same {
		e.scrubbed = out
		e.cost += len(out)
	}
	if e.cost > c.maxBytes {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return out // another goroutine stored it meanwhile
	}
	c.m[key] = c.ll.PushFront(e)
	c.bytes += e.cost
	historyScrubSpec.RecordStore()
	for c.ll.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.ll.Back()
		victim := oldest.Value.(*scrubMemoEntry)
		c.ll.Remove(oldest)
		delete(c.m, victim.key)
		c.bytes -= victim.cost
		historyScrubSpec.RecordEviction(cache.EvictCapacity)
	}
	return out
}

// len reports the number of entries (tests).
func (c *scrubMemo) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Len and Cost size the memo for the cache metrics: its entries and the bytes they hold.
func (c *scrubMemo) Len() int { return c.len() }

func (c *scrubMemo) Cost() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return int64(c.bytes)
}

// tokenCountMemo memoises provider.EstimateTokens across turns: the per-turn memo
// (tokenMemo) spans one tool loop, so every turn re-estimated the same immutable stored
// messages (~110 ms per 400 KB through the cl100k BPE). Keyed by the provider id and the
// text's SHA-256 (tokenCountSpec) — never the text itself (a 1 MiB history would be held
// twice over); the provider id because a test double sharing a real provider's id must
// estimate the way that provider does. Bounded by entries, LRU evicted.
type tokenCountMemo struct {
	mu         sync.Mutex
	ll         *list.List // front = most recently used; values are *tokenCountEntry
	m          map[[sha256.Size]byte]*list.Element
	maxEntries int
}

type tokenCountEntry struct {
	key    [sha256.Size]byte
	tokens int
}

func newTokenCountMemo(maxEntries int) *tokenCountMemo {
	c := &tokenCountMemo{ll: list.New(), m: make(map[[sha256.Size]byte]*list.Element), maxEntries: maxEntries}
	cache.Track(tokenCountSpec, c)
	return c
}

// tokenCounts is the process-wide memo tokenMemo.estimate falls back to.
var tokenCounts = newTokenCountMemo(16384)

// estimate is provider.EstimateTokens(text), memoised by provider id and text.
func (c *tokenCountMemo) estimate(provider ai.Provider, text string) int {
	key, ok := memoKey(tokenCountSpec, cache.Param("provider", provider.ID()), textDigest(text))
	if !ok {
		return provider.EstimateTokens(text)
	}

	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		n := el.Value.(*tokenCountEntry).tokens
		c.mu.Unlock()
		tokenCountSpec.RecordHit()
		return n
	}
	c.mu.Unlock()
	tokenCountSpec.RecordMiss()

	n := provider.EstimateTokens(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return n // another goroutine stored it meanwhile
	}
	c.m[key] = c.ll.PushFront(&tokenCountEntry{key: key, tokens: n})
	tokenCountSpec.RecordStore()
	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.m, oldest.Value.(*tokenCountEntry).key)
		tokenCountSpec.RecordEviction(cache.EvictCapacity)
	}
	return n
}

// len reports the number of entries (tests).
func (c *tokenCountMemo) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Len and Cost size the memo for the cache metrics (one unit per count).
func (c *tokenCountMemo) Len() int { return c.len() }

func (c *tokenCountMemo) Cost() int64 { return int64(c.len()) }
