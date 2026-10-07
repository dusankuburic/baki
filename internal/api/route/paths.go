package route

import (
	"net/url"
	"path"
	"strings"
)

// DispatchPath returns the path chi dispatches a request for u on: RawPath when it is set, which chi routes
// verbatim, else the cleaned Path (Router.ServeHTTP cleans Path, never RawPath), with the /api/v1 alias stripped
// from either (the router's alias middleware). Every policy decision about a request must be made on this path.
// Deciding on the cleaned, decoded Path let /api/system/settings/org/..%2F..%2Fhealth read as the public
// /api/health while chi ran the protected route. path.Clean("") is ".", which is what chi then sees.
func DispatchPath(u *url.URL) string {
	if u.RawPath != "" {
		return StripVersionAlias(u.RawPath)
	}
	return CanonicalPath(u.Path)
}

// CanonicalPath returns p as the router leaves a decoded path: path.Clean (Router.ServeHTTP), then the /api/v1
// alias strip, in that order. It is the path the NotFound handler serves static files and the SPA shell from.
func CanonicalPath(p string) string {
	return StripVersionAlias(path.Clean(p))
}

// StripVersionAlias maps the /api/v1 alias to /api, as a whole segment only ("/api/v1foo" is a path of its own).
// Applied alone to a RawPath, which must not be cleaned: path.Clean cannot tell an escaped "/" from a separator.
func StripVersionAlias(p string) string {
	if rest, ok := strings.CutPrefix(p, "/api/v1/"); ok {
		return "/api/" + rest
	}
	if p == "/api/v1" {
		return "/api"
	}
	return p
}
