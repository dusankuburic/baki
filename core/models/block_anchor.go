package models

import "sort"

// BlockAnchor names a block by its content, so a reference to it can outlive a re-parse: block ids are minted
// per parse, and every applied fix or edit re-parses the flow. Its peers are the blocks of its subflow with the
// same action type and name; Ordinal is its place among them in line order, Count how many there are.
type BlockAnchor struct {
	Subflow string `json:"subflow"`
	RawType string `json:"rawType"`
	Name    string `json:"name"`
	Ordinal int    `json:"ordinal"`
	Count   int    `json:"count"`
}

// anchorPeers returns the blocks of subflow with rawType and name, in line order (ties by id).
func (d *FlowDocument) anchorPeers(subflow, rawType, name string) []*Block {
	var peers []*Block
	for id, b := range d.BlocksByID {
		if b.RawType != rawType || b.Name != name {
			continue
		}
		if sf := d.BlockSubflow[id]; sf != nil && sf.Name == subflow {
			peers = append(peers, b)
		}
	}
	sort.Slice(peers, func(i, j int) bool {
		if peers[i].LineNumber != peers[j].LineNumber {
			return peers[i].LineNumber < peers[j].LineNumber
		}
		return peers[i].ID < peers[j].ID
	})
	return peers
}

// AnchorOf returns the anchor of the block with id blockID; nil when the document has no such block.
func (d *FlowDocument) AnchorOf(blockID string) *BlockAnchor {
	if d == nil || d.BlocksByID == nil {
		return nil
	}
	b := d.BlocksByID[blockID]
	if b == nil {
		return nil
	}
	a := &BlockAnchor{RawType: b.RawType, Name: b.Name}
	if sf := d.BlockSubflow[blockID]; sf != nil {
		a.Subflow = sf.Name
	}
	peers := d.anchorPeers(a.Subflow, a.RawType, a.Name)
	a.Count = len(peers)
	for i, p := range peers {
		if p.ID == blockID {
			a.Ordinal = i
		}
	}
	return a
}

// ResolveAnchor returns the id of the block a names: the peer at the same place, while there are as many peers
// as when the anchor was taken. Lines are no guide (a fix that inserts lines above the block can move it onto a
// peer's old line). A changed peer count resolves to "": the likeliest cause is a fix that removed this very
// block (remove-block on a duplicate leaves its twin in place), and a block reported gone is safer than a
// conversation silently moved onto its twin.
func (d *FlowDocument) ResolveAnchor(a BlockAnchor) string {
	if d == nil || d.BlocksByID == nil || a.RawType == "" {
		return ""
	}
	peers := d.anchorPeers(a.Subflow, a.RawType, a.Name)
	if len(peers) != a.Count || a.Ordinal < 0 || a.Ordinal >= len(peers) {
		return ""
	}
	return peers[a.Ordinal].ID
}
