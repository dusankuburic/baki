package archtest

import (
	"go/ast"
	"go/types"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// TestArch_HandlerStoreHandleCallsRatchet is TestArch_HandlerStorageCallsRatchet for the store handles that are not
// called `backend`: a refresh-token store, an identity store, a capability asserted out of the backend, raw
// database/sql. It needs types, so it type-checks internal/api. A call counts when its receiver is
//   - a type of database/sql;
//   - a type declared under internal/storage, through a method taking a context (a stored value's own helpers,
//     such as a cursor's IsZero, don't count);
//   - an interface the Postgres backend implements, through a method taking a context (an api-declared slice of
//     the backend, such as RefreshTokenStore).
//
// Calls the backend ratchet already counts (receiver `backend` or `x.backend`) are left to it.
func TestArch_HandlerStoreHandleCallsRatchet(t *testing.T) {
	root := repoRoot(t)
	const (
		apiPkg      = "pad-analyzer/internal/api"
		databasePkg = "pad-analyzer/internal/storage/database"
	)
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo,
		Dir:  root,
	}
	pkgs, err := packages.Load(cfg, apiPkg+"/...", databasePkg)
	if err != nil {
		t.Fatal(err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		t.Fatal("internal/api does not type-check")
	}
	var postgres types.Type
	for _, p := range pkgs {
		if p.PkgPath == databasePkg {
			if obj := p.Types.Scope().Lookup("PostgresStorageBackend"); obj != nil {
				postgres = types.NewPointer(obj.Type())
			}
		}
	}
	if postgres == nil {
		t.Fatal("database.PostgresStorageBackend not found")
	}
	postgresMethods := types.NewMethodSet(postgres)
	implementedByPostgres := func(iface *types.Interface) bool {
		for i := range iface.NumMethods() {
			m := iface.Method(i)
			sel := postgresMethods.Lookup(m.Pkg(), m.Name())
			if sel == nil || types.TypeString(sel.Type(), nil) != types.TypeString(m.Type(), nil) {
				return false
			}
		}
		return iface.NumMethods() > 0
	}
	takesContext := func(sig *types.Signature) bool {
		if sig.Params().Len() == 0 {
			return false
		}
		named, ok := types.Unalias(sig.Params().At(0).Type()).(*types.Named)
		return ok && named.Obj().Pkg() != nil && named.Obj().Pkg().Path() == "context" && named.Obj().Name() == "Context"
	}
	storeHandle := func(sel *types.Selection) bool {
		recv := sel.Recv()
		if ptr, ok := recv.(*types.Pointer); ok {
			recv = ptr.Elem()
		}
		named, ok := recv.(*types.Named)
		if !ok || named.Obj().Pkg() == nil {
			return false
		}
		path := named.Obj().Pkg().Path()
		sig, _ := sel.Type().(*types.Signature)
		switch {
		case path == "database/sql":
			return true
		case sig == nil || !takesContext(sig):
			return false
		case path == "pad-analyzer/internal/storage" || strings.HasPrefix(path, "pad-analyzer/internal/storage/"):
			return true
		}
		iface, ok := named.Underlying().(*types.Interface)
		return ok && implementedByPostgres(iface)
	}
	countedByBackendRatchet := func(sel *ast.SelectorExpr) bool {
		switch x := sel.X.(type) {
		case *ast.SelectorExpr:
			return x.Sel.Name == "backend"
		case *ast.Ident:
			return x.Name == "backend"
		}
		return false
	}

	got := map[string]int{}
	for _, p := range pkgs {
		if p.PkgPath != apiPkg && !strings.HasPrefix(p.PkgPath, apiPkg+"/") {
			continue
		}
		for _, f := range p.Syntax {
			abs := p.Fset.Position(f.Pos()).Filename
			if strings.HasSuffix(abs, "_test.go") {
				continue
			}
			rel, _ := filepath.Rel(root, abs)
			rel = filepath.ToSlash(rel)
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				fun, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || countedByBackendRatchet(fun) {
					return true
				}
				if sel := p.TypesInfo.Selections[fun]; sel != nil && sel.Kind() == types.MethodVal && storeHandle(sel) {
					got[rel]++
				}
				return true
			})
		}
	}
	checkRatchet(t, "store-handle calls in internal/api", got, handlerStoreHandleCalls)
}

// handlerStoreHandleCalls pins the store-handle calls left per file. The infrastructure ones stay: the request
// transaction (rls_middleware.go), the Idempotency-Key store, PAT authentication (router.go), the migration lock
// and the health probes; the rest move into services.
var handlerStoreHandleCalls = map[string]int{
	"internal/api/handlers_system.go":  2, // health probe (Redis): infrastructure
	"internal/api/idempotency.go":      2, // the Idempotency-Key store: infrastructure
	"internal/api/migration_runner.go": 1, // the migration's global lock: infrastructure
	"internal/api/router.go":           3, // PAT authentication: infrastructure
}
