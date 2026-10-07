package audit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestAllActions_CoversEveryConstant parses actions.go and checks that every Action* constant is listed by
// AllActions(). The admin audit UI builds its action filter from that list; a constant missing here is an
// action the UI can never filter for (which is exactly how 28 of 35 actions went unfilterable before).
func TestAllActions_CoversEveryConstant(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "actions.go", nil, 0)
	if err != nil {
		t.Fatalf("parse actions.go: %v", err)
	}
	listed := map[string]bool{}
	for _, a := range AllActions() {
		if listed[a] {
			t.Errorf("AllActions lists %q twice", a)
		}
		listed[a] = true
	}
	var constants int
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				constants++
				val := strings.Trim(lit.Value, `"`)
				if !listed[val] {
					t.Errorf("%s = %q is not returned by AllActions()", name.Name, val)
				}
			}
		}
	}
	if constants == 0 {
		t.Fatalf("no Action constants found — did the parser target the wrong file?")
	}
	if len(listed) != constants {
		t.Errorf("AllActions has %d entries but actions.go declares %d constants", len(listed), constants)
	}
}
