package archtest

import (
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Redis keys are declared (ADR 0029): a package-level redisx.Declare names the key family, its pattern, TTL, payload
// version and on-down behaviour, and its owner builds names with the declaration (Key, Glob, Prefix). That keeps the
// golden (internal/di/testdata/redis_keys.golden) a complete list of what two releases share in a rolling deploy.
// These ratchets keep the literals inside the declarations.

const redisxPath = "pad-analyzer/internal/redisx"

// redisxDeclareCalls counts the redisx.Declare calls of f.
func redisxDeclareCalls(f goFile) int {
	names := importNames(f.ast, func(p string) (string, bool) { return "redisx", p == redisxPath })
	return countSelectors(f.ast, names, func(_, sel string) bool { return sel == "Declare" })
}

// declaredPatterns returns the literal Pattern: fields of the redisx.Spec values in f (and, inside the package
// itself, of its Spec values).
func declaredPatterns(f goFile) []string {
	inRedisx := strings.HasPrefix(f.path, "internal/redisx/")
	var out []string
	ast.Inspect(f.ast, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		switch t := cl.Type.(type) {
		case *ast.SelectorExpr:
			if x, ok := t.X.(*ast.Ident); !ok || x.Name != "redisx" || t.Sel.Name != "Spec" {
				return true
			}
		case *ast.Ident:
			if !inRedisx || t.Name != "Spec" {
				return true
			}
		default:
			return true
		}
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Pattern" {
				continue
			}
			if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				v, _ := strconv.Unquote(lit.Value)
				out = append(out, v)
			}
		}
		return true
	})
	return out
}

// TestArch_RedisDeclarationsRatchet pins the files that declare Redis keys. A new Redis user adds its declaration
// here on purpose, with the golden refreshed: that is the review point for a new key family. (internal/redisx's
// own declarations are not counted.)
func TestArch_RedisDeclarationsRatchet(t *testing.T) {
	got := map[string]int{}
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/redisx/") {
			continue
		}
		if n := redisxDeclareCalls(f); n > 0 {
			got[f.path] = n
		}
	}
	checkRatchet(t, "redisx.Declare calls (a new key family: declare it, refresh the redis_keys golden, pin it here)", got, redisDeclarations)
}

// redisDeclarations is the number of key families each file declares.
var redisDeclarations = map[string]int{
	"internal/api/middleware/ratelimit.go": 1, // ratelimit.bucket (the per-user group is a group of it)
	"internal/service/chat_commands.go":    5, // chat.cmd, chat.reply, chat.approval, chat.decision, chat.active (ADR 0033)
	"internal/api/ws_ticket.go":            1, // ws.ticket
	"internal/invalidation/fanout.go":      1, // inval.fanout
	"internal/service/chat_resume.go":      1, // chat.resume
	"internal/service/flow_upload_ids.go":  1, // flow.upload-id
	"internal/websocket/backplane.go":      2, // ws.room, ws.presence
}

// TestArch_RedisKeyLiteralsStayInDeclarations: no file outside internal/redisx and the files that declare a key
// family spells the start of a declared pattern ("ws:room:", "ratelimit:", ...) as a string literal. A key built by
// hand is a key the golden and the TTL rule do not see. The prefixes are read from the declarations themselves, so
// a family declared later is covered without touching this test.
func TestArch_RedisKeyLiteralsStayInDeclarations(t *testing.T) {
	files := sourceFiles(t)
	prefixSet := map[string]bool{}
	for _, f := range files {
		for _, p := range declaredPatterns(f) {
			if i := strings.IndexByte(p, '{'); i >= 0 {
				p = p[:i]
			}
			if p != "" {
				prefixSet[p] = true
			}
		}
	}
	if len(prefixSet) == 0 {
		t.Fatal("no declared patterns found: the AST walk is broken")
	}
	var prefixes []string
	for p := range prefixSet {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)

	got := map[string]int{}
	for _, f := range files {
		if strings.HasPrefix(f.path, "internal/redisx/") || redisxDeclareCalls(f) > 0 {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, p := range prefixes {
				if strings.HasPrefix(v, p) {
					got[f.path]++
					break
				}
			}
			return true
		})
	}
	checkRatchet(t, "Redis key literals outside the declarations (build the name with the redisx declaration)", got, redisKeyLiterals)
}

// redisKeyLiterals pins the literals that cannot move: internal/auth must not import redisx (it stays free of the
// Redis dependency and defines the publisher interfaces redisx implements), so it spells its channel once;
// TestRedisKeys_AuthConstantMatches in internal/di holds the two spellings together.
var redisKeyLiterals = map[string]int{
	"internal/auth/cached_blacklist.go": 1, // RevocationChannel
}
