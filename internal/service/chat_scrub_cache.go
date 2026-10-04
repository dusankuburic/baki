package service

import (
	"container/list"
	"crypto/sha256"
	"io"
	"sync"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
)

// scrubMemo memoises scrubber.ScrubText for the chat history: the secret regexes run at
// ~3 MB/s, so re-scrubbing the stored conversation every turn cost ~350 ms at the 1 MiB
// history cap. Results are keyed by the SHA-256 of the caller's scope and the content —
// never the text itself (content-only keying would tell one user that another had
// recently sent that exact text) — bounded by entries and by the bytes of redacted texts
// held, LRU evicted; short texts are scrubbed directly (the hash costs about as much as
// the prefiltered scrub).
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
	return &scrubMemo{scrub: scrubber.ScrubText, ll: list.New(), m: make(map[[sha256.Size]byte]*list.Element), maxBytes: maxBytes, maxEntries: maxEntries}
}

// historyScrubs is the process-wide memo buildScopedMessages scrubs through.
var historyScrubs = newScrubMemo(16384, 32<<20)

// scrubText is scrubber.ScrubText(text), memoised per scope for texts of
// scrubMemoMinBytes and up.
func (c *scrubMemo) scrubText(scope, text string) string {
	if len(text) < scrubMemoMinBytes {
		return c.scrub(text)
	}
	h := sha256.New()
	_, _ = io.WriteString(h, scope)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, text)
	var key [sha256.Size]byte
	h.Sum(key[:0])

	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*scrubMemoEntry)
		c.mu.Unlock()
		if e.same {
			return text
		}
		return e.scrubbed
	}
	c.mu.Unlock()

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
	for c.ll.Len() > c.maxEntries || c.bytes > c.maxBytes {
		oldest := c.ll.Back()
		victim := oldest.Value.(*scrubMemoEntry)
		c.ll.Remove(oldest)
		delete(c.m, victim.key)
		c.bytes -= victim.cost
	}
	return out
}

// len reports the number of entries (tests).
func (c *scrubMemo) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// tokenCountMemo memoises provider.EstimateTokens across turns: the per-turn memo
// (tokenMemo) spans one tool loop, so every turn re-estimated the same immutable stored
// messages (~110 ms per 400 KB through the cl100k BPE). Keyed by the SHA-256 of the
// provider id and the text — never the text alone (a 1 MiB history would be held twice
// over); the provider id because a test double sharing a real provider's id must
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
	return &tokenCountMemo{ll: list.New(), m: make(map[[sha256.Size]byte]*list.Element), maxEntries: maxEntries}
}

// tokenCounts is the process-wide memo tokenMemo.estimate falls back to.
var tokenCounts = newTokenCountMemo(16384)

// estimate is provider.EstimateTokens(text), memoised by provider id and text.
func (c *tokenCountMemo) estimate(provider ai.Provider, text string) int {
	h := sha256.New()
	_, _ = io.WriteString(h, provider.ID())
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, text)
	var key [sha256.Size]byte
	h.Sum(key[:0])

	c.mu.Lock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		n := el.Value.(*tokenCountEntry).tokens
		c.mu.Unlock()
		return n
	}
	c.mu.Unlock()

	n := provider.EstimateTokens(text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.m[key]; ok {
		return n // another goroutine stored it meanwhile
	}
	c.m[key] = c.ll.PushFront(&tokenCountEntry{key: key, tokens: n})
	for c.ll.Len() > c.maxEntries {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.m, oldest.Value.(*tokenCountEntry).key)
	}
	return n
}

// len reports the number of entries (tests).
func (c *tokenCountMemo) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}
