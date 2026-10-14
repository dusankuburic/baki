package redisx

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// BootOrigin is the name a process goes by in Redis: its instance id (PAD_INSTANCE_ID, the human-facing name in
// logs, traces and the admin page) and a random suffix chosen at boot, "pod-7#3fa91c0e". The SSE backplane's
// origin and subscriber field, the owner replica in a resume snapshot and an approval record, and the address of
// the replica's command channel all use it. Two replicas that were given the same PAD_INSTANCE_ID (a copied
// setting, a fixed hostname, host networking) are then still two origins, where a bare id made each take the other's
// events for its own, overwrite its subscriber record and answer its commands (REVIEW_REDIS M3). A restarted
// process is a new origin too, which is right: what the old one held is gone with it.
func BootOrigin(instance string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return instance + "#0"
	}
	return instance + "#" + hex.EncodeToString(b[:])
}

// OriginInstance is the instance id part of an origin, for a log line or a label.
func OriginInstance(origin string) string {
	instance, _, _ := strings.Cut(origin, "#")
	return instance
}
