// =============================================================================
// File: internal/dap/viz_test.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

package dap

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// vizLeaf builds a synthetic leaf variable.
func vizLeaf(name, value string) *VarNode {
	return &VarNode{Var: Variable{Name: name, Value: value}}
}

// vizBox builds a synthetic expanded container. The reference only has to be
// non-zero: these trees are already expanded, nothing will ask for it.
func vizBox(name, value string, kids ...*VarNode) *VarNode {
	return &VarNode{Var: Variable{Name: name, Value: value, VariablesReference: 1}, Children: kids}
}

// vizStr returns a pointer to s, for comparing table and grid cells.
func vizStr(s string) *string { return &s }

// vizSchema loads the schema from docs/, the file renderers are written to.
func vizSchema(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("../../docs/viz-schema-v1.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	return s
}

// vizValidate checks v (decoded JSON) against schema node s, resolving refs
// against root, and returns every violation. It implements only the keywords
// docs/viz-schema-v1.json uses and reports any other as an error, so a schema
// edit cannot silently escape it.
func vizValidate(root, s map[string]any, v any, path string) []string {
	var errs []string
	known := map[string]bool{"$schema": true, "$id": true, "$comment": true, "title": true, "description": true, "$defs": true,
		"$ref": true, "type": true, "const": true, "enum": true, "required": true, "properties": true,
		"additionalProperties": true, "items": true, "minimum": true, "allOf": true, "if": true, "then": true}
	for k := range s {
		if !known[k] {
			errs = append(errs, fmt.Sprintf("%s: validator does not implement keyword %q", path, k))
		}
	}
	if ref, ok := s["$ref"].(string); ok {
		name := strings.TrimPrefix(ref, "#/$defs/")
		defs, _ := root["$defs"].(map[string]any)
		def, _ := defs[name].(map[string]any)
		if def == nil {
			return append(errs, fmt.Sprintf("%s: unresolved $ref %s", path, ref))
		}
		errs = append(errs, vizValidate(root, def, v, path)...)
	}
	if typ, ok := s["type"]; ok {
		var types []string
		switch tt := typ.(type) {
		case string:
			types = []string{tt}
		case []any:
			for _, x := range tt {
				if xs, ok := x.(string); ok {
					types = append(types, xs)
				}
			}
		}
		if !slices.ContainsFunc(types, func(want string) bool { return vizIsType(v, want) }) {
			return append(errs, fmt.Sprintf("%s: %v is not of type %v", path, v, types))
		}
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, v) {
		errs = append(errs, fmt.Sprintf("%s: %v != const %v", path, v, c))
	}
	if e, ok := s["enum"].([]any); ok && !slices.ContainsFunc(e, func(x any) bool { return reflect.DeepEqual(x, v) }) {
		errs = append(errs, fmt.Sprintf("%s: %v not in enum %v", path, v, e))
	}
	if m, ok := s["minimum"].(float64); ok {
		if f, isNum := v.(float64); isNum && f < m {
			errs = append(errs, fmt.Sprintf("%s: %v < minimum %v", path, f, m))
		}
	}
	if obj, isObj := v.(map[string]any); isObj {
		for _, r := range vizAnySlice(s["required"]) {
			if _, ok := obj[fmt.Sprint(r)]; !ok {
				errs = append(errs, fmt.Sprintf("%s: missing required %q", path, r))
			}
		}
		props, _ := s["properties"].(map[string]any)
		for k, val := range obj {
			if ps, ok := props[k].(map[string]any); ok {
				errs = append(errs, vizValidate(root, ps, val, path+"."+k)...)
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					errs = append(errs, fmt.Sprintf("%s: unexpected property %q", path, k))
				}
			case map[string]any:
				errs = append(errs, vizValidate(root, ap, val, path+"."+k)...)
			}
		}
	}
	if arr, isArr := v.([]any); isArr {
		if items, ok := s["items"].(map[string]any); ok {
			for i, x := range arr {
				errs = append(errs, vizValidate(root, items, x, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	}
	for _, sub := range vizAnySlice(s["allOf"]) {
		if sm, ok := sub.(map[string]any); ok {
			errs = append(errs, vizValidate(root, sm, v, path)...)
		}
	}
	if cond, ok := s["if"].(map[string]any); ok && len(vizValidate(root, cond, v, path)) == 0 {
		if then, ok := s["then"].(map[string]any); ok {
			errs = append(errs, vizValidate(root, then, v, path)...)
		}
	}
	return errs
}

// vizAnySlice is a nil-safe []any assertion.
func vizAnySlice(x any) []any {
	s, _ := x.([]any)
	return s
}

// vizIsType is JSON Schema's type test over encoding/json's decoded values.
func vizIsType(v any, want string) bool {
	switch want {
	case "null":
		return v == nil
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// vizRoundTrip encodes a result the way the socket will and decodes it back
// into plain JSON values.
func vizRoundTrip(t *testing.T, r VizResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc
}

// vizFixtures are synthetic variable trees in each adapter's own spelling,
// with the kind each one must normalise to.
func vizFixtures() map[string]struct {
	root *VarNode
	want VizKind
} {
	type fx = struct {
		root *VarNode
		want VizKind
	}
	return map[string]fx{
		"delve slice of structs": {vizBox("people", "[]main.P len: 2",
			vizBox("[0]", "main.P {Name: \"ann\", Age: 31}", vizLeaf("Name", `"ann"`), vizLeaf("Age", "31")),
			vizBox("[1]", "main.P {Name: \"bo\", Age: 4}", vizLeaf("Name", `"bo"`), vizLeaf("Age", "4")),
		), VizTable},
		"debugpy list of dicts": {vizBox("rows", "[{...}, {...}]",
			vizBox("special variables", ""),
			vizBox("0", "{'id': 1}", vizBox("special variables", ""), vizLeaf("'id'", "1"), vizLeaf("len()", "1")),
			vizBox("1", "{'id': 2, 'tag': 'x'}", vizLeaf("'id'", "2"), vizLeaf("'tag'", "'x'"), vizLeaf("len()", "2")),
			vizLeaf("len()", "2"),
		), VizTable},
		"js-debug number array": {vizBox("xs", "(3) [1, 2.5, -3]",
			vizLeaf("0", "1"), vizLeaf("1", "2.5"), vizLeaf("2", "-3"),
			vizLeaf("length", "3"), vizBox("[[Prototype]]", "Array(0)"),
		), VizPlot},
		"delve matrix": {vizBox("m", "[][]int len: 2",
			vizBox("[0]", "[]int len: 2", vizLeaf("[0]", "1"), vizLeaf("[1]", "2")),
			vizBox("[1]", "[]int len: 1", vizLeaf("[0]", "3")),
		), VizGrid},
		"js-debug ast": {vizBox("node", "{type: 'Program', children: Array(2)}",
			vizLeaf("type", "'Program'"),
			vizBox("children", "(2) [{…}, 'tok']",
				vizBox("0", "{type: 'Ident', name: 'x', children: Array(0)}",
					vizLeaf("type", "'Ident'"), vizLeaf("name", "'x'"), vizBox("children", "[]", vizLeaf("length", "0"))),
				vizLeaf("1", "'tok'"),
				vizLeaf("length", "2"),
			),
			vizBox("[[Prototype]]", "Object"),
		), VizAST},
		"go struct ast": {vizBox("n", "*main.Node", vizLeaf("Type", `"Leaf"`), vizLeaf("Children", "[]*main.Node len: 0")), VizAST},
		"plain struct":  {vizBox("cfg", "main.Cfg", vizLeaf("Port", "8080"), vizBox("Inner", "main.In", vizLeaf("A", "1"))), VizTree},
		"string leaf":   {vizLeaf("s", `"hello"`), VizText},
		"one number":    {vizBox("xs", "[1]", vizLeaf("0", "1")), VizTree},
		"string array":  {vizBox("xs", "['1', '2']", vizLeaf("0", "'1'"), vizLeaf("1", "'2'")), VizTree},
		"nan array":     {vizBox("xs", "[nan, 1]", vizLeaf("0", "NaN"), vizLeaf("1", "1")), VizTree},
		"3-D array": {vizBox("c", "[][][]int", vizBox("[0]", "[][]int",
			vizBox("[0]", "[]int", vizLeaf("[0]", "1")))), VizTree},
		"index gap": {vizBox("m", "map", vizLeaf("0", "1"), vizLeaf("2", "3")), VizTree},
		"unexpanded table row": {vizBox("xs", "[]*P", vizBox("[0]", "*P", vizLeaf("A", "1")),
			&VarNode{Var: Variable{Name: "[1]", Value: "*P", VariablesReference: 9}}), VizTree},
	}
}

// TestVizExtractorsChooseTheRightKind pins the detection order on synthetic
// trees in each adapter's spelling — including the near-misses that must fall
// back to the generic tree rather than be shown as something they are not.
func TestVizExtractorsChooseTheRightKind(t *testing.T) {
	for name, fx := range vizFixtures() {
		got := Normalize(fx.root, VizSource{Expression: name}).Kind
		if got != fx.want {
			t.Errorf("%s: kind = %s, want %s", name, got, fx.want)
		}
	}
}

// TestVizEveryOutputValidatesAgainstTheSchemaFile is the contract test: every
// fixture's encoded document is checked against docs/viz-schema-v1.json
// itself, not against a second description of the shape kept in Go.
func TestVizEveryOutputValidatesAgainstTheSchemaFile(t *testing.T) {
	schema := vizSchema(t)
	for name, fx := range vizFixtures() {
		doc := vizRoundTrip(t, Normalize(fx.root, VizSource{Expression: name, FrameID: 3, Adapter: "dlv"}))
		for _, e := range vizValidate(schema, schema, doc, "$") {
			t.Errorf("%s: %s", name, e)
		}
	}
	nilDoc := vizRoundTrip(t, Normalize(nil, VizSource{}))
	for _, e := range vizValidate(schema, schema, nilDoc, "$") {
		t.Errorf("nil root: %s", e)
	}
}

// TestVizSchemaValidatorIsNotVacuous proves the validator above can fail: a
// validator that accepts everything would make the contract test meaningless.
func TestVizSchemaValidatorIsNotVacuous(t *testing.T) {
	schema := vizSchema(t)
	good := func() map[string]any {
		return vizRoundTrip(t, Normalize(vizFixtures()["js-debug number array"].root, VizSource{Adapter: "js"}))
	}
	if errs := vizValidate(schema, schema, good(), "$"); len(errs) != 0 {
		t.Fatalf("baseline should validate: %v", errs)
	}
	mutations := map[string]func(map[string]any){
		"missing frameId":  func(d map[string]any) { delete(d["source"].(map[string]any), "frameId") },
		"wrong data shape": func(d map[string]any) { d["kind"] = "table" },
		"string in plot":   func(d map[string]any) { d["data"].(map[string]any)["y"] = []any{"1"} },
		"unknown kind":     func(d map[string]any) { d["kind"] = "chart" },
		"extra field":      func(d map[string]any) { d["extra"] = true },
		"schemaVersion 2":  func(d map[string]any) { d["schemaVersion"] = float64(2) },
		"unknown cap":      func(d map[string]any) { d["truncatedBy"] = []any{"memory"} },
		"warning not bool": func(d map[string]any) { d["sideEffectWarning"] = "yes" },
	}
	for name, mutate := range mutations {
		d := good()
		mutate(d)
		if errs := vizValidate(schema, schema, d, "$"); len(errs) == 0 {
			t.Errorf("%s: mutated document validated", name)
		}
	}
}

// TestVizSchemaFileMatchesTheGoConstants keeps the kind enum and the version
// in the schema file and in viz.go from drifting apart.
func TestVizSchemaFileMatchesTheGoConstants(t *testing.T) {
	schema := vizSchema(t)
	props := schema["properties"].(map[string]any)
	if v := props["schemaVersion"].(map[string]any)["const"]; v != float64(VizSchemaVersion) {
		t.Errorf("schema schemaVersion const = %v, Go = %d", v, VizSchemaVersion)
	}
	var fileKinds, goKinds []string
	for _, k := range props["kind"].(map[string]any)["enum"].([]any) {
		fileKinds = append(fileKinds, k.(string))
	}
	for _, k := range vizKinds {
		goKinds = append(goKinds, string(k))
	}
	sort.Strings(fileKinds)
	sort.Strings(goKinds)
	if !slices.Equal(fileKinds, goKinds) {
		t.Errorf("schema kinds %v != Go kinds %v", fileKinds, goKinds)
	}
	defs := schema["$defs"].(map[string]any)
	for _, k := range goKinds {
		if _, ok := defs[k]; !ok {
			t.Errorf("schema has no $defs entry for kind %q", k)
		}
	}
}

// TestVizGraphIsDeclaredButNeverProduced is where the "graph is in the schema
// but no extractor emits it" claim lives. DAP variables carry no object
// identity, so a graph cannot be built honestly yet; update this test, not a
// comment, the day one is.
func TestVizGraphIsDeclaredButNeverProduced(t *testing.T) {
	if !slices.Contains(vizKinds, VizGraph) {
		t.Fatal("graph must stay a declared kind in schema v1")
	}
	for name, fx := range vizFixtures() {
		if k := Normalize(fx.root, VizSource{}).Kind; k == VizGraph {
			t.Errorf("%s produced graph", name)
		}
	}
}

// TestVizTableAlignsHeterogeneousRecords checks that a record missing a column
// gets a null cell instead of shifting its other values left, and that
// debugpy's quoted dict keys and bookkeeping rows do not become columns.
func TestVizTableAlignsHeterogeneousRecords(t *testing.T) {
	r := Normalize(vizFixtures()["debugpy list of dicts"].root, VizSource{})
	tbl := r.Data.(VizTableData)
	if !slices.Equal(tbl.Columns, []string{"id", "tag"}) {
		t.Fatalf("columns = %v", tbl.Columns)
	}
	want := [][]*string{{vizStr("1"), nil}, {vizStr("2"), vizStr("'x'")}}
	if !reflect.DeepEqual(tbl.Rows, want) {
		t.Errorf("rows = %v", vizRoundTrip(t, r)["data"])
	}
}

// TestVizTableKeepsLengthAsARealField: "length" is dropped only when judging
// whether something is an array; on a record it is ordinary data.
func TestVizTableKeepsLengthAsARealField(t *testing.T) {
	root := vizBox("segs", "Array(1)", vizBox("0", "{length: 5}", vizLeaf("length", "5")), vizLeaf("length", "1"))
	r := Normalize(root, VizSource{})
	if r.Kind != VizTable || !slices.Equal(r.Data.(VizTableData).Columns, []string{"length"}) {
		t.Errorf("got %s %+v", r.Kind, r.Data)
	}
}

// TestVizPlotValues checks the series is the parsed numbers, in index order.
func TestVizPlotValues(t *testing.T) {
	r := Normalize(vizFixtures()["js-debug number array"].root, VizSource{})
	if got := r.Data.(VizPlotData).Y; !slices.Equal(got, []float64{1, 2.5, -3}) {
		t.Errorf("y = %v", got)
	}
}

// TestVizNumericMatrixIsAGridNotAPlot: a 2-D array of numbers is a grid, the
// detection order puts grid before plot on purpose.
func TestVizNumericMatrixIsAGridNotAPlot(t *testing.T) {
	r := Normalize(vizFixtures()["delve matrix"].root, VizSource{})
	g := r.Data.(VizGridData)
	if g.Rows != 2 || g.Cols != 2 {
		t.Fatalf("dims = %dx%d", g.Rows, g.Cols)
	}
	want := [][]*string{{vizStr("1"), vizStr("2")}, {vizStr("3"), nil}}
	if !reflect.DeepEqual(g.Cells, want) {
		t.Errorf("ragged row was not padded with null: %v", vizRoundTrip(t, r)["data"])
	}
}

// TestVizASTStructure checks types, props, a bare-token child and that the
// prototype entry is not read as a field.
func TestVizASTStructure(t *testing.T) {
	r := Normalize(vizFixtures()["js-debug ast"].root, VizSource{})
	root := r.Data.(VizASTData).Root
	if root.Type != "Program" || len(root.Children) != 2 || root.Props != nil {
		t.Fatalf("root = %+v", root)
	}
	ident, tok := root.Children[0], root.Children[1]
	if ident.Type != "Ident" || ident.Props["name"] != "x" || len(ident.Children) != 0 {
		t.Errorf("ident = %+v", ident)
	}
	if tok.Value != "tok" {
		t.Errorf("token child = %+v", tok)
	}
	if r.Truncated {
		t.Error("fully expanded AST reported truncated")
	}
}

// TestVizASTUnfetchedChildrenIsTruncated: a children list the expander never
// fetched must not be presented as a childless node with no warning.
func TestVizASTUnfetchedChildrenIsTruncated(t *testing.T) {
	root := vizBox("n", "{…}", vizLeaf("type", `"Block"`),
		&VarNode{Var: Variable{Name: "children", Value: "Array(40)", VariablesReference: 7}})
	r := Normalize(root, VizSource{})
	if r.Kind != VizAST || !r.Truncated {
		t.Errorf("kind=%s truncated=%v", r.Kind, r.Truncated)
	}
}

// TestVizTruncatedPropagatesFromAnyNode: the expander marks the node it cut;
// the document must say so whichever extractor ran.
func TestVizTruncatedPropagatesFromAnyNode(t *testing.T) {
	root := vizFixtures()["delve slice of structs"].root
	root.Children[1].Truncated = true
	if r := Normalize(root, VizSource{}); r.Kind != VizTable || !r.Truncated {
		t.Errorf("kind=%s truncated=%v", r.Kind, r.Truncated)
	}
	if doc := vizRoundTrip(t, Normalize(vizLeaf("x", "1"), VizSource{})); doc["truncated"] != nil {
		t.Error("a complete document must omit truncated")
	}
}

// TestVizTreeKeepsEverythingAndMarksUnexpanded: the fallback assumes nothing,
// so bookkeeping rows stay, and an unfetched container is marked rather than
// drawn as a leaf.
func TestVizTreeKeepsEverythingAndMarksUnexpanded(t *testing.T) {
	root := vizBox("obj", "Object", vizLeaf("a", "1"), vizBox("[[Prototype]]", "Object"),
		&VarNode{Var: Variable{Name: "deep", Value: "{…}", VariablesReference: 4}})
	r := Normalize(root, VizSource{})
	tree := r.Data.(VizTreeData).Root
	if r.Kind != VizTree || len(tree.Children) != 3 {
		t.Fatalf("kind=%s children=%d", r.Kind, len(tree.Children))
	}
	if !tree.Children[2].Unexpanded || tree.Children[0].Unexpanded {
		t.Errorf("unexpanded flags wrong: %+v", tree.Children)
	}
}

// TestVizFrameIDZeroSurvivesMarshal: js-debug's innermost frame is id 0, and
// the same omitempty mistake already shipped once in evaluateArgs.
func TestVizFrameIDZeroSurvivesMarshal(t *testing.T) {
	doc := vizRoundTrip(t, Normalize(vizLeaf("x", "1"), VizSource{Expression: "x", FrameID: 0, Adapter: "js-debug"}))
	src := doc["source"].(map[string]any)
	if v, ok := src["frameId"]; !ok || v != float64(0) {
		t.Errorf("frameId = %v present=%v", v, ok)
	}
	if doc["schemaVersion"] != float64(1) {
		t.Errorf("schemaVersion = %v", doc["schemaVersion"])
	}
}

// TestVizTextAndNilRoot: a leaf keeps the adapter's rendering and type; a nil
// root is an empty text document, not a panic.
func TestVizTextAndNilRoot(t *testing.T) {
	leaf := &VarNode{Var: Variable{Name: "s", Value: `"hi"`, Type: "string"}}
	if r := Normalize(leaf, VizSource{}); r.Data != (VizTextData{Value: `"hi"`, Type: "string"}) {
		t.Errorf("leaf data = %+v", r.Data)
	}
	if r := Normalize(nil, VizSource{}); r.Kind != VizText || r.SchemaVersion != 1 {
		t.Errorf("nil root = %+v", r)
	}
}

// TestVizIndexNameAndUnquote covers the two spelling helpers directly.
func TestVizIndexNameAndUnquote(t *testing.T) {
	for in, want := range map[string]int{"0": 0, "[3]": 3, " 12 ": 12} {
		if got, ok := indexName(in); !ok || got != want {
			t.Errorf("indexName(%q) = %d,%v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "[]", "-1", "a", "[0 … 99]", "1.5"} {
		if _, ok := indexName(bad); ok {
			t.Errorf("indexName(%q) accepted", bad)
		}
	}
	for in, want := range map[string]string{`"a\"b"`: `a"b`, "'x'": "x", "`r`": "r", `"`: `"`, "plain": "plain", `'mis"`: `'mis"`} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestVizSchemaAcceptsTheEvaluateFields pins the deliberate v1 additions: a
// document carrying the evaluate path's side-effect warning and the caps it
// hit validates, exactly as encoded from VizResult.
func TestVizSchemaAcceptsTheEvaluateFields(t *testing.T) {
	schema := vizSchema(t)
	r := Normalize(vizFixtures()["js-debug number array"].root, VizSource{Adapter: "js"})
	r.Truncated, r.TruncatedBy, r.SideEffectWarning = true, []string{"depth", "nodes", "output", "timeout"}, true
	doc := vizRoundTrip(t, r)
	if doc["sideEffectWarning"] != true || len(vizAnySlice(doc["truncatedBy"])) != 4 {
		t.Fatalf("fields not encoded: %v", doc)
	}
	for _, e := range vizValidate(schema, schema, doc, "$") {
		t.Error(e)
	}
}

// TestVizSchemaDescribesTheSocketReplies runs the full validator over the two
// non-visualization replies the socket command sends, in their wire form: a
// truncated frames list whose selected frame is id 0, and an error. The app's
// tests check its replies' keys against these same defs.
func TestVizSchemaDescribesTheSocketReplies(t *testing.T) {
	schema := vizSchema(t)
	defs, _ := schema["$defs"].(map[string]any)
	good := map[string]string{
		"frames": `{"schemaVersion":1,"frames":[{"id":0,"name":"main","path":"/p/m.go","line":3}],"selectedFrameId":0,"truncated":true}`,
		"error":  `{"schemaVersion":1,"error":"resumed during evaluate"}`,
	}
	for def, doc := range good {
		var v any
		if err := json.Unmarshal([]byte(doc), &v); err != nil {
			t.Fatal(err)
		}
		node, _ := defs[def].(map[string]any)
		for _, e := range vizValidate(schema, node, v, "$") {
			t.Errorf("%s: %s", def, e)
		}
		v.(map[string]any)["extra"] = true
		if len(vizValidate(schema, node, v, "$")) == 0 {
			t.Errorf("%s accepted an undeclared key", def)
		}
	}
}
