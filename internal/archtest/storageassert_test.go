package archtest

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestArch_StorageTypeAssertionsRatchet: a capability found by type-asserting a storage value is invisible in the
// signature that depends on it, and the branch for a value without it rots. Phase 3 folded the domain capabilities
// into the role interfaces and turned the database-only ones into fx values (docs/adr/0010), so new code asks for
// what it needs. The assertions left outside the storage packages are pinned per file. An assertion (or type switch)
// counts when the asserted value's static type or an asserted type is a store: an interface declared under
// internal/storage, a type declared there with a method taking a context, or an interface of this module that a
// type of the database package implements (its keystore's methods take no context). Stored values (a FlowDocument
// in a context) are not stores.
func TestArch_StorageTypeAssertionsRatchet(t *testing.T) {
	root := repoRoot(t)
	const (
		module      = "pad-analyzer"
		storagePkg  = module + "/internal/storage"
		databasePkg = storagePkg + "/database"
	)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}
	pkgs, err := packages.Load(cfg, module+"/...")
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("the module does not type-check")
	}
	underStorage := func(path string) bool { return path == storagePkg || strings.HasPrefix(path, storagePkg+"/") }

	// isStore: a concrete type with a method taking a context (by pointer, so pointer methods count).
	isStore := func(named *types.Named) bool {
		mset := types.NewMethodSet(types.NewPointer(named))
		for i := range mset.Len() {
			sig, _ := mset.At(i).Type().(*types.Signature)
			if sig != nil && sig.Params().Len() > 0 {
				if p, ok := types.Unalias(sig.Params().At(0).Type()).(*types.Named); ok && p.Obj().Pkg() != nil &&
					p.Obj().Pkg().Path() == "context" && p.Obj().Name() == "Context" {
					return true
				}
			}
		}
		return false
	}
	// The database package's types with methods, by pointer: what an interface of the module must be implemented
	// by to count.
	var databaseStores []types.Type
	for _, p := range pkgs {
		if p.PkgPath != databasePkg {
			continue
		}
		scope := p.Types.Scope()
		for _, name := range scope.Names() {
			if tn, ok := scope.Lookup(name).(*types.TypeName); ok && tn.Exported() && !tn.IsAlias() {
				if named, ok := tn.Type().(*types.Named); ok && !types.IsInterface(named) && named.NumMethods() > 0 {
					databaseStores = append(databaseStores, types.NewPointer(named))
				}
			}
		}
	}
	if len(databaseStores) == 0 {
		t.Fatal("no types with methods found in the database package")
	}
	storageType := func(typ types.Type) bool {
		if typ == nil {
			return false
		}
		if ptr, ok := typ.(*types.Pointer); ok {
			typ = ptr.Elem()
		}
		named, ok := types.Unalias(typ).(*types.Named)
		if !ok || named.Obj().Pkg() == nil {
			return false
		}
		path := named.Obj().Pkg().Path()
		iface, isIface := named.Underlying().(*types.Interface)
		switch {
		case underStorage(path):
			return isIface || isStore(named)
		case !isIface || iface.NumMethods() == 0 || (path != module && !strings.HasPrefix(path, module+"/")):
			return false
		}
		for _, store := range databaseStores {
			if types.Implements(store, iface) {
				return true
			}
		}
		return false
	}

	got := map[string]int{}
	for _, p := range pkgs {
		if underStorage(p.PkgPath) {
			continue
		}
		for _, f := range p.Syntax {
			abs := p.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(abs, "_test.go") {
				continue
			}
			rel, _ := filepath.Rel(root, abs)
			rel = filepath.ToSlash(rel)
			typeOf := func(e ast.Expr) types.Type {
				if e == nil {
					return nil
				}
				return p.TypesInfo.TypeOf(e)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.TypeAssertExpr:
					if n.Type != nil && (storageType(typeOf(n.X)) || storageType(typeOf(n.Type))) {
						got[rel]++
					}
				case *ast.TypeSwitchStmt:
					var x ast.Expr
					switch s := n.Assign.(type) {
					case *ast.AssignStmt:
						x = s.Rhs[0].(*ast.TypeAssertExpr).X
					case *ast.ExprStmt:
						x = s.X.(*ast.TypeAssertExpr).X
					}
					counts := storageType(typeOf(x))
					for _, c := range n.Body.List {
						for _, e := range c.(*ast.CaseClause).List {
							counts = counts || storageType(typeOf(e))
						}
					}
					if counts {
						got[rel]++
					}
				}
				return true
			})
		}
	}
	checkRatchet(t, "type assertions on storage values", got, storageTypeAssertions)
}

// storageTypeAssertions pins the type assertions on storage values left outside the storage packages: none. The
// organisation service's store is a StorageBackend role (OrgStore, OrgInviteStore), and the keystore's capabilities
// are fx values of provideKeyStore (the organisation credential store with its key families and restorer, the
// personal key-family lister), nil with the OS keyring.
var storageTypeAssertions = map[string]int{}
