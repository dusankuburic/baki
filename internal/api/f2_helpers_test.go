package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
)

// asCaller serves h with callerID as every request's actor, as jwtAuth installs it without accounts, for tests that
// serve handlers without the Router's middleware.
func asCaller(callerID string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(auth.WithActor(r.Context(), auth.LocalActor(callerID))))
	})
}

// mountedAs serves m's routes as the Router mounts them, their route middleware included (the policy guard and the
// Router's own layers are not), with callerID as the actor and the request's reqinfo.Info the outer chain installs.
func mountedAs(m Module, callerID string) http.Handler {
	mux := chi.NewRouter()
	m.Mount(route.NewBuilder(mux))
	return middleware.RequestInfo(nil)(asCaller(callerID, mux))
}
