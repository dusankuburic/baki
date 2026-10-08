package interfaces

import (
	"fmt"
	"strings"
)

// ── Flow-tag normalization ───────────────────────────────────────────
// Shared by every backend: lowercase letters/digits/'-'/'_', ≤32 chars each, ≤20 per flow, deduped.

const (
	MaxFlowTags   = 20
	MaxFlowTagLen = 32
)

// NormalizeFlowTags canonicalizes a tag list: trim, lowercase, drop empties and duplicates, enforce the whitelist and caps; returns an error naming the first rejected tag.
func NormalizeFlowTags(tags []string) ([]string, error) {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if len(tag) > MaxFlowTagLen {
			return nil, fmt.Errorf("tag %q exceeds %d characters", raw, MaxFlowTagLen)
		}
		for _, r := range tag {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			default:
				return nil, fmt.Errorf("tag %q may only contain letters, digits, '-' and '_'", raw)
			}
		}
		if seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) > MaxFlowTags {
		return nil, fmt.Errorf("a flow carries at most %d tags", MaxFlowTags)
	}
	return out, nil
}

// SplitFlowTags parses the CSV tags column; empty ⇒ nil (untagged).
func SplitFlowTags(csv string) []string {
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
