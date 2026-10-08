package database

import (
	"reflect"
	"testing"
)

// TestSQLArgs_PlaceholdersFollowTheArguments: add numbers placeholders by position, after any arguments the slice
// already holds, and the slice spreads as the statement's args.
func TestSQLArgs_PlaceholdersFollowTheArguments(t *testing.T) {
	args := sqlArgs{"alert-1"}
	if p := args.add("u-1"); p != "$2" {
		t.Errorf("first add after one argument = %s, want $2", p)
	}
	if p := args.add(10); p != "$3" {
		t.Errorf("second add = %s, want $3", p)
	}
	spread := func(vs ...any) []any { return vs }
	if got := spread(args...); !reflect.DeepEqual(got, []any{"alert-1", "u-1", 10}) {
		t.Errorf("args = %v", got)
	}

	var empty sqlArgs
	if p := empty.add(nil); p != "$1" || len(empty) != 1 {
		t.Errorf("add on a nil sqlArgs = %s (len %d), want $1 (len 1)", p, len(empty))
	}
}
