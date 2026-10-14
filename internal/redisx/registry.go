package redisx

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Kind is the Redis data structure a declared key or channel uses.
type Kind uint8

const (
	KindPubSub Kind = iota + 1
	KindHash
	KindString
	KindZSet
	KindStream
)

func (k Kind) String() string {
	switch k {
	case KindPubSub:
		return "pubsub"
	case KindHash:
		return "hash"
	case KindString:
		return "string"
	case KindZSet:
		return "zset"
	case KindStream:
		return "stream"
	}
	return "invalid"
}

// OnDown is what a mechanism does while Redis is unreachable. It is the row of the failure-mode table
// (docs/adr/0029-redis-envelope-key-registry-and-health.md) the mechanism answers to.
type OnDown uint8

const (
	// OnDownOpen: local behaviour continues, the cross-replica part is lost until Redis returns (a TTL bounds the
	// staleness).
	OnDownOpen OnDown = iota + 1
	// OnDownDegrade: a weaker local stand-in keeps the guarantee approximately (the rate limiter's per-replica
	// bucket: the limit becomes N times the rate).
	OnDownDegrade
	// OnDownClosed: the operation is refused rather than guessed (a read of the revocation set that cannot be
	// answered).
	OnDownClosed
)

func (o OnDown) String() string {
	switch o {
	case OnDownOpen:
		return "open"
	case OnDownDegrade:
		return "degrade"
	case OnDownClosed:
		return "closed"
	}
	return "invalid"
}

// Spec declares one family of Redis keys or one pub/sub channel family: what it is called, how its names are
// built, how long they live, which payload version its owner writes and what the owner does while Redis is down.
// Every key a replica writes is declared, so the golden (testdata/redis_keys.golden) names all of them, the server
// can run an eviction policy that only evicts keys with a TTL (volatile-lru) and a rolling deploy has a list of
// what two releases share.
type Spec struct {
	// Name is dot-separated lowercase segments ("ws.room"). With the dots turned into underscores it is the
	// mechanism label of pad_redis_errors_total.
	Name string
	Kind Kind
	// Pattern is the key or channel name with {placeholder} parts for the variable segments
	// ("ws:room:{flow}"). It starts with a literal prefix, so a key can be matched back to its spec, and it is
	// never renamed once shipped: a rolling deploy runs two releases against the same keys.
	Pattern string
	// TTL is the longest a key lives without a write. Required for every kind except pub/sub (a channel stores
	// nothing); a key that never expires cannot be evicted under volatile-lru and is never reclaimed.
	TTL time.Duration
	// Version is the payload version the owner writes: 0 is the shape every release before the envelope wrote, 1
	// carries {"v":1,"kind":...}. Readers accept every version up to it and ignore a higher one.
	Version int
	// Kinds lists the payload kinds a channel carries when it carries several (the invalidation channel's inval,
	// recheck, hb and resync). Without it a channel carries one kind, the Name, and the layer under the subscriber
	// drops a message of another; with it the subscriber decodes and counts each kind itself (Decode with the kind
	// it expects), so a kind added later is its own change. Channels only.
	Kinds  []string
	OnDown OnDown
	// Owner is the package (slash path from the repository root) that reads and writes the key.
	Owner string
}

// Key is a declared Spec with its pattern parsed. Build names with Key (or Glob for a pattern subscription): that
// keeps the literal in the declaration and out of the call sites, and the archtest ratchet holds it there.
type Key struct {
	spec Spec
	lit  []string // literal text before, between and after the placeholders: len(vars)+1 elements
	vars []string
	glob string
}

var (
	specName    = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)
	payloadKind = regexp.MustCompile(`^[a-z][a-z0-9.-]*$`)
	placeholder = regexp.MustCompile(`\{([a-zA-Z][a-zA-Z0-9]*)\}`)
)

var registry = struct {
	sync.Mutex
	keys map[string]*Key
}{keys: map[string]*Key{}}

// Declare registers a key family. Call it once per family, in a package-level var: a malformed declaration panics,
// so the boot and every test of the declaring package fail at init. It panics for a malformed or repeated name or
// pattern, a pattern that starts with a placeholder or has an unbalanced brace, an invalid kind or on-down
// behaviour, a missing owner, a missing TTL on a kind that stores something, and a TTL on a channel.
func Declare(s Spec) *Key {
	k, err := newKey(s)
	if err != nil {
		panic(err)
	}
	registry.Lock()
	defer registry.Unlock()
	for _, other := range registry.keys {
		switch {
		case other.spec.Name == s.Name:
			panic(fmt.Errorf("redisx: %q is declared twice", s.Name))
		case other.spec.Pattern == s.Pattern:
			panic(fmt.Errorf("redisx: %q declares the pattern %q of %q", s.Name, s.Pattern, other.spec.Name))
		}
	}
	registry.keys[s.Name] = k
	return k
}

func newKey(s Spec) (*Key, error) {
	bad := func(format string, args ...any) (*Key, error) {
		return nil, fmt.Errorf("redisx: %q: "+format, append([]any{s.Name}, args...)...)
	}
	if len(s.Name) > 64 || !specName.MatchString(s.Name) {
		return nil, fmt.Errorf("redisx: name %q must be dot-separated lowercase segments (domain.name), at most 64 bytes", s.Name)
	}
	if s.Kind < KindPubSub || s.Kind > KindStream {
		return bad("kind is not set")
	}
	if s.OnDown < OnDownOpen || s.OnDown > OnDownClosed {
		return bad("on-down behaviour is not set")
	}
	if s.Owner == "" {
		return bad("owner is not set")
	}
	if s.Version < 0 {
		return bad("negative version")
	}
	if len(s.Kinds) > 0 && s.Kind != KindPubSub {
		return bad("kinds are for a channel that carries several payload kinds")
	}
	seenKinds := map[string]bool{}
	for _, k := range s.Kinds {
		if !payloadKind.MatchString(k) || seenKinds[k] {
			return bad("kind %q must be lowercase letters, digits, dots and hyphens, and listed once", k)
		}
		seenKinds[k] = true
	}
	switch {
	case s.Kind == KindPubSub && s.TTL != 0:
		return bad("a channel stores nothing, so it has no TTL")
	case s.Kind != KindPubSub && s.TTL <= 0:
		return bad("every key needs a TTL (the server evicts only keys that have one)")
	}
	locs := placeholder.FindAllStringSubmatchIndex(s.Pattern, -1)
	k := &Key{spec: s}
	rest := 0
	var glob strings.Builder
	for _, loc := range locs {
		k.lit = append(k.lit, s.Pattern[rest:loc[0]])
		k.vars = append(k.vars, s.Pattern[loc[2]:loc[3]])
		glob.WriteString(s.Pattern[rest:loc[0]])
		glob.WriteByte('*')
		rest = loc[1]
	}
	k.lit = append(k.lit, s.Pattern[rest:])
	glob.WriteString(s.Pattern[rest:])
	k.glob = glob.String()
	if strings.ContainsAny(strings.Join(k.lit, ""), "{}*? \t\r\n") {
		return bad("pattern %q has an unbalanced brace, a glob character or a space", s.Pattern)
	}
	if k.lit[0] == "" {
		return bad("pattern %q must start with a literal prefix", s.Pattern)
	}
	// go-redis v9.21 peeks 36 bytes of the reply to a subscription before it reads it, and the reply to a
	// (P)SUBSCRIBE is shorter than that for a name under 8 bytes: Receive then waits for the next message instead
	// of the confirmation, and a subscriber never reports ready.
	if s.Kind == KindPubSub && len(k.lit[0]) < minChannelPrefix {
		return bad("a channel needs a literal prefix of at least %d bytes (go-redis stalls on the confirmation of a shorter name), %q has %d", minChannelPrefix, k.lit[0], len(k.lit[0]))
	}
	return k, nil
}

// Spec returns the declaration.
func (k *Key) Spec() Spec { return k.spec }

// Name is the declared name.
func (k *Key) Name() string { return k.spec.Name }

// Prefix is the literal text a name starts with, up to the first variable part.
func (k *Key) Prefix() string { return k.lit[0] }

// Glob is the pattern with every variable part as "*", for a pattern subscription (PSUBSCRIBE) or a SCAN match.
func (k *Key) Glob() string { return k.glob }

// Key builds the name from its variable parts in pattern order. A count that differs from the pattern's panics:
// that is a bug in the call site, which a test of the call site finds.
func (k *Key) Key(parts ...string) string {
	if len(parts) != len(k.vars) {
		panic(fmt.Sprintf("redisx: %s takes %d parts (%s), got %d", k.spec.Name, len(k.vars), strings.Join(k.vars, ", "), len(parts)))
	}
	n := 0
	for _, l := range k.lit {
		n += len(l)
	}
	for _, p := range parts {
		n += len(p)
	}
	var b strings.Builder
	b.Grow(n)
	for i, p := range parts {
		b.WriteString(k.lit[i])
		b.WriteString(p)
	}
	b.WriteString(k.lit[len(parts)])
	return b.String()
}

// Specs returns every declared family, sorted by name.
func Specs() []Spec {
	registry.Lock()
	defer registry.Unlock()
	out := make([]Spec, 0, len(registry.keys))
	for _, k := range registry.keys {
		out = append(out, k.spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup returns the family declared under name.
func Lookup(name string) (*Key, bool) {
	registry.Lock()
	defer registry.Unlock()
	k, ok := registry.keys[name]
	return k, ok
}

// minChannelPrefix is the shortest literal prefix of a channel pattern: see newKey.
const minChannelPrefix = 8

// otherMechanism labels a key no declaration claims.
const otherMechanism = "other"

// Match returns the family a key or channel name belongs to: the one with the longest declared prefix it starts
// with.
func Match(name string) (Spec, bool) {
	registry.Lock()
	defer registry.Unlock()
	var best *Key
	for _, k := range registry.keys {
		if strings.HasPrefix(name, k.lit[0]) && (best == nil || len(k.lit[0]) > len(best.lit[0])) {
			best = k
		}
	}
	if best == nil {
		return Spec{}, false
	}
	return best.spec, true
}

// MechanismOf names the mechanism a Redis key or channel belongs to (see Match), or "other". It is the label of
// pad_redis_errors_total, so its value set is the declared names: closed, and bounded by the code.
func MechanismOf(key string) string {
	if s, ok := Match(key); ok {
		return strings.ReplaceAll(s.Name, ".", "_")
	}
	return otherMechanism
}

// Describe renders a declaration as one line of the registry golden.
func Describe(s Spec) string {
	ttl := "none"
	if s.TTL > 0 {
		ttl = s.TTL.String()
	}
	kinds := ""
	if len(s.Kinds) > 0 {
		kinds = " | kinds: " + strings.Join(s.Kinds, ", ")
	}
	return fmt.Sprintf("%s | %s | %s | ttl: %s | v%d%s | on-down: %s | %s", s.Name, s.Kind, s.Pattern, ttl, s.Version, kinds, s.OnDown, s.Owner)
}
