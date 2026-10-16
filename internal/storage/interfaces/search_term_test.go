package interfaces

import (
	"encoding/json"
	"os"
	"testing"
)

// searchTermCases is the table the web client's rule is tested on too (frontend/src/lib/searchTerm.test.ts), so the
// two cannot drift apart.
const searchTermCases = "../../../frontend/src/lib/searchTerm.cases.json"

// TestShortSearchTerm: a term is long enough to search by substring only with a run of three word characters, the
// shortest word pg_trgm draws a trigram from inside a LIKE pattern. Word characters are what glibc's iswalnum
// counts, which pg_trgm uses: letters, the vowel signs and other marks Unicode calls alphabetic (so नाम and สวัสดี
// are one word each), letter numbers and decimal digits of any script. Punctuation, spaces, underscores, symbols
// and the other combining marks split words. Blank is not short: it does not filter.
func TestShortSearchTerm(t *testing.T) {
	raw, err := os.ReadFile(searchTermCases)
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Term  string `json:"term"`
		Short bool   `json:"short"`
		Why   string `json:"why"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 50 {
		t.Fatalf("%d cases in %s; want the whole table", len(cases), searchTermCases)
	}
	for _, tc := range cases {
		if got := ShortSearchTerm(tc.Term); got != tc.Short {
			t.Errorf("ShortSearchTerm(%q) = %v, want %v (%s)", tc.Term, got, tc.Short, tc.Why)
		}
	}
}

// TestUserQuery_ShortTermMatchesAPrefix: the admin user search falls back to a prefix for a term without a run of
// three letters or digits, as for one under three characters, and for any term its Prefix asks for.
func TestUserQuery_ShortTermMatchesAPrefix(t *testing.T) {
	u := &User{Email: "x---y@example.com", DisplayName: "--- ops"}
	if !(UserQuery{Q: "---"}).Matches(u) {
		t.Error(`"---" does not match the display name it starts`)
	}
	if (UserQuery{Q: "---"}).Matches(&User{Email: "x---y@example.com"}) {
		t.Error(`"---" matched the inside of an email; want a prefix match`)
	}
	if !(UserQuery{Q: "example"}).Matches(u) {
		t.Error(`"example" does not match the email it is in`)
	}
	if (UserQuery{Q: "example", Prefix: true}).Matches(u) {
		t.Error(`"example" with Prefix matched the inside of an email; want a prefix match`)
	}
	if !(UserQuery{Q: "x---", Prefix: true}).Matches(u) {
		t.Error(`"x---" with Prefix does not match the email it starts`)
	}
}
