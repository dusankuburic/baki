// Shared UI timing/behavior constants (U2.4). Extracted so the same concept
// isn't re-derived per component with drifting values — the findings search
// and library search used 150ms and 250ms debounces for the identical
// store-write pattern, and the fix-decision window was duplicated from the
// backend with only a comment tying them together.

// Input debounce for search-as-you-type → store write (the value that drives
// the heavy filter/regroup pipeline). Snappy for short queries; long enough
// to coalesce a fast typist's keystrokes.
export const SEARCH_DEBOUNCE_MS = 200

// Fix-approval card decision window. MUST mirror the backend's
// AI_FIX_DECISION_WINDOW (chat service): the backend timer is authoritative
// (its fix_decision SSE event resolves the card); this constant only renders
// the countdown so the timeout isn't a surprise. Update both together.
export const FIX_DECISION_WINDOW_S = 60

// Streaming tool-trail refresh interval. The SSE stream pushes chunks; the
// trail polls the slot at this cadence to batch UI updates cheaply.
export const TRAIL_REFRESH_MS = 400
