package analyzer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"pad-core/models"
	"pad-core/parser"
)

// sourceAliases walks v and returns the paths of every string whose bytes lie
// inside src's backing array.
//
// The tokenizer slices lines out of the source text without copying (by
// design: the parsed document legitimately shares it). A string that escapes
// into a long-lived cache — the analysis report cache, the suppression
// inventory cache — while still pointing into that array keeps the ENTIRE
// source alive (up to the 10 MiB body cap) for as long as the entry lives,
// however short the string is and long after the document itself is gone.
func sourceAliases(src string, v any) []string {
	lo := uintptr(unsafe.Pointer(unsafe.StringData(src)))
	hi := lo + uintptr(len(src))
	var out []string
	seen := map[uintptr]bool{}
	var walk func(path string, rv reflect.Value)
	walk = func(path string, rv reflect.Value) {
		switch rv.Kind() {
		case reflect.String:
			s := rv.String()
			if s == "" {
				return
			}
			if p := uintptr(unsafe.Pointer(unsafe.StringData(s))); p >= lo && p < hi {
				out = append(out, fmt.Sprintf("%s = %.40q", path, s))
			}
		case reflect.Pointer:
			if rv.IsNil() || seen[rv.Pointer()] {
				return
			}
			seen[rv.Pointer()] = true
			walk(path, rv.Elem())
		case reflect.Interface:
			if !rv.IsNil() {
				walk(path, rv.Elem())
			}
		case reflect.Struct:
			for i := 0; i < rv.NumField(); i++ {
				if rv.Type().Field(i).IsExported() {
					walk(path+"."+rv.Type().Field(i).Name, rv.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < rv.Len(); i++ {
				walk(fmt.Sprintf("%s[%d]", path, i), rv.Index(i))
			}
		case reflect.Map:
			iter := rv.MapRange()
			for iter.Next() {
				walk(fmt.Sprintf("%s{key}", path), iter.Key())
				walk(fmt.Sprintf("%s[%v]", path, iter.Key()), iter.Value())
			}
		}
	}
	walk("", reflect.ValueOf(v))
	return out
}

// TestCachedResults_DoNotPinTheSource guards the retention amplifier: every
// cached report and suppression inventory built from uploaded text held a
// substring of it (a parse-error snippet, a suppression's block label, ...),
// so each of the analysis cache's 50 reports and the inventory cache's 64
// entries could pin a whole uploaded source.
func TestCachedResults_DoNotPinTheSource(t *testing.T) {
	src := "#Region \"Main\"\n" +
		"# pad-ignore[parse-error]\n" +
		"WebAutomation.Click Element: 'btn'\n" +
		"ELSE\n" +
		"SET NewVar TO $'''value'''\n" +
		"FOREACH CurrentItem IN List\n" +
		"END\n" +
		"#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	if len(doc.ParseErrors) == 0 {
		t.Fatal("fixture rot: the source must produce a parse error")
	}
	settings := models.DefaultSettings()

	report := CachedAnalysis(doc, AllRules(), settings, nil)
	if len(report.Findings) == 0 {
		t.Fatal("fixture rot: the source must produce findings")
	}
	inventory := SuppressionInventoryCached(doc, AllRules(), settings)
	if len(inventory) == 0 {
		t.Fatal("fixture rot: the source must produce a suppression entry")
	}

	for name, v := range map[string]any{"report": report, "inventory": inventory} {
		if leaks := sourceAliases(src, v); len(leaks) > 0 {
			t.Errorf("the cached %s shares the source's backing array in %d string(s), pinning the whole source:\n  %s",
				name, len(leaks), strings.Join(leaks, "\n  "))
		}
	}
}
