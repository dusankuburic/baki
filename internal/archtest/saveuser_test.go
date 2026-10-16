package archtest

import (
	"strings"
	"testing"
)

// TestArch_SaveUserOnlyInStorageAndTests: SaveUser writes every column of a user, so a caller that read the row
// first puts back whatever changed in between. The failed-login path did that (a password reset racing a wrong
// password got its old hash back, and parallel attempts never reached the lock). Production code changes an account
// through the targeted updates (RecordFailedLogin, UpdateUserPassword, UpdateUserRole, UpdateUserAvatarURL, ...);
// the storage packages, which implement and contract-test SaveUser, and the test doubles may call it.
func TestArch_SaveUserOnlyInStorageAndTests(t *testing.T) {
	for _, f := range sourceFiles(t) {
		if strings.HasPrefix(f.path, "internal/storage/") || strings.HasPrefix(f.path, "internal/testutil/") {
			continue
		}
		if n := countCalls(f.ast, methodCall("SaveUser")); n > 0 {
			t.Errorf("%s calls SaveUser %d times; change the account through a targeted update instead", f.path, n)
		}
	}
}
