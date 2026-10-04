package search

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"

	"pad-core/models"
	"pad-core/parser"
)

// refCamelCaseRe and refTokenize are the pre-rewrite tokenizer, kept verbatim
// (minus the retention test seam) as the oracle the hand-written splitter must
// match token for token.
var refCamelCaseRe = regexp.MustCompile(`[A-Z][a-z]*|[a-z]+|[0-9]+`)

func refTokenize(text string) []string {
	if text == "" {
		return nil
	}
	var sb strings.Builder
	sb.Grow(len(text))
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		} else {
			sb.WriteRune(' ')
		}
	}
	words := strings.Fields(sb.String())
	var tokens []string
	seen := map[string]bool{}
	for _, word := range words {
		camelParts := refCamelCaseRe.FindAllString(word, -1)
		var parts []string
		if len(camelParts) > 1 {
			parts = camelParts
		} else if len(word) > 1 {
			parts = []string{word}
		}
		for _, part := range parts {
			lower := strings.ToLower(part)
			if len(lower) <= 1 {
				continue
			}
			if !seen[lower] {
				tokens = append(tokens, lower)
				seen[lower] = true
			}
		}
	}
	return tokens
}

// refIndexTokens is the pre-rewrite indexBlock's token map: tokenize every
// field, concatenate, then insert with the per-block "already listed" check.
func refIndexTokens(doc *models.FlowDocument) map[string][]string {
	tokens := map[string][]string{}
	var index func(block *models.Block)
	index = func(block *models.Block) {
		toks := refTokenize(block.Name)
		toks = append(toks, refTokenize(string(block.Type))...)
		toks = append(toks, refTokenize(block.RawType)...)
		for _, v := range block.Variables {
			toks = append(toks, refTokenize(v)...)
		}
		for _, val := range block.Properties {
			toks = append(toks, refTokenize(val)...)
		}
		for _, tok := range toks {
			list := tokens[tok]
			if len(list) == 0 || list[len(list)-1] != block.ID {
				tokens[tok] = append(list, block.ID)
			}
		}
		for i := range block.Children {
			index(&block.Children[i])
		}
	}
	for i := range doc.Subflows {
		for j := range doc.Subflows[i].Blocks {
			index(&doc.Subflows[i].Blocks[j])
		}
	}
	return tokens
}

// tokenizeCorpus returns hand-picked edge cases plus a large random corpus
// over an alphabet built to hit every branch of the splitter: ASCII case
// boundaries, acronyms, digits, separators (underscore, punctuation,
// whitespace), non-ASCII letters (including ones whose lower case changes
// byte length), non-ASCII digits, symbols and invalid UTF-8.
func tokenizeCorpus() []string {
	corpus := []string{
		"", "a", "A", "ab", "AB", "aB", "Ab", "HTTPServer", "parseHTTPResponse2xx", "getURLForID",
		"XMLHttpRequest", "IOError", "ABCdef", "abcDEF", "a1b2c3", "123abc456", "v2", "V2", "x86_64",
		"with_underscore-hyphen", "snake_case_name", "__init__", "WebAutomation.Click.Click",
		"%Var_Name%", "%NewVar%", "C:\\Users\\Bob\\file.txt", "https://example.com/a?b=c&d=1",
		"Ünïcödé Straße", "ÀBC", "İstanbul", "İ", "ǅungla", "Ωmega", "ΣΑΣ", "日本語テキスト", "日本Abc語",
		"٣٤٥ digits", "a٣b", "ⅫRoman", "\xff\xfeAbc", "Ab\xc3", "\xc3\xa9t\xc3\xa9", "é", "éa1", "Clické",
		"   spaces   ", "tab\tnew\nline", "emoji 🙂 Smile", "MiXeD123CaSe456", "ID", "Id", "iD",
	}
	alphabet := []string{
		"a", "b", "z", "A", "B", "Z", "0", "7", "9", " ", "_", "-", ".", "%", "\\", "/", ":", "\t",
		"é", "É", "ß", "ü", "İ", "ı", "Σ", "ς", "ǅ", "日", "本", "٣", "Ⅻ", "🙂", "\xff", "\xc3", "\u00a0",
		"HTTP", "Server", "get", "URL", "ID", "x2", "Abc",
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		n := rng.Intn(24)
		for j := 0; j < n; j++ {
			sb.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		corpus = append(corpus, sb.String())
	}
	// Real flow text: every line of every sample flow.
	files, _ := filepath.Glob("../parser/testdata/samples/*.txt")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		corpus = append(corpus, strings.Split(string(b), "\n")...)
	}
	return corpus
}

// TestTokenize_MatchesRegexpPipeline is the differential proof that the
// hand-written splitter is a drop-in for the regexp pipeline.
func TestTokenize_MatchesRegexpPipeline(t *testing.T) {
	corpus := tokenizeCorpus()
	if len(corpus) < 20000 {
		t.Fatalf("fixture rot: corpus has %d entries", len(corpus))
	}
	failures := 0
	for _, s := range corpus {
		got, want := tokenize(s), refTokenize(s)
		if !reflect.DeepEqual(got, want) {
			failures++
			if failures <= 10 {
				t.Errorf("tokenize(%q) = %q, regexp pipeline = %q", s, got, want)
			}
		}
	}
	if failures > 10 {
		t.Errorf("... %d mismatches in total", failures)
	}
}

func parseSamples(t testing.TB) []*models.FlowDocument {
	files, _ := filepath.Glob("../parser/testdata/samples/*.txt")
	if len(files) == 0 {
		t.Fatal("fixture rot: no sample flows found")
	}
	var docs []*models.FlowDocument
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := parser.ParseText(string(b), filepath.Base(f), int64(len(b)))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		docs = append(docs, doc)
	}
	return docs
}

// TestNewSearchIndex_MatchesReference compares the whole token map built by
// the streaming indexer against the pre-rewrite indexer on every sample flow
// plus a synthetic doc with nested children and shared tokens.
func TestNewSearchIndex_MatchesReference(t *testing.T) {
	docs := append(parseSamples(t), makeTestDoc())
	docs = append(docs, &models.FlowDocument{ID: "nested", Subflows: []models.Subflow{{ID: "sf", Blocks: []models.Block{{
		ID: "p", Name: "Loop over HTTPServer rows", Type: models.BlockTypeLoop, RawType: "LOOP",
		Variables:  []string{"RowIndex", "row_count"},
		Properties: map[string]string{"From": "1", "To": "%RowCount%", "Note": "Ünïcödé ÀBC 日本"},
		Children: []models.Block{
			{ID: "c1", Name: "Click row", Type: models.BlockTypeAction, RawType: "WebAutomation.Click"},
			{ID: "p", Name: "duplicate id reuses parent", Type: models.BlockTypeAction, RawType: "Variables.SetVariable"},
		},
	}}}}})
	for _, doc := range docs {
		got := NewSearchIndex(doc.ID, doc).tokens
		want := refIndexTokens(doc)
		if !reflect.DeepEqual(got, want) {
			for k, v := range want {
				if !reflect.DeepEqual(got[k], v) {
					t.Errorf("%s: token %q -> %v, reference %v", doc.Name, k, got[k], v)
					break
				}
			}
			for k := range got {
				if _, ok := want[k]; !ok {
					t.Errorf("%s: extra token %q", doc.Name, k)
					break
				}
			}
			t.Errorf("%s: token map differs from the reference indexer (%d vs %d keys)", doc.Name, len(got), len(want))
		}
	}
}

// TestNewSearchIndex_AllocationBound pins the index build's allocation
// count on the largest sample flow. The regexp pipeline (one FindAllString
// slice per word, a scratch copy and Fields slice per field, a token slice
// per field and the pooled seen-map churn) allocated several times per
// indexed field; the streaming builder allocates only for new map entries
// and list growth.
func TestNewSearchIndex_AllocationBound(t *testing.T) {
	b, err := os.ReadFile("../parser/testdata/samples/synthetic_10k.txt")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parser.ParseText(string(b), "synthetic_10k.txt", int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	fields := 0
	var count func(bs []models.Block)
	count = func(bs []models.Block) {
		for i := range bs {
			fields += 3 + len(bs[i].Variables) + len(bs[i].Properties)
			count(bs[i].Children)
		}
	}
	for i := range doc.Subflows {
		count(doc.Subflows[i].Blocks)
	}
	allocs := testing.AllocsPerRun(3, func() { _ = NewSearchIndex(doc.ID, doc) })
	// Measured on this flow: the streaming builder ~1.7 allocations per
	// indexed field (new keys, list growth, map growth); the regexp pipeline
	// ~15.
	if limit := float64(3 * fields); allocs > limit {
		t.Errorf("NewSearchIndex: %.0f allocations for %d indexed fields, want <= %.0f (three per field)", allocs, fields, limit)
	}
	t.Logf("NewSearchIndex: %.0f allocations, %d fields", allocs, fields)
}

func BenchmarkNewSearchIndex(b *testing.B) {
	src, err := os.ReadFile("../parser/testdata/samples/synthetic_10k.txt")
	if err != nil {
		b.Fatal(err)
	}
	doc, err := parser.ParseText(string(src), "synthetic_10k.txt", int64(len(src)))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = NewSearchIndex(doc.ID, doc)
	}
}
