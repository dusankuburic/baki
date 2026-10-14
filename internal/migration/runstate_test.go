package migration

import (
	"strings"
	"testing"
)

// A result with thousands of failures is stored bounded, not as an unbounded document.
func TestDetail_IsBoundedAndKeepsTheCounts(t *testing.T) {
	var res Result
	res.FlowsFailed = 20000
	for range 20000 {
		res.Errors = append(res.Errors, MigrationError{FlowID: "flow-with-a-long-identifier", Message: strings.Repeat("e", 200)})
	}
	doc := Detail(res)
	if len(doc) > DetailMax {
		t.Fatalf("detail is %d bytes, want at most %d", len(doc), DetailMax)
	}
	back, err := ParseDetail(doc)
	if err != nil {
		t.Fatalf("detail is not a result: %v", err)
	}
	if back.FlowsFailed != 20000 || len(back.Errors) == 0 || len(back.Errors) >= 20000 {
		t.Errorf("bounded detail: failed %d, %d errors kept, want the count kept and the list cut", back.FlowsFailed, len(back.Errors))
	}
	if back.Errors[0] != res.Errors[0] {
		t.Errorf("the first error is %+v, want the run's first", back.Errors[0])
	}
}

// A small result is stored whole.
func TestDetail_SmallResultRoundTrips(t *testing.T) {
	res := Result{FlowsMigrated: 3, SettingsMoved: true, NotMigrated: []string{"users"},
		Errors: []MigrationError{{FlowID: "f", Message: "bad"}}}
	back, err := ParseDetail(Detail(res))
	if err != nil || back.FlowsMigrated != 3 || !back.SettingsMoved || len(back.NotMigrated) != 1 || len(back.Errors) != 1 {
		t.Errorf("round trip = %+v, %v", back, err)
	}
}
