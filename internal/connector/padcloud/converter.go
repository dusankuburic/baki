package padcloud

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"pad-core/models"
	"pad-core/parser"
)

// maxConvertDepth caps how deep toBlock recurses into the cloud action tree.
// A hostile or malformed Dataverse response with ~50k nesting levels is only
// ~3 MB of JSON but overflows the goroutine stack (1 GB) via unbounded
// recursion, killing the whole ingest sweep. Real flows are tens of levels
// deep at most, so 1000 is a generous bound that stops a pathological tree
// without affecting legitimate input.
const maxConvertDepth = 1000

// CloudConverter is the concrete Converter implementation: it turns a PAD cloud
// flow definition (the JSON action tree returned by the Power Platform /
// Dataverse exportworkflowdefinition endpoint) into the parser's
// models.FlowDocument — the representation the analyzer, library, and governance
// surface consume.
//
// IMPORTANT — validation status: the cloud action schema (nested "actions" /
// "rpaActions" / "nestedItems" with "type" + "properties") is documented but NOT
// yet validated against a real tenant response. The converter is therefore
// deliberately defensive: it tolerates the documented candidate keys and several
// plausible aliases, and produces a best-effort FlowDocument. It must be
// re-validated against a real API sample before the connector is enabled in
// production. Until then the connector is wired as experimental.
type CloudConverter struct{}

// NewCloudConverter returns the default cloud-definition converter.
func NewCloudConverter() *CloudConverter { return &CloudConverter{} }

// cloudAction is a permissive decoding of one PAD cloud action node. PAD's cloud
// schema names the action type and child arrays differently across versions, so
// multiple candidate keys are decoded; the first non-empty child array wins.
type cloudAction struct {
	Type     string                     `json:"type"`
	TypeName string                     `json:"typeName"`
	Kind     string                     `json:"actionType"`
	Name     string                     `json:"name"`
	Comment  string                     `json:"comment"`
	Props    map[string]any             `json:"properties"`
	Actions  []cloudAction              `json:"actions"`
	Rpa      []cloudAction              `json:"rpaActions"`
	Nested   []cloudAction              `json:"nestedItems"`
	Sub      []cloudAction              `json:"subactions"`
	Kids     []cloudAction              `json:"children"`
	Extra    map[string]json.RawMessage `json:"-"`
}

// cloudDefinition mirrors the top-level exportworkflowdefinition response. PAD
// may wrap the action tree under "definition" or "properties"; both are
// unwrapped when present.
type cloudDefinition struct {
	Name       string           `json:"name"`
	Actions    []cloudAction    `json:"actions"`
	Rpa        []cloudAction    `json:"rpaActions"`
	Nested     []cloudAction    `json:"nestedItems"`
	Definition *cloudDefinition `json:"definition"`
	Properties *cloudDefinition `json:"properties"`
}

// varRef matches PAD variable references embedded in property values so the
// converter can populate Block.Variables (the analyzer keys off these). It
// captures the BASE identifier, so subscripted and dotted forms — %Items[0]%,
// %Item.Prop%, %Row['Name']% — count as uses of Items/Item/Row rather than
// being dropped (which would under-count variables in the analyzer). The tail
// after the identifier may be anything except a %, so one reference can never
// swallow the next.
var varRef = regexp.MustCompile(`%([A-Za-z_][A-Za-z0-9_]*)(?:[.\[][^%]*)?%`)

// Convert implements Converter.
func (c *CloudConverter) Convert(name string, def json.RawMessage) (*models.FlowDocument, error) {
	if len(def) == 0 {
		return nil, fmt.Errorf("empty flow definition")
	}

	var root cloudDefinition
	if err := json.Unmarshal(def, &root); err != nil {
		return nil, fmt.Errorf("parse cloud definition: %w", err)
	}

	// Unwrap a nested "definition" / "properties" envelope (some responses wrap
	// the action tree one or two levels deep).
	for d := &root; ; {
		if d.Definition != nil {
			*d = *d.Definition
			continue
		}
		if d.Properties != nil && len(d.Properties.Actions)+len(d.Properties.Rpa)+len(d.Properties.Nested) > 0 {
			*d = *d.Properties
			continue
		}
		break
	}

	topActions := mergeChildren(root.Actions, root.Rpa, root.Nested)
	if len(topActions) == 0 {
		// No actionable content — return nil so the ingester records it as a skip
		// rather than storing an empty flow (which would pollute the library and
		// dashboards). This likely signals a schema mismatch to investigate.
		return nil, nil
	}

	flowName := name
	if root.Name != "" {
		flowName = root.Name
	}

	subflowID := uuid.NewString()
	subflow := models.Subflow{
		ID:   subflowID,
		Name: flowName,
	}

	var (
		lineNum int
		blocks  int
		maxDpt  int
	)
	for _, a := range topActions {
		depth := 1
		blk := c.toBlock(a, subflowID, "", 1, &lineNum, &blocks, &depth)
		if depth > maxDpt {
			maxDpt = depth
		}
		subflow.Blocks = append(subflow.Blocks, blk)
	}

	doc := &models.FlowDocument{
		ID:       uuid.NewString(),
		Name:     flowName,
		Subflows: []models.Subflow{subflow},
		Metadata: models.FlowMetadata{
			BlockCount:   blocks,
			SubflowCount: 1,
			MaxDepth:     maxDpt,
		},
	}
	doc.RebuildIndexes()
	return doc, nil
}

// toBlock converts one cloud action node (recursively) into a models.Block. The
// depth/line/block counters are threaded through so synthesized indent/line
// numbers and metadata reflect the tree shape.
func (c *CloudConverter) toBlock(a cloudAction, subflowID, parentID string, depth int, line, blockCount, maxDepth *int) models.Block {
	*blockCount++
	*line++
	if depth > *maxDepth {
		*maxDepth = depth
	}

	rawType := actionType(a)
	name := a.Comment
	if strings.TrimSpace(name) == "" {
		name = a.Name
	}
	if strings.TrimSpace(name) == "" {
		name = humanizeType(rawType)
	}

	blk := models.Block{
		ID:         uuid.NewString(),
		Name:       name,
		Type:       parser.ClassifyBlockType(rawType),
		RawType:    rawType,
		Indent:     depth,
		LineNumber: *line,
		Properties: propsToStrings(a.Props),
		ParentID:   parentID,
		SubflowID:  subflowID,
	}
	blk.Variables = extractVariables(blk.Properties, a.Comment, a.Name)

	// Stop descending past the depth cap: keep this block but drop its children,
	// so a pathologically deep cloud definition truncates instead of overflowing
	// the goroutine stack and crashing the process. Combined with the per-flow
	// recover in Ingester.Ingest, a hostile definition is recorded as a failure
	// rather than taking down ingestion for the whole tenant.
	if depth < maxConvertDepth {
		for _, child := range mergeChildren(a.Actions, a.Rpa, a.Nested, a.Sub, a.Kids) {
			blk.Children = append(blk.Children, c.toBlock(child, subflowID, blk.ID, depth+1, line, blockCount, maxDepth))
		}
	}
	return blk
}

// actionType resolves the action's module-qualified type string from the
// candidate keys PAD's schema uses across versions.
func actionType(a cloudAction) string {
	for _, v := range []string{a.Type, a.TypeName, a.Kind} {
		if v != "" {
			return v
		}
	}
	return ""
}

// humanizeType turns a module-qualified action (e.g. "Excel.ReadCell") into a
// readable block name when the node carries no comment/name.
func humanizeType(t string) string {
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// mergeChildren returns the first non-empty candidate child slice. PAD's cloud
// schema names the child array differently across action types / versions.
func mergeChildren(groups ...[]cloudAction) []cloudAction {
	for _, g := range groups {
		if len(g) > 0 {
			return g
		}
	}
	return nil
}

// propsToStrings flattens a cloud properties object into the string→string map
// the analyzer expects. Scalars are stringified; complex values are JSON-encoded
// so no information is lost.
func propsToStrings(props map[string]any) map[string]string {
	if len(props) == 0 {
		return nil
	}
	out := make(map[string]string, len(props))
	for k, v := range props {
		switch vv := v.(type) {
		case nil:
			out[k] = ""
		case string:
			out[k] = vv
		case bool:
			out[k] = fmt.Sprint(vv)
		case float64:
			// All JSON numbers decode to float64; 'f'/-1 formats integers and
			// decimals without scientific notation (1000000 → "1000000", not
			// "1e+06"), which would otherwise corrupt the value the analyzer reads.
			out[k] = strconv.FormatFloat(vv, 'f', -1, 64)
		default:
			if b, err := json.Marshal(vv); err == nil {
				out[k] = string(b)
			}
		}
	}
	return out
}

// extractVariables scans property values and the comment/name for %Var%
// references, returning the unique set so the analyzer can track usage.
func extractVariables(props map[string]string, extra ...string) []string {
	seen := map[string]struct{}{}
	scan := func(s string) {
		for _, m := range varRef.FindAllStringSubmatch(s, -1) {
			seen[m[1]] = struct{}{}
		}
	}
	for _, v := range props {
		scan(v)
	}
	for _, v := range extra {
		scan(v)
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}
