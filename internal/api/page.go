package api

import "encoding/json"

// Page sizes of a request that embeds PageRequest, unless its endpoint sets others (PageLimits).
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// PageRequest is the paging part of a list request; embed it by value. The endpoint bounds Limit before the
// request is validated, so a list never runs unbounded: no limit, or one below 1, becomes the default, and one
// above the maximum becomes the maximum. Cursor is the previous page's NextCursor, or "" for the first page. It is
// opaque to the client, and the endpoint refuses a cursor it did not issue (docs/architecture/api-conventions.md).
type PageRequest struct {
	Limit  int    `json:"limit,omitempty" query:"limit"`
	Cursor string `json:"cursor,omitempty" query:"cursor"`
}

// pageRequester is a request that embeds PageRequest.
type pageRequester interface {
	pageRequest() *PageRequest
}

func (p *PageRequest) pageRequest() *PageRequest { return p }

// bound applies an endpoint's page sizes to the requested limit.
func (p *PageRequest) bound(defaultLimit, maxLimit int) {
	switch {
	case p.Limit < 1:
		p.Limit = defaultLimit
	case p.Limit > maxLimit:
		p.Limit = maxLimit
	}
}

// Page is the response of a paged list. Items is never null on the wire. NextCursor is the opaque position of the
// next page and is omitted on the last one. Total is the size of the whole list, omitted when the endpoint does
// not count it.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	Total      *int   `json:"total,omitempty"`
}

// MarshalJSON writes nil Items as an empty array.
func (p Page[T]) MarshalJSON() ([]byte, error) {
	if p.Items == nil {
		p.Items = []T{}
	}
	// The same fields without the method, so encoding does not come back here.
	return json.Marshal(struct {
		Items      []T    `json:"items"`
		NextCursor string `json:"nextCursor,omitempty"`
		Total      *int   `json:"total,omitempty"`
	}(p))
}
