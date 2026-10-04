package parser

import (
	"sort"
	"strings"

	"pad-core/models"
)

// exactMap: O(1) exact-match lookup. Keys are stored as-is (PAD emits consistent casing).
var exactMap = map[string]models.BlockType{
	// Loop keywords
	"Loop": models.BlockTypeLoop,
	"LOOP": models.BlockTypeLoop,
	// Condition keywords
	"If":   models.BlockTypeCondition,
	"IF":   models.BlockTypeCondition,
	"Else": models.BlockTypeCondition,
	"ELSE": models.BlockTypeCondition,
	// Subflow / call keywords
	"Subflow":       models.BlockTypeSubflow,
	"Run subflow":   models.BlockTypeSubflow,
	"Call subflow":  models.BlockTypeSubflow,
	"Region":        models.BlockTypeSubflow,
	"SUBFLOW":       models.BlockTypeSubflow,
	"CALL":          models.BlockTypeSubflow,
	"DISABLED_CALL": models.BlockTypeSubflow,
	// Error handling keywords
	"OnBlockError":   models.BlockTypeErrorHandler,
	"On block error": models.BlockTypeErrorHandler,
	"Try":            models.BlockTypeErrorHandler,
	"Catch":          models.BlockTypeErrorHandler,
	// Structural keywords
	"BLOCK":  models.BlockTypeBlock,
	"SWITCH": models.BlockTypeSwitch,
	// Comment / annotation
	"COMMENT": models.BlockTypeComment,
	"Comment": models.BlockTypeComment,
	// Variable-assignment keywords
	"SET":          models.BlockTypeVariable,
	"Set variable": models.BlockTypeVariable,
	// Wait / sleep
	"WAIT": models.BlockTypeWait,
	"Wait": models.BlockTypeWait,
	// Flow-control primitives (these are leaf actions, not containers)
	"GOTO":          models.BlockTypeAction,
	"LABEL":         models.BlockTypeAction,
	"EXIT":          models.BlockTypeAction,
	"EXIT_LOOP":     models.BlockTypeAction,
	"NEXT_LOOP":     models.BlockTypeAction,
	"ERROR_CAPTURE": models.BlockTypeAction,
}

// prefixEntry pairs a module prefix with the BlockType it implies; entries
// sort longest-first so the first match is always the most-specific one,
// deterministic regardless of map-iteration randomness.
type prefixEntry struct {
	prefix string
	bt     models.BlockType
}

var prefixRules []prefixEntry

func init() {
	raw := []prefixEntry{
		{"UIAutomation.IfWindow", models.BlockTypeCondition},
		{"UIAutomation.If", models.BlockTypeCondition},
		{"System.IfProcess", models.BlockTypeCondition},
		{"System.If", models.BlockTypeCondition},
		{"Variables.If", models.BlockTypeCondition},
		{"Display.If", models.BlockTypeCondition},
		{"Text.If", models.BlockTypeCondition},
		{"Variables.Else", models.BlockTypeCondition},
		{"Display.Else", models.BlockTypeCondition},
		{"Text.Else", models.BlockTypeCondition},

		{"FlowControl.RunSubflow", models.BlockTypeSubflow},
		{"FlowControl.RunDesktopFlow", models.BlockTypeSubflow},
		{"FlowControl.Run", models.BlockTypeSubflow},

		{"Loop.", models.BlockTypeLoop},

		// These are standalone operations, not variable assignments; longer
		// prefixes sort before the catch-all "Variables." rule, so they win.
		{"Variables.CreateNewList", models.BlockTypeAction},
		{"Variables.ClearList", models.BlockTypeAction},
		{"Variables.AddItemToList", models.BlockTypeAction},
		{"Variables.RemoveItemFromList", models.BlockTypeAction},
		{"Variables.SortList", models.BlockTypeAction},
		{"Variables.ShuffleList", models.BlockTypeAction},
		{"Variables.MergeLists", models.BlockTypeAction},
		{"Variables.ReverseList", models.BlockTypeAction},
		{"Variables.RemoveDuplicateItemsFromList", models.BlockTypeAction},
		{"Variables.FindCommonListItems", models.BlockTypeAction},
		{"Variables.SubtractLists", models.BlockTypeAction},
		{"Variables.IncreaseVariable", models.BlockTypeAction},
		{"Variables.DecreaseVariable", models.BlockTypeAction},
		{"Variables.GenerateRandomNumber", models.BlockTypeAction},
		{"Variables.TruncateNumber", models.BlockTypeAction},

		// Catch-all for SET and future Variables.*
		{"Variables.", models.BlockTypeVariable},
		{"DataTable.", models.BlockTypeVariable},

		{"Synchronization.", models.BlockTypeWait},

		// All other known PAD modules default to ACTION; listing them
		// explicitly classifies any new action in them without code changes.
		{"ActiveDirectory.", models.BlockTypeAction},
		{"AWS.", models.BlockTypeAction},
		{"Azure.", models.BlockTypeAction},
		{"Clipboard.", models.BlockTypeAction},
		{"Cognitiveservices.", models.BlockTypeAction},
		{"Compression.", models.BlockTypeAction},
		{"Cryptography.", models.BlockTypeAction},
		{"CSV.", models.BlockTypeAction},
		{"CyberArk.", models.BlockTypeAction},
		{"Database.", models.BlockTypeAction},
		{"DateTime.", models.BlockTypeAction},
		{"Display.", models.BlockTypeAction},
		{"Email.", models.BlockTypeAction},
		{"Excel.", models.BlockTypeAction},
		{"Exchange.", models.BlockTypeAction},
		{"File.", models.BlockTypeAction},
		{"FlowControl.", models.BlockTypeAction},
		{"Folder.", models.BlockTypeAction},
		{"FTP.", models.BlockTypeAction},
		{"GoogleCognitive.", models.BlockTypeAction},
		{"HTTPClient.", models.BlockTypeAction},
		{"IMAP.", models.BlockTypeAction},
		{"Json.", models.BlockTypeAction},
		{"Keyboard.", models.BlockTypeAction},
		{"Mouse.", models.BlockTypeAction},
		{"OCR.", models.BlockTypeAction},
		{"Outlook.", models.BlockTypeAction},
		{"Pdf.", models.BlockTypeAction},
		{"PowerPoint.", models.BlockTypeAction},
		{"Runbook.", models.BlockTypeAction},
		{"SAP.", models.BlockTypeAction},
		{"Scripting.", models.BlockTypeAction},
		{"Services.", models.BlockTypeAction},
		{"Sharepoint.", models.BlockTypeAction},
		{"System.", models.BlockTypeAction},
		{"Terminal.", models.BlockTypeAction},
		{"Text.", models.BlockTypeAction},
		{"UIAutomation.", models.BlockTypeAction},
		{"Web.", models.BlockTypeAction},
		{"Word.", models.BlockTypeAction},
		{"WorkstationAutomation.", models.BlockTypeAction},
		{"Xml.", models.BlockTypeAction},
	}

	// Sort longest-first so the greedy first-match is the most-specific rule.
	sort.Slice(raw, func(i, j int) bool {
		return len(raw[i].prefix) > len(raw[j].prefix)
	})
	prefixRules = raw
}

// ClassifyBlockType maps a PAD rawType to a BlockType: (1) exactMap exact
// match, (2) exact after ToUpper (lowercase PAD variants), (3) longest-prefix
// in sorted prefixRules (deterministic), (4) any dotted identifier → ACTION,
// (5) fallback ACTION.
func ClassifyBlockType(rawType string) models.BlockType {
	// 1. Fast exact match.
	if bt, ok := exactMap[rawType]; ok {
		return bt
	}

	// Dotted raw types (the majority of blocks) can never hit the exact-keyword
	// map (no key contains '.'), so skip the allocating ToUpper copy for them.
	if !strings.Contains(rawType, ".") {
		// 2. Case-insensitive fallback for exact keywords (handles "loop", "IF", etc.).
		if bt, ok := exactMap[strings.ToUpper(rawType)]; ok {
			return bt
		}
	}

	// 3. Longest-prefix match — deterministic because prefixRules is sorted.
	for _, pr := range prefixRules {
		if strings.HasPrefix(rawType, pr.prefix) {
			return pr.bt
		}
	}

	// 4. Any unrecognised dotted action (future PAD modules) is treated as a generic action.
	if strings.Contains(rawType, ".") {
		return models.BlockTypeAction
	}

	// 5. Ultimate fallback.
	return models.BlockTypeAction
}
