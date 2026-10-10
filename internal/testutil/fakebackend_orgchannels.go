package testutil

import (
	"maps"
	"slices"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
)

// copyOrgChannel is ch with its maps and slices copied, so neither the caller nor the store sees the other's later
// changes (the Postgres store hands out fresh values on every read). Unreadable is a read's verdict, never stored:
// a test sets it on the stored channel to stand for a row no configured key opens, and a save clears it, as a
// re-save under the current key does.
func copyOrgChannel(ch *interfaces.OrgChannel) *interfaces.OrgChannel {
	cp := *ch
	cp.Config, cp.Secrets, cp.Events = maps.Clone(ch.Config), maps.Clone(ch.Secrets), slices.Clone(ch.Events)
	return &cp
}

// eraseChannelRecipientLocked is the erase step of org_channels: the user leaves every email channel's recipients
// (comma-separated member ids).
func (m *FakeBackend) eraseChannelRecipientLocked(userID string) {
	for _, ch := range m.OrgChannels {
		ids := strings.Split(ch.Config["recipients"], ",")
		if !slices.Contains(ids, userID) {
			continue
		}
		ch.Config = maps.Clone(ch.Config)
		ch.Config["recipients"] = strings.Join(slices.DeleteFunc(ids, func(id string) bool { return id == userID }), ",")
	}
}
