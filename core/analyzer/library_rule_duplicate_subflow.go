package analyzer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"pad-core/models"
)

// DuplicateSubflowAcrossFlowsRule is the reference library rule: a subflow
// whose actions are identical to a subflow in another flow.
type DuplicateSubflowAcrossFlowsRule struct{}

func (r *DuplicateSubflowAcrossFlowsRule) ID() string   { return "duplicate-subflow-across-flows" }
func (r *DuplicateSubflowAcrossFlowsRule) Name() string { return "Duplicate subflow across flows" }
func (r *DuplicateSubflowAcrossFlowsRule) Description() string {
	return "Subflows whose actions are identical to a subflow in another flow; a fix in one copy misses the others."
}
func (r *DuplicateSubflowAcrossFlowsRule) DefaultSeverity() models.Severity {
	return models.SeverityInfo
}
func (r *DuplicateSubflowAcrossFlowsRule) Category() string { return "Style" }

func (r *DuplicateSubflowAcrossFlowsRule) FactsVersion() int { return 1 }

// dupSubflowMinBlocks: a subflow with fewer counted blocks (see
// subflowBodyHash: not a comment, END or disabled block) is trivial and
// never reported; empty, one-call and two-step helpers are duplicated by design.
const dupSubflowMinBlocks = 3

type dupSubflowFacts struct {
	Subflows []dupSubflowEntry `json:"subflows"`
	// Truncated: the smallest subflows were left out to fit MaxLibraryFactsBytes.
	Truncated bool `json:"truncated,omitempty"`
}

type dupSubflowEntry struct {
	Name   string `json:"name"`
	Hash   string `json:"hash"`
	Blocks int    `json:"blocks"`
}

// Extract records every non-trivial subflow's body hash, in document order.
func (r *DuplicateSubflowAcrossFlowsRule) Extract(ctx *RuleContext) (json.RawMessage, error) {
	var entries []dupSubflowEntry
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		hash, n := subflowBodyHash(sf)
		if n < dupSubflowMinBlocks {
			continue
		}
		entries = append(entries, dupSubflowEntry{Name: sf.Name, Hash: hash, Blocks: n})
	}
	if len(entries) == 0 {
		return nil, nil
	}
	return encodeDupSubflowFacts(entries, MaxLibraryFactsBytes)
}

// encodeDupSubflowFacts encodes entries in document order; over limit it keeps
// the largest subflows that fit (ties: document order) and marks Truncated.
func encodeDupSubflowFacts(entries []dupSubflowEntry, limit int) (json.RawMessage, error) {
	data, err := json.Marshal(dupSubflowFacts{Subflows: entries})
	if err != nil || len(data) <= limit {
		return data, err
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return entries[order[a]].Blocks > entries[order[b]].Blocks })
	size := len(`{"subflows":[],"truncated":true}`)
	keep := make([]bool, len(entries))
	for _, i := range order {
		e, err := json.Marshal(entries[i])
		if err != nil {
			return nil, err
		}
		if size+len(e)+1 > limit {
			continue
		}
		size += len(e) + 1
		keep[i] = true
	}
	kept := make([]dupSubflowEntry, 0, len(entries))
	for i, e := range entries {
		if keep[i] {
			kept = append(kept, e)
		}
	}
	return json.Marshal(dupSubflowFacts{Subflows: kept, Truncated: true})
}

// subflowBodyHash hashes what a subflow does, not what it is called or where
// it sits: per block in pre-order its nesting depth, type, raw type, name and
// properties (sorted by key), each length-prefixed. Comments, END blocks
// (implied by the depth) and disabled blocks with their children are left
// out, and so are IDs and line numbers. It returns 128 bits as hex and the
// number of blocks hashed.
func subflowBodyHash(sf *models.Subflow) (string, int) {
	h := sha256.New()
	var buf []byte
	var keys []string
	n := 0
	str := func(s string) {
		buf = binary.AppendUvarint(buf, uint64(len(s)))
		buf = append(buf, s...)
	}
	var walk func(blocks []models.Block, depth uint64)
	walk = func(blocks []models.Block, depth uint64) {
		for i := range blocks {
			b := &blocks[i]
			if b.Type == models.BlockTypeComment || b.Type == models.BlockTypeEnd || strings.HasPrefix(b.RawType, "DISABLED_") {
				continue
			}
			n++
			buf = binary.AppendUvarint(buf[:0], depth)
			str(string(b.Type))
			str(b.RawType)
			str(b.Name)
			keys = keys[:0]
			for k := range b.Properties {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			buf = binary.AppendUvarint(buf, uint64(len(keys)))
			for _, k := range keys {
				str(k)
				str(b.Properties[k])
			}
			h.Write(buf)
			walk(b.Children, depth+1)
		}
	}
	walk(sf.Blocks, 0)
	return hex.EncodeToString(h.Sum(nil)[:16]), n
}

type dupOccurrence struct {
	flowID, flowName, subflow, anchor string
	blocks                            int
}

// CheckLibrary groups the recorded subflows by body hash and reports every
// copy in a group that spans two or more flows; copies inside one flow alone
// are not reported.
func (r *DuplicateSubflowAcrossFlowsRule) CheckLibrary(ctx context.Context, lib Library) ([]models.LibraryFinding, error) {
	groups := make(map[string][]dupOccurrence)
	visited := 0
	err := lib.Each(func(s models.FlowSummary) error {
		if visited++; visited%256 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		facts, ok := s.Facts[r.ID()]
		if !ok {
			return nil
		}
		var f dupSubflowFacts
		if json.Unmarshal(facts.Data, &f) != nil {
			return nil // a corrupt extract only loses this flow
		}
		var sameName map[string]int
		for _, e := range f.Subflows {
			o := dupOccurrence{flowID: s.FlowID, flowName: s.FlowName, subflow: e.Name, blocks: e.Blocks}
			// Subflow names are unique in a valid flow; a repeated one gets
			// its document-order rank as the anchor so fingerprints differ.
			if sameName == nil {
				sameName = make(map[string]int, len(f.Subflows))
			}
			if k := sameName[e.Name]; k > 0 {
				o.anchor = "#" + strconv.Itoa(k)
			}
			sameName[e.Name]++
			groups[e.Hash] = append(groups[e.Hash], o)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0, len(groups))
	for h, occ := range groups {
		if len(occ) > 1 {
			hashes = append(hashes, h)
		}
	}
	sort.Strings(hashes)
	var out []models.LibraryFinding
	for _, h := range hashes {
		occ := groups[h]
		flows := make(map[string]bool, len(occ))
		for _, o := range occ {
			flows[o.flowID] = true
		}
		if len(flows) < 2 {
			continue
		}
		sort.Slice(occ, func(i, j int) bool {
			a, b := occ[i], occ[j]
			if a.flowName != b.flowName {
				return a.flowName < b.flowName
			}
			if a.subflow != b.subflow {
				return a.subflow < b.subflow
			}
			if a.flowID != b.flowID {
				return a.flowID < b.flowID
			}
			return a.anchor < b.anchor
		})
		for i, o := range occ {
			related := make([]models.LibraryLocation, 0, min(len(occ)-1, maxLibraryRelated))
			example := -1
			for j, p := range occ {
				if j == i {
					continue
				}
				if example < 0 && p.flowID != o.flowID {
					example = j
				}
				if len(related) < maxLibraryRelated {
					related = append(related, models.LibraryLocation{FlowID: p.flowID, FlowName: p.flowName, SubflowName: p.subflow})
				}
			}
			out = append(out, models.LibraryFinding{
				Finding: models.Finding{
					Severity:    r.DefaultSeverity(),
					Title:       "Subflow duplicated across flows",
					Description: dupSubflowDescription(o, occ[example], len(occ), len(flows)),
					Suggestion: "Move the shared steps into one desktop flow and call it with 'Run desktop flow' " +
						"from each flow, so a fix reaches every copy.",
					Metadata: map[string]interface{}{
						"subflow":     o.subflow,
						"contentHash": h,
						"blocks":      o.blocks,
						"copies":      len(occ),
						"flows":       len(flows),
					},
				},
				FlowID:      o.flowID,
				FlowName:    o.flowName,
				SubflowName: o.subflow,
				Related:     related,
				Anchor:      o.anchor,
			})
		}
	}
	return out, nil
}

// dupSubflowDescription names one copy in another flow; copies and flows
// count the whole group, this copy included.
func dupSubflowDescription(o, example dupOccurrence, copies, flows int) string {
	if copies == 2 {
		return fmt.Sprintf("Subflow '%s' (%d actions) is identical to subflow '%s' in flow '%s'.",
			o.subflow, o.blocks, example.subflow, example.flowName)
	}
	return fmt.Sprintf("Subflow '%s' (%d actions) is one of %d identical copies in %d flows, including '%s' in flow '%s'.",
		o.subflow, o.blocks, copies, flows, example.subflow, example.flowName)
}

func init() {
	registerLibraryRule(&DuplicateSubflowAcrossFlowsRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StagePreview,
	})
}
