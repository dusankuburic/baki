package redisx

import (
	"sync"
	"time"

	"pad-core/logger"
)

// LimitedWarnInterval is the least time between two lines of one key.
const LimitedWarnInterval = 30 * time.Second

var (
	// warnSink and warnNow are LimitedWarn's output and clock; tests replace them.
	warnSink = logger.Warn
	warnNow  = time.Now

	warnState = struct {
		sync.Mutex
		m map[string]*warnEntry
	}{m: map[string]*warnEntry{}}
)

type warnEntry struct {
	last       time.Time
	suppressed int
}

// LimitedWarn logs a Redis failure at most once per LimitedWarnInterval for key, with how many it held back since
// the last line. key names the mechanism or call site and must come from a closed set (a constant of the caller):
// it is the table's key. The line carries the error's class, never its text, which includes the server's address.
// With Redis down for an hour and a collaborator on every room, an unthrottled line per broadcast and per heartbeat
// floods the log pipeline without telling the operator anything one line does not (REVIEW_COMPAT M3).
func LimitedWarn(key, msg string, err error, kv ...any) {
	now := warnNow()
	warnState.Lock()
	e, ok := warnState.m[key]
	if !ok {
		e = &warnEntry{}
		warnState.m[key] = e
	}
	if !e.last.IsZero() && now.Sub(e.last) < LimitedWarnInterval {
		e.suppressed++
		warnState.Unlock()
		return
	}
	suppressed := e.suppressed
	e.last, e.suppressed = now, 0
	warnState.Unlock()

	if err != nil {
		kv = append(kv, "class", errorClass(err))
	}
	if suppressed > 0 {
		kv = append(kv, "suppressed", suppressed)
	}
	warnSink(msg, kv...)
}

// ResetLimitedWarns forgets every key's history, so a test sees the first line of a key as the first. Production code
// never calls it.
func ResetLimitedWarns() {
	warnState.Lock()
	warnState.m = map[string]*warnEntry{}
	warnState.Unlock()
}
