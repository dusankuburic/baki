package archtest

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestArch_ConfigNotMutatedOutsideConfig: config.Load builds the configuration once, and about thirty providers
// then share the one *config.Config by pointer, so a provider that writes to it changes what the others read,
// depending on the order fx constructs them in (the keystore once picked up an auth secret the auth manager had
// minted). Outside internal/config, non-test code must not write to anything reached from a configuration value:
//   - an assignment (=, op-assign) to, or ++/-- of, a field at any depth, a slice element, a map entry, or the
//     whole value through a dereferenced pointer;
//   - append, copy, delete or clear on such a slice or map;
//   - a pointer-receiver method called on such a field (config.Duration's UnmarshalText, say);
//   - taking the address of such a field (&cfg.Runtime), which hands out a pointer to write through out of sight
//     (ai.NewProviderFactory took &cfg.Runtime until it took the resilience settings it needs);
//   - a package-level variable of internal/config written the same ways.
//
// A configuration value is a *config.Config or any section type it holds (config.RuntimeConfig, …), however it
// was reached. Out of reach: a write through a local alias of a slice or map, or inside a function the value was
// passed to. config.Load hands out slices whose capacity is their length, so an append to an alias copies.
func TestArch_ConfigNotMutatedOutsideConfig(t *testing.T) {
	root := repoRoot(t)
	const configPkg = "pad-analyzer/internal/config"
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}, "pad-analyzer/...")
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the module does not type-check")
	}

	var cfgPkg *types.Package
	for _, p := range pkgs {
		if p.PkgPath == configPkg {
			cfgPkg = p.Types
		}
	}
	if cfgPkg == nil {
		t.Fatal("internal/config not loaded")
	}
	sections := configSections(t, cfgPkg)

	// isConfig reports whether t is a configuration value, or a pointer to one.
	isConfig := func(t types.Type) bool {
		if t == nil {
			return false
		}
		if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
			t = ptr.Elem()
		}
		named, ok := types.Unalias(t).(*types.Named)
		return ok && sections[named.Obj()]
	}

	var violations []string
	for _, p := range pkgs {
		if p.PkgPath == configPkg || strings.HasPrefix(p.PkgPath, configPkg+"/") {
			continue
		}
		info := p.TypesInfo
		// reaches reports whether writing to e writes to configuration: walking from e towards its root, some
		// selected field, indexed element, slice or dereference has a configuration value as its operand, or the
		// root is a package-level variable of internal/config.
		reaches := func(e ast.Expr) bool {
			for {
				switch x := e.(type) {
				case *ast.ParenExpr:
					e = x.X
				case *ast.SelectorExpr:
					sel := info.Selections[x]
					if sel == nil { // a qualified identifier: pkg.Var
						v, ok := info.Uses[x.Sel].(*types.Var)
						return ok && v.Pkg() == cfgPkg && v.Parent() == cfgPkg.Scope()
					}
					if sel.Kind() != types.FieldVal {
						return false
					}
					if isConfig(info.TypeOf(x.X)) {
						return true
					}
					e = x.X
				case *ast.IndexExpr:
					if isConfig(info.TypeOf(x.X)) {
						return true
					}
					e = x.X
				case *ast.SliceExpr:
					e = x.X
				case *ast.StarExpr:
					if isConfig(info.TypeOf(x.X)) {
						return true
					}
					e = x.X
				default:
					return false
				}
			}
		}

		for _, f := range p.Syntax {
			abs := p.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(abs, "_test.go") {
				continue
			}
			rel, _ := filepath.Rel(root, abs)
			rel = filepath.ToSlash(rel)
			report := func(n ast.Node, what string) {
				violations = append(violations, fmt.Sprintf("%s:%d: %s", rel, p.Fset.Position(n.Pos()).Line, what))
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.AssignStmt:
					if n.Tok == token.DEFINE {
						return true
					}
					for _, lhs := range n.Lhs {
						if reaches(lhs) {
							report(lhs, fmt.Sprintf("%s %s … writes to it", types.ExprString(lhs), n.Tok))
						}
					}
				case *ast.IncDecStmt:
					if reaches(n.X) {
						report(n, fmt.Sprintf("%s%s writes to it", types.ExprString(n.X), n.Tok))
					}
				case *ast.UnaryExpr:
					if n.Op == token.AND && reaches(n.X) {
						report(n, "&"+types.ExprString(n.X)+" hands out a pointer to write to it through")
					}
				case *ast.CallExpr:
					switch fun := ast.Unparen(n.Fun).(type) {
					case *ast.Ident:
						b, ok := info.Uses[fun].(*types.Builtin)
						if !ok || len(n.Args) == 0 {
							return true
						}
						switch b.Name() {
						case "append", "copy", "delete", "clear":
							if reaches(n.Args[0]) {
								rest := ""
								if len(n.Args) > 1 {
									rest = ", …"
								}
								report(n, fmt.Sprintf("%s(%s%s) writes to it", b.Name(), types.ExprString(n.Args[0]), rest))
							}
						}
					case *ast.SelectorExpr:
						sel := info.Selections[fun]
						if sel == nil || sel.Kind() != types.MethodVal {
							return true
						}
						sig, _ := sel.Obj().Type().(*types.Signature)
						if sig == nil || sig.Recv() == nil {
							return true
						}
						_, ptrRecv := sig.Recv().Type().(*types.Pointer)
						_, ptrOperand := types.Unalias(info.TypeOf(fun.X)).(*types.Pointer)
						// A pointer method on an addressable field takes its address implicitly.
						if ptrRecv && !ptrOperand && reaches(fun.X) {
							report(n, fmt.Sprintf("%s.%s(…), a pointer method, may write to it", types.ExprString(fun.X), fun.Sel.Name))
						}
					}
				}
				return true
			})
		}
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("%s: the configuration is filled in by config.Load, once; the providers sharing the one *config.Config only read it", v)
	}
}

// configSections is config.Config and every named struct type of internal/config it holds, at any depth: the
// types whose values are configuration.
func configSections(t *testing.T, pkg *types.Package) map[*types.TypeName]bool {
	t.Helper()
	root, ok := pkg.Scope().Lookup("Config").(*types.TypeName)
	if !ok {
		t.Fatal("config.Config not found")
	}
	out := map[*types.TypeName]bool{}
	var walk func(types.Type)
	walk = func(t types.Type) {
		switch x := types.Unalias(t).(type) {
		case *types.Pointer:
			walk(x.Elem())
		case *types.Slice:
			walk(x.Elem())
		case *types.Array:
			walk(x.Elem())
		case *types.Map:
			walk(x.Elem())
		case *types.Named:
			if x.Obj().Pkg() != pkg || out[x.Obj()] {
				return
			}
			st, ok := x.Underlying().(*types.Struct)
			if !ok {
				return
			}
			out[x.Obj()] = true
			for i := range st.NumFields() {
				walk(st.Field(i).Type())
			}
		}
	}
	walk(root.Type())
	if len(out) < 5 {
		t.Fatalf("found only %d configuration types: did config.Config change shape?", len(out))
	}
	return out
}
