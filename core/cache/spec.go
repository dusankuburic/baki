package cache

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Spec is a declared cache kind: its name, the parts every key carries, why it may be tenant-less, where it lives,
// and the statistics of every cache built on it. Declare one per kind, at package level.
type Spec struct {
	name     string
	kinds    []PartKind
	global   string
	locality Locality
	where    string
	stats    stats
}

// Locality is where a cache's entries live, which decides what another replica sees (Phase 5b).
type Locality uint8

const (
	// LocalityReplica: each replica holds its own entries; Declare records why serving one replica's possibly stale
	// entry is acceptable.
	LocalityReplica Locality = iota + 1
	// LocalityShared: the entries are shared across replicas by a named mechanism (reserved for Phase 5b).
	LocalityShared
)

func (l Locality) String() string {
	switch l {
	case LocalityReplica:
		return "replica-local"
	case LocalityShared:
		return "shared"
	}
	return "invalid"
}

// SpecOption configures a declaration.
type SpecOption func(*specConfig)

type specConfig struct {
	kinds     []PartKind
	keysGiven int
	global    string
	globals   int
	locality  Locality
	where     string
	locations int
}

// Keys lists the parts every key of the cache carries, in encoding order. KindTenant is required unless the cache
// is Global, and appears at most once; the other kinds may repeat (two Params, say).
func Keys(kinds ...PartKind) SpecOption {
	return func(c *specConfig) {
		c.kinds = append([]PartKind(nil), kinds...)
		c.keysGiven++
	}
}

// Global declares a cache that is deliberately not keyed by a tenant; the reason goes into the registry golden.
func Global(reason string) SpecOption {
	return func(c *specConfig) {
		c.global = reason
		c.globals++
	}
}

// ReplicaLocal declares a cache each replica holds on its own, with why per-replica staleness is acceptable.
func ReplicaLocal(reason string) SpecOption {
	return func(c *specConfig) {
		c.locality, c.where = LocalityReplica, reason
		c.locations++
	}
}

// Shared declares a cache shared across replicas by mechanism (reserved for Phase 5b; New refuses such a spec).
func Shared(mechanism string) SpecOption {
	return func(c *specConfig) {
		c.locality, c.where = LocalityShared, mechanism
		c.locations++
	}
}

// maxParts bounds a key's parts (Key matches them without allocating).
const maxParts = 16

// specName: dot-separated lowercase segments ("analyzer.reports", "flow.search-index"). It is a metric label value.
var specName = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)+$`)

var registry = struct {
	sync.Mutex
	specs map[string]*Spec
}{specs: map[string]*Spec{}}

// Declare registers a cache kind. Call it once per kind, in a package-level var: a declaration error panics, so
// the boot and every test of the package fail at init. It panics when the name is malformed or already declared,
// when the keys carry no KindTenant and Global is absent, when both are given, when a kind is invalid or the tenant
// repeats, and when the locality (ReplicaLocal or Shared) is missing, doubled or has no reason.
func Declare(name string, opts ...SpecOption) *Spec {
	s, err := newSpec(name, opts...)
	if err != nil {
		panic(err)
	}
	registry.Lock()
	defer registry.Unlock()
	if _, dup := registry.specs[name]; dup {
		panic(fmt.Errorf("cache: %q is declared twice", name))
	}
	registry.specs[name] = s
	return s
}

// newSpec validates a declaration without registering it.
func newSpec(name string, opts ...SpecOption) (*Spec, error) {
	if len(name) > 64 || !specName.MatchString(name) {
		return nil, fmt.Errorf("cache: name %q must be dot-separated lowercase segments (domain.name), at most 64 bytes", name)
	}
	var c specConfig
	for _, opt := range opts {
		opt(&c)
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("cache %q: "+format, append([]any{name}, args...)...)
	}
	if c.keysGiven > 1 {
		return nil, bad("Keys given twice")
	}
	if len(c.kinds) > maxParts {
		return nil, bad("%d key parts, at most %d", len(c.kinds), maxParts)
	}
	tenants := 0
	for _, k := range c.kinds {
		if !k.valid() {
			return nil, bad("invalid part kind %d", k)
		}
		if k == KindTenant {
			tenants++
		}
	}
	switch {
	case tenants > 1:
		return nil, bad("the tenant part is declared %d times", tenants)
	case c.globals > 1:
		return nil, bad("Global given twice")
	case c.globals == 1 && strings.TrimSpace(c.global) == "":
		return nil, bad("Global needs a reason")
	case c.globals == 1 && tenants == 1:
		return nil, bad("keyed by a tenant and Global at once")
	case c.globals == 0 && tenants == 0:
		return nil, bad("keys carry no KindTenant: add the tenant part, or declare Global(reason) if the cache is deliberately tenant-less")
	case c.locations == 0:
		return nil, bad("no locality: declare ReplicaLocal(reason) or Shared(mechanism)")
	case c.locations > 1:
		return nil, bad("more than one locality")
	case strings.TrimSpace(c.where) == "":
		return nil, bad("%s needs a reason", c.locality)
	}
	return &Spec{name: name, kinds: c.kinds, global: c.global, locality: c.locality, where: c.where}, nil
}

// Name is the declared name.
func (s *Spec) Name() string { return s.name }

// ErrInvalidKey is what every key error matches with errors.Is.
var ErrInvalidKey = errors.New("cache: invalid key")

// KeyError is a key that does not match its spec: a zero part (built from empty input), a missing declared part, an
// extra one, or a key used with another spec's cache.
type KeyError struct {
	Spec    string
	Kind    PartKind // the part concerned; 0 when the problem is not one part
	Problem string   // "zero", "missing", "extra", "foreign"
}

func (e *KeyError) Error() string {
	switch {
	case e.Problem == "zero":
		return fmt.Sprintf("cache %q: a key part is zero (built from empty input)", e.Spec)
	case e.Problem == "foreign":
		return fmt.Sprintf("cache %q: the key was built by another cache's spec", e.Spec)
	case e.Kind == 0:
		return fmt.Sprintf("cache %q: %s key parts", e.Spec, e.Problem)
	}
	return fmt.Sprintf("cache %q: %s %s part", e.Spec, e.Problem, e.Kind)
}

func (e *KeyError) Unwrap() error { return ErrInvalidKey }

// keyErrorsPanic decides whether a key error panics (in test binaries) or is returned (in production, where the
// caller computes without the cache). SetKeyErrorPanicForTesting overrides it.
var keyErrorsPanic atomic.Bool

func init() { keyErrorsPanic.Store(testing.Testing()) }

// SetKeyErrorPanicForTesting makes key errors panic (on) or be returned (off), as in production, and returns the
// restore. Tests that assert the production path (a key error skips the cache) turn it off; not for parallel tests.
func SetKeyErrorPanicForTesting(on bool) (restore func()) {
	prev := keyErrorsPanic.Swap(on)
	return func() { keyErrorsPanic.Store(prev) }
}

// keyError counts a rejected key on s and panics under test.
func (s *Spec) keyError(err *KeyError) error {
	s.stats.rejected.Add(1)
	if keyErrorsPanic.Load() {
		panic(err)
	}
	return err
}

// Key is a key of one spec. The zero Key is never a cache hit: a cache given it computes without storing.
type Key struct {
	spec *Spec
	enc  string
}

// Key builds a key from exactly the declared parts, none zero, in any argument order (parts of the same kind keep
// their argument order). The encoding is length-prefixed in the declared order, so distinct part tuples never
// collide. An error (wrapping ErrInvalidKey) means "do not cache": the caller computes and skips the store, and the
// rejection is counted. In a test binary it panics instead, so a missing part fails the test that hit it.
func (s *Spec) Key(parts ...Part) (Key, error) {
	for _, p := range parts {
		if p.IsZero() {
			return Key{}, s.keyError(&KeyError{Spec: s.name, Problem: "zero"})
		}
	}
	if len(parts) > maxParts {
		return Key{}, s.keyError(&KeyError{Spec: s.name, Problem: "extra"})
	}
	var used [maxParts]bool
	order := make([]int, 0, maxParts) // stack-allocated: constant capacity, does not escape
	size := 0
	for _, kind := range s.kinds {
		at := -1
		for i, p := range parts {
			if !used[i] && p.kind == kind {
				at = i
				break
			}
		}
		if at < 0 {
			return Key{}, s.keyError(&KeyError{Spec: s.name, Kind: kind, Problem: "missing"})
		}
		used[at] = true
		order = append(order, at)
		size += 1 + decimalLen(len(parts[at].name)) + 1 + len(parts[at].name) + decimalLen(len(parts[at].val)) + 1 + len(parts[at].val)
	}
	for i, p := range parts {
		if !used[i] {
			return Key{}, s.keyError(&KeyError{Spec: s.name, Kind: p.kind, Problem: "extra"})
		}
	}
	var b strings.Builder
	b.Grow(size)
	var num [20]byte
	for _, i := range order {
		p := parts[i]
		b.WriteByte(kindCodes[p.kind])
		b.Write(strconv.AppendInt(num[:0], int64(len(p.name)), 10))
		b.WriteByte(':')
		b.WriteString(p.name)
		b.Write(strconv.AppendInt(num[:0], int64(len(p.val)), 10))
		b.WriteByte(':')
		b.WriteString(p.val)
	}
	return Key{spec: s, enc: b.String()}, nil
}

// decimalLen is the number of decimal digits of n >= 0.
func decimalLen(n int) int {
	d := 1
	for n >= 10 {
		n /= 10
		d++
	}
	return d
}

// IsZero reports whether k is the zero Key (what a failed Key returns).
func (k Key) IsZero() bool { return k.spec == nil }

// Spec is the spec k was built by; nil for the zero Key.
func (k Key) Spec() *Spec { return k.spec }

// String is the key's encoding: one tagged, length-prefixed component per part, in declared order.
func (k Key) String() string { return k.enc }

// Parts decodes the key back into its parts, in declared order.
func (k Key) Parts() []Part {
	parts, _ := decodeKey(k.enc)
	return parts
}

// decodeKey is the inverse of the encoding Key writes; ok is false for a string Key cannot have written.
func decodeKey(enc string) (parts []Part, ok bool) {
	for enc != "" {
		kind := kindOfCode(enc[0])
		if kind == 0 {
			return nil, false
		}
		name, rest, ok := cutPrefixed(enc[1:])
		if !ok {
			return nil, false
		}
		val, rest, ok := cutPrefixed(rest)
		if !ok {
			return nil, false
		}
		parts = append(parts, Part{kind: kind, name: name, val: val})
		enc = rest
	}
	return parts, true
}

// lenPrefixed encodes s as "<len>:<s>".
func lenPrefixed(s string) string { return strconv.Itoa(len(s)) + ":" + s }

// cutPrefixed reads one "<len>:<s>" from the front of enc.
func cutPrefixed(enc string) (s, rest string, ok bool) {
	head, tail, found := strings.Cut(enc, ":")
	if !found {
		return "", "", false
	}
	n, err := strconv.Atoi(head)
	if err != nil || n < 0 || n > len(tail) || strconv.Itoa(n) != head {
		return "", "", false
	}
	return tail[:n], tail[n:], true
}

// SpecInfo is one declaration and its live statistics.
type SpecInfo struct {
	Name     string
	Kinds    []PartKind
	Global   string // why the cache is deliberately tenant-less; empty when its keys carry the tenant
	Locality Locality
	Where    string // the ReplicaLocal reason or the Shared mechanism
	Stats    Stats
}

// Info is s's declaration and live statistics.
func (s *Spec) Info() SpecInfo {
	return SpecInfo{
		Name:     s.name,
		Kinds:    append([]PartKind(nil), s.kinds...),
		Global:   s.global,
		Locality: s.locality,
		Where:    s.where,
		Stats:    s.stats.snapshot(),
	}
}

// Specs lists every declared cache with its statistics, sorted by name.
func Specs() []SpecInfo {
	registry.Lock()
	specs := make([]*Spec, 0, len(registry.specs))
	for _, s := range registry.specs {
		specs = append(specs, s)
	}
	registry.Unlock()
	sort.Slice(specs, func(i, j int) bool { return specs[i].name < specs[j].name })
	out := make([]SpecInfo, len(specs))
	for i, s := range specs {
		out[i] = s.Info()
	}
	return out
}
