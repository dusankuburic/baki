package rag

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// ErrEmbedderMismatch: the caller's embedder cannot see a single chunk of the org's corpus — every chunk was embedded
// under another embedding identity (ADR 0027) — so a search would pay for a query embedding and find nothing. It is
// answered before the embedding (R-knowledge-1). The remedy is a re-index under the caller's model or, for a
// deployment whose members embed with their own keys, the operator's pin of the embedding provider, which embeds on
// the organisation's shared key.
var ErrEmbedderMismatch = errors.New("the knowledge base was indexed with another embedding model than yours, so your searches cannot see it — re-index it, or ask the operator to pin the embedding provider (PAD_EMBEDDING_PROVIDER) so members embed on the organisation's shared key")

// The phases of a knowledge search, in the order they run (SearchResult.Phases, pad_chat_rag_lookup_seconds{phase}).
const (
	// PhaseResolve: the corpus count, the caller's embedder and the identity census.
	PhaseResolve = "resolve"
	// PhaseEmbed: the query embedding (a memoised one included).
	PhaseEmbed = "embed"
	// PhaseSearch: the vector and the lexical statements.
	PhaseSearch = "search"
)

// SearchPhase is the time one phase of a search took, until it finished or failed.
type SearchPhase struct {
	Name string
	Took time.Duration
}

// SearchResult is one knowledge search: the guidelines block ("" for none) and the phases it ran.
type SearchResult struct {
	Guidelines string
	Phases     []SearchPhase
	// followUp explains an empty result for ops (explainEmptySearch); nil when there is nothing to explain.
	followUp func(context.Context)
}

// FollowUp is what is left to do once the result is delivered, best effort: an empty result's census for ops (the
// stranded and width signals). nil when there is nothing; the caller runs it off its critical path, under a context
// of its own.
func (r SearchResult) FollowUp() func(context.Context) { return r.followUp }

// searchTopK is how many chunks a search returns, and how many each half of the hybrid search hands the fusion: five
// is more context than one chunk at little cost, and the floors keep the irrelevant ones out.
const searchTopK = 5

// Search returns the org's guidelines for query (SearchDetailed), with an empty result's census run before it returns.
func (s *KnowledgeService) Search(ctx context.Context, scope, orgID, query string) (string, error) {
	res, err := s.SearchDetailed(ctx, scope, orgID, query)
	if f := res.FollowUp(); f != nil {
		f(ctx)
	}
	return res.Guidelines, err
}

// SearchDetailed runs a hybrid knowledge search for the caller in the org. In order: an org without a single chunk
// answers nothing (cached count); the caller's embedder (memoised per scope and org); the census of the caller's
// identity, which answers ErrEmbedderMismatch when that identity sees no chunk of a corpus that has some; the
// scrubbed, truncated query's embedding (cached per caller, embedder and query); then the vector top-k at the
// relevance floor and the lexical top-k at the trigram floor, fused by reciprocal rank. Each phase's time is in the
// result, also when it failed.
func (s *KnowledgeService) SearchDetailed(ctx context.Context, scope, orgID, query string) (SearchResult, error) {
	return s.SearchLimited(ctx, scope, orgID, query, searchTopK)
}

// SearchLimited is SearchDetailed returning at most limit chunks (1 to searchTopK; anything else is searchTopK):
// the chat's search_knowledge tool asks for fewer when the model does.
func (s *KnowledgeService) SearchLimited(ctx context.Context, scope, orgID, query string, limit int) (res SearchResult, err error) {
	if limit <= 0 || limit > searchTopK {
		limit = searchTopK
	}
	start := time.Now()
	phase := func(name string) {
		now := time.Now()
		res.Phases = append(res.Phases, SearchPhase{Name: name, Took: now.Sub(start)})
		start = now
	}
	// An org without a single indexed chunk has nothing to retrieve: without this check every
	// org-flow chat turn would pay a billed embedding call only to find nothing (count is cached briefly).
	if s.store != nil && s.corpusEmpty(ctx, orgID) {
		phase(PhaseResolve)
		return res, nil
	}
	ctx = usageRun(ctx, scope, orgID)
	provider, err := s.resolvedEmbedder(ctx, scope, orgID)
	if err != nil {
		phase(PhaseResolve)
		return res, err
	}
	// Defense-in-depth: nil store (local mode) → empty augmentation instead of a panic; chat works
	// without org-guideline context.
	if s.store == nil {
		phase(PhaseResolve)
		return res, nil
	}
	// Only chunks this embedder produced (or legacy rows of unknown origin) are candidates: another model's vectors
	// at the same width are noise cosine cannot tell from a miss. When the census says there is not one, the query
	// embedding would be bought for nothing. A census that cannot be read lets the search run.
	identity := ai.EmbeddingIdentity(provider)
	if identity != "" {
		if counts, cerr := s.identityCounts(ctx, orgID, identity); cerr == nil && counts.Total > 0 && counts.Stranded == counts.Total {
			phase(PhaseResolve)
			recordRAGStrandedSearch()
			return res, ErrEmbedderMismatch
		}
	}
	phase(PhaseResolve)

	// Scrub the query before it leaves the process: the chat hands this path the RAW message, so the embeddings
	// request was the one place a pasted credential reached a provider verbatim; scrubbed before truncation so a cut
	// cannot hide half a secret from the patterns.
	query = scrubText(query)
	// Truncate before embedding: an oversized message would become a multi-million-token,
	// billed-per-token request above every provider's per-request cap.
	if utf8.RuneCountInString(query) > maxQueryRunes {
		query = string([]rune(query)[:maxQueryRunes])
	}
	vec, ok, err := s.embedQuery(ctx, provider, scope, orgID, query)
	phase(PhaseEmbed)
	if err != nil || !ok {
		return res, err
	}
	s.noteEmbeddingWidth(orgID, provider, [][]float32{vec})

	dense, err := s.store.SearchKnowledge(ctx, orgID, vec, limit, identity)
	if err != nil {
		phase(PhaseSearch)
		return res, err
	}
	// The lexical half (R-knowledge-8): exact action names, %Variables%, error codes and file names, which a dense
	// embedding represents poorly in a short question. It only adds: a failure leaves the dense hits.
	var lexical []interfaces.KnowledgeChunk
	if phrases := lexicalPhrases(query); len(phrases) > 0 {
		if lexical, err = s.store.SearchKnowledgeLexical(ctx, orgID, phrases, limit, identity); err != nil {
			if ctx.Err() != nil {
				phase(PhaseSearch)
				return res, err
			}
			logger.FromContext(ctx).Warn("knowledge: the lexical search failed; answering with the vector hits", "org_id", orgID, "error", err)
			lexical = nil
		}
	}
	phase(PhaseSearch)

	if chunks := fuseRanked(dense, lexical, limit); len(chunks) > 0 {
		res.Guidelines = assembleGuidelines(chunks)
		return res, nil
	}
	res.followUp = func(ctx context.Context) { s.explainEmptySearch(ctx, orgID, identity, len(vec)) }
	return res, nil
}

// rrfK is reciprocal rank fusion's constant: a rank-r hit scores 1/(rrfK+r) in each list that has it. 60 is the
// usual choice; it keeps a hit both lists rank above one list's first hit.
const rrfK = 60

// fuseRanked merges the dense and the lexical hits by reciprocal rank fusion: each chunk once, by its summed score,
// ties in the order first seen (the dense list's first), at most limit. Each list was cut at its own floor, so a
// dense-only hit passed the vector floor and a lexical-only hit the trigram floor.
func fuseRanked(dense, lexical []interfaces.KnowledgeChunk, limit int) []interfaces.KnowledgeChunk {
	type fused struct {
		chunk interfaces.KnowledgeChunk
		score float64
		seen  int
	}
	byID := map[string]*fused{}
	var order []*fused
	add := func(list []interfaces.KnowledgeChunk) {
		for rank, c := range list {
			f, ok := byID[c.ID]
			if !ok {
				f = &fused{chunk: c, seen: len(order)}
				byID[c.ID] = f
				order = append(order, f)
			}
			f.score += 1 / float64(rrfK+rank+1)
		}
	}
	add(dense)
	add(lexical)
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].score != order[j].score {
			return order[i].score > order[j].score
		}
		return order[i].seen < order[j].seen
	})
	out := make([]interfaces.KnowledgeChunk, 0, min(limit, len(order)))
	for _, f := range order[:min(max(limit, 0), len(order))] {
		out = append(out, f.chunk)
	}
	return out
}

// maxLexicalPhrases bounds the phrases one search hands the lexical statement: each is a probe of the trigram index,
// and each probe's candidates are scored.
const maxLexicalPhrases = 12

// lexicalStopWords are the words a phrase never starts or ends on: function words, and the words a question and the
// chat's selection description are made of rather than what a guideline names.
var lexicalStopWords = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`a an the and or but if then else of in on at to for from by with without about as into onto
		is are was were be been being do does did done have has had having i me my mine we us our you your it its they them
		their this that these those there here what which who whom whose why how when where can could should would will
		shall may might must not no yes ok okay please thanks thank so too very just also any all some each every more most
		other such only own same than now again still even get got make made need needs want wants like see tell explain
		show give let lets help fix check use using used work works working does doesn't don't isn't it's i'm what's
		selected selection block action subflow findings finding rule rules flow`) {
		m[w] = true
	}
	return m
}()

// quotedText finds quoted spans of at least two characters: "…", “…”, `…`, and '…' only between word breaks, so the
// apostrophes of "don't" and "it's" never pair up.
var quotedText = regexp.MustCompile(`"([^"]{2,80})"|“([^”]{2,80})”|(?:^|\s)'([^']{2,80})'(?:$|[\s.,;:!?)])` + "|`([^`]{2,80})`")

// lexicalPhrases picks the phrases of query a guideline would name verbatim, lowercased, at most maxLexicalPhrases:
// quoted text first, then identifier-like tokens (a digit, an inner '.', '_', '-', '%', '\' or '/', or a capital
// after the first letter: %Invoice_Total%, flow.pad, 0x80070005, a rule id, SharePoint, DOS), then runs of two, then
// three, words within a clause that neither start nor end on a stop word ("run dos command"). A lone common word is
// never a phrase: it would match half the corpus.
func lexicalPhrases(query string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.Join(strings.Fields(strings.ToLower(p)), " ")
		if utf8.RuneCountInString(p) < 3 || seen[p] || len(out) >= maxLexicalPhrases {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, m := range quotedText.FindAllStringSubmatch(query, -1) {
		for _, g := range m[1:] {
			if strings.TrimSpace(g) != "" {
				add(g)
			}
		}
	}

	// Clauses of words: a token's sentence punctuation ends its clause.
	var clauses [][]string
	var clause []string
	for _, raw := range strings.Fields(query) {
		tok, ends := trimToken(raw)
		if tok != "" {
			clause = append(clause, tok)
			if identifierLike(tok) {
				add(tok)
			}
		}
		if ends || tok == "" {
			if len(clause) > 0 {
				clauses = append(clauses, clause)
			}
			clause = nil
		}
	}
	if len(clause) > 0 {
		clauses = append(clauses, clause)
	}
	for _, n := range []int{2, 3} {
		for _, c := range clauses {
			for i := 0; i+n <= len(c); i++ {
				words := c[i : i+n]
				first, last := strings.ToLower(words[0]), strings.ToLower(words[n-1])
				if lexicalStopWords[first] || lexicalStopWords[last] || slices.ContainsFunc(words, func(w string) bool { return utf8.RuneCountInString(w) < 2 }) {
					continue
				}
				add(strings.Join(words, " "))
			}
		}
	}
	return out
}

// trimToken strips a token's surrounding punctuation and quotes, keeping a variable's '%' pair; ends reports a
// sentence or clause break after it.
func trimToken(raw string) (tok string, ends bool) {
	tok = strings.TrimLeftFunc(raw, func(r rune) bool { return r != '%' && !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	trimmed := strings.TrimRightFunc(tok, func(r rune) bool { return r != '%' && !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	ends = strings.ContainsAny(tok[len(trimmed):], ".,;:!?()[]{}")
	return trimmed, ends
}

// identifierLike reports a token a guideline would quote as is: one with a digit, an inner separator ('.', '_', '-',
// '%', '\', '/') or a capital after its first letter, at least three runes long.
func identifierLike(tok string) bool {
	if utf8.RuneCountInString(tok) < 3 {
		return false
	}
	for i, r := range tok {
		switch {
		case unicode.IsDigit(r), strings.ContainsRune("._-%\\/", r):
			return true
		case i > 0 && unicode.IsUpper(r):
			return true
		}
	}
	return false
}
