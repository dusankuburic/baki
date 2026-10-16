package interfaces

import "time"

// PageCursor is a keyset position in a list ordered by a timestamp with the id breaking ties: the last row a page
// returned. The next page holds the rows strictly after it in the list's order, found through the list's index
// instead of re-reading every earlier row the way an OFFSET does, and unshifted by rows written in between.
type PageCursor struct {
	At time.Time
	ID string
	// Counted says the cursor carries the total its list's first page counted, Total (TotalCapped: it stopped at the
	// cap): a keyset page of a list that counts (ListFlowsPage) reports it instead of counting again, which read the
	// caller's whole visible set a second time on every page. A cursor issued without it leaves the page to count.
	Counted     bool
	Total       int
	TotalCapped bool
	// Skip is how many rows after the position the page passes over (ListFlowsPage, ListFlows): the rows the page
	// before ended on whose ids could not stand in a cursor, so its cursor names the last row before them.
	Skip int
}
