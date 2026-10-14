// =============================================================================
// File: internal/dap/viz.go
// Author: Vonzelle Brown
// Created: 2026-10-14
// Copyright: 2026 Vonzelle Brown. All rights reserved.
// =============================================================================

// viz.go normalises an evaluated expression's DAP variable tree into the
// versioned visualization document described by docs/viz-schema-v1.json.
//
// Clean-room: written against DAP and our own schema only. No visualizer's
// source was read; the feature list comes from public product descriptions.
//
// The input is a tree that has ALREADY been expanded (breadth-first, under the
// depth/node caps the evaluate path owns). This file never talks to an adapter:
// it only looks at the shape of what came back, so it can be tested on
// synthetic trees and cannot block on a debuggee.
//
// Extractors run in a fixed order and the first match wins:
//
//	ast   an object with a `type` field and a `children` field
//	grid  an array whose elements are all arrays of leaves
//	table an array whose elements are all records (objects with fields)
//	plot  an array of two or more numeric leaves
//	tree  anything else with children
//	text  a leaf
//
// `graph` is part of schema v1 so a renderer can be written against it, but NO
// extractor produces it yet: building a graph needs object identity (two
// references to the same node), and DAP variables do not carry one.
// TestVizGraphIsDeclaredButNeverProduced is where that claim lives.
//
// Adapters spell arrays differently — delve names elements "[0]", debugpy and
// js-debug name them "0" — and pad containers with bookkeeping entries
// (debugpy's "len()" and "special variables", js-debug's "length" and
// "[[Prototype]]"). Those are filtered by name before any shape is judged.
// js-debug's range groups for very large arrays ("[0 … 99]") are NOT flattened:
// such an array falls through to the generic tree rather than a guessed plot.

package dap

import (
	"math"
	"strconv"
	"strings"
)

// VizSchemaVersion is the schemaVersion every document carries. Bump it, and
// add docs/viz-schema-v2.json, on any change a v1 renderer could misread.
const VizSchemaVersion = 1

// VizKind is the document's discriminator; it selects the shape of Data.
type VizKind string

// The kinds schema v1 defines. The order here is the schema's enum order.
const (
	VizText  VizKind = "text"
	VizTable VizKind = "table"
	VizTree  VizKind = "tree"
	VizGraph VizKind = "graph"
	VizGrid  VizKind = "grid"
	VizPlot  VizKind = "plot"
	VizAST   VizKind = "ast"
)

// vizKinds lists every kind, so the schema test can compare it with the file.
var vizKinds = []VizKind{VizText, VizTable, VizTree, VizGraph, VizGrid, VizPlot, VizAST}

// VarNode is one DAP variable plus whichever children the expander fetched.
//
// Truncated is set by the expander when it stopped before fetching all of this
// node's children (a node or depth cap). A node with a non-zero
// VariablesReference and no Children was never expanded at all.
type VarNode struct {
	Var       Variable
	Children  []*VarNode
	Truncated bool
}

// VizSource records what produced a document. FrameID has no omitempty on
// purpose: js-debug's innermost frame is id 0, and dropping it would make the
// document claim a global evaluation.
type VizSource struct {
	Expression string `json:"expression"`
	FrameID    int    `json:"frameId"`
	Adapter    string `json:"adapter"`
}

// VizResult is the whole document, ready for encoding/json.
//
// SideEffectWarning and TruncatedBy are set by the evaluate path, not by
// Normalize, which only sees a variable tree. Both are declared in
// docs/viz-schema-v1.json; see its $comment for why they stay in version 1.
type VizResult struct {
	SchemaVersion     int       `json:"schemaVersion"`
	Kind              VizKind   `json:"kind"`
	Data              any       `json:"data"`
	Source            VizSource `json:"source"`
	Truncated         bool      `json:"truncated,omitempty"`
	TruncatedBy       []string  `json:"truncatedBy,omitempty"`
	SideEffectWarning bool      `json:"sideEffectWarning,omitempty"`
}

// VizTextData is a leaf: the adapter's rendered value.
type VizTextData struct {
	Value string `json:"value"`
	Type  string `json:"type,omitempty"`
}

// VizTreeNode is one node of the generic tree. Unexpanded marks a node the
// adapter said has children that were not fetched, so a renderer can show it
// as collapsible rather than as a leaf.
type VizTreeNode struct {
	Name       string         `json:"name"`
	Value      string         `json:"value"`
	Type       string         `json:"type,omitempty"`
	Children   []*VizTreeNode `json:"children,omitempty"`
	Unexpanded bool           `json:"unexpanded,omitempty"`
}

// VizTreeData wraps the generic tree's root.
type VizTreeData struct {
	Root *VizTreeNode `json:"root"`
}

// VizTableData is an array of records. Each row is aligned with Columns; a
// record that lacks a column has a nil (JSON null) cell, which is different
// from a field whose value is the empty string.
type VizTableData struct {
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
}

// VizPlotData is a numeric series indexed by position.
type VizPlotData struct {
	Y []float64 `json:"y"`
}

// VizGridData is a 2-D array. Ragged rows are padded with nil to Cols.
type VizGridData struct {
	Rows  int         `json:"rows"`
	Cols  int         `json:"cols"`
	Cells [][]*string `json:"cells"`
}

// VizASTNode is one syntax-tree node. Type is the `type` field's value, Props
// are its other leaf fields, and Value is set only for a child that was not
// itself node-shaped (a bare token or literal).
type VizASTNode struct {
	Type     string            `json:"type"`
	Value    string            `json:"value,omitempty"`
	Props    map[string]string `json:"props,omitempty"`
	Children []*VizASTNode     `json:"children,omitempty"`
}

// VizASTData wraps the syntax tree's root.
type VizASTData struct {
	Root *VizASTNode `json:"root"`
}

// VizGraphNode is a graph vertex. See the file comment: declared, not produced.
type VizGraphNode struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// VizGraphEdge is a directed edge between two VizGraphNode ids.
type VizGraphEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Label string `json:"label,omitempty"`
}

// VizGraphData is the graph kind's payload.
type VizGraphData struct {
	Nodes []VizGraphNode `json:"nodes"`
	Edges []VizGraphEdge `json:"edges"`
}

// vizGroupNames are adapter bookkeeping entries that are never data: grouping
// folders, prototypes, and debugpy's synthetic length row.
var vizGroupNames = map[string]bool{
	"special variables":   true,
	"function variables":  true,
	"class variables":     true,
	"protected variables": true,
	"[[Prototype]]":       true,
	"__proto__":           true,
	"len()":               true,
}

// Normalize turns an expanded variable tree into a schema-v1 document,
// choosing the most specific kind whose extractor accepts the tree's shape.
func Normalize(root *VarNode, src VizSource) VizResult {
	res := VizResult{SchemaVersion: VizSchemaVersion, Source: src}
	if root == nil {
		res.Kind, res.Data = VizText, VizTextData{}
		return res
	}
	res.Truncated = anyTruncated(root)

	if ast, trunc, ok := astOf(root); ok {
		res.Kind, res.Data = VizAST, VizASTData{Root: ast}
		res.Truncated = res.Truncated || trunc
		return res
	}
	if elems, ok := arrayElems(root); ok {
		if g, ok := gridOf(elems); ok {
			res.Kind, res.Data = VizGrid, g
			return res
		}
		if t, ok := tableOf(elems); ok {
			res.Kind, res.Data = VizTable, t
			return res
		}
		if p, ok := plotOf(elems); ok {
			res.Kind, res.Data = VizPlot, p
			return res
		}
	}
	if len(root.Children) > 0 || root.Var.VariablesReference != 0 {
		res.Kind, res.Data = VizTree, VizTreeData{Root: treeOf(root)}
		return res
	}
	res.Kind, res.Data = VizText, VizTextData{Value: root.Var.Value, Type: root.Var.Type}
	return res
}

// anyTruncated reports whether the expander cut anything short in this tree.
func anyTruncated(n *VarNode) bool {
	if n.Truncated {
		return true
	}
	for _, c := range n.Children {
		if anyTruncated(c) {
			return true
		}
	}
	return false
}

// isLeaf is true for a variable with no children at all — not merely none
// fetched. An unexpanded container is not a leaf.
func isLeaf(n *VarNode) bool {
	return len(n.Children) == 0 && n.Var.VariablesReference == 0
}

// dataChildren drops the adapter's bookkeeping entries.
func dataChildren(n *VarNode) []*VarNode {
	out := make([]*VarNode, 0, len(n.Children))
	for _, c := range n.Children {
		if !vizGroupNames[c.Var.Name] {
			out = append(out, c)
		}
	}
	return out
}

// indexName parses an element name, "[3]" (delve) or "3" (debugpy, js-debug).
func indexName(name string) (int, bool) {
	s := strings.TrimSpace(name)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	if s == "" || strings.TrimLeft(s, "0123456789") != "" {
		return 0, false
	}
	i, err := strconv.Atoi(s)
	return i, err == nil
}

// arrayElems returns n's elements when n is array-shaped: at least one child,
// and every data child named by consecutive indices from 0. js-debug's
// "length" row is ignored here, and only here — on a record it is a real field.
func arrayElems(n *VarNode) ([]*VarNode, bool) {
	var elems []*VarNode
	for _, c := range dataChildren(n) {
		if c.Var.Name == "length" {
			continue
		}
		i, ok := indexName(c.Var.Name)
		if !ok || i != len(elems) {
			return nil, false
		}
		elems = append(elems, c)
	}
	return elems, len(elems) > 0
}

// recordFields returns n's fields when n is an object: it has data children
// and is not array-shaped.
func recordFields(n *VarNode) ([]*VarNode, bool) {
	fields := dataChildren(n)
	if len(fields) == 0 {
		return nil, false
	}
	if _, isArray := arrayElems(n); isArray {
		return nil, false
	}
	return fields, true
}

// fieldKey is a field's name as a user would write it: debugpy shows dict keys
// as their repr ('type'), so quotes are stripped before names are compared.
func fieldKey(name string) string {
	return unquote(name)
}

// unquote strips one layer of matching quotes from an adapter-rendered string.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q != '"' && q != '\'' && q != '`') || s[len(s)-1] != q {
		return s
	}
	if q == '"' || q == '`' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s[1 : len(s)-1]
}

// cell is a node's rendered value as a table/grid cell.
func cell(n *VarNode) *string {
	v := n.Var.Value
	return &v
}

// gridOf accepts a non-empty array of arrays whose elements are all leaves.
func gridOf(rows []*VarNode) (VizGridData, bool) {
	g := VizGridData{Rows: len(rows)}
	cells := make([][]*string, len(rows))
	for i, r := range rows {
		elems, ok := arrayElems(r)
		if !ok {
			return VizGridData{}, false
		}
		for _, e := range elems {
			if !isLeaf(e) {
				return VizGridData{}, false
			}
			cells[i] = append(cells[i], cell(e))
		}
		g.Cols = max(g.Cols, len(elems))
	}
	for i := range cells {
		for len(cells[i]) < g.Cols {
			cells[i] = append(cells[i], nil)
		}
	}
	g.Cells = cells
	return g, true
}

// tableOf accepts an array whose elements are all records. Columns are the
// union of field names in first-seen order, so heterogeneous records still
// line up and a missing field reads as null, not as a shifted cell.
func tableOf(elems []*VarNode) (VizTableData, bool) {
	var t VizTableData
	col := map[string]int{}
	recs := make([][]*VarNode, len(elems))
	for i, e := range elems {
		fields, ok := recordFields(e)
		if !ok {
			return VizTableData{}, false
		}
		recs[i] = fields
		for _, f := range fields {
			k := fieldKey(f.Var.Name)
			if _, seen := col[k]; !seen {
				col[k] = len(t.Columns)
				t.Columns = append(t.Columns, k)
			}
		}
	}
	t.Rows = make([][]*string, len(recs))
	for i, fields := range recs {
		row := make([]*string, len(t.Columns))
		for _, f := range fields {
			row[col[fieldKey(f.Var.Name)]] = cell(f)
		}
		t.Rows[i] = row
	}
	return t, true
}

// numeric parses a leaf's display text as a plain number. Variable.Value is
// display text, not data, so this is deliberately narrow: a decimal literal and
// nothing else. NaN and ±Inf are refused — JSON cannot carry them.
func numeric(n *VarNode) (float64, bool) {
	if !isLeaf(n) {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(n.Var.Value), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// plotOf accepts two or more numeric leaves; one number is not a series.
func plotOf(elems []*VarNode) (VizPlotData, bool) {
	if len(elems) < 2 {
		return VizPlotData{}, false
	}
	ys := make([]float64, len(elems))
	for i, e := range elems {
		f, ok := numeric(e)
		if !ok {
			return VizPlotData{}, false
		}
		ys[i] = f
	}
	return VizPlotData{Y: ys}, true
}

// astFields finds the `type` and `children` fields of a node-shaped object.
// Names match case-insensitively so a Go struct's Type/Children qualifies.
func astFields(n *VarNode) (typ, kids *VarNode, rest []*VarNode, ok bool) {
	fields, ok := recordFields(n)
	if !ok {
		return nil, nil, nil, false
	}
	for _, f := range fields {
		switch strings.ToLower(fieldKey(f.Var.Name)) {
		case "type":
			typ = f
		case "children":
			kids = f
		default:
			rest = append(rest, f)
		}
	}
	if typ == nil || kids == nil || !isLeaf(typ) {
		return nil, nil, nil, false
	}
	if _, isArray := arrayElems(kids); !isArray && !emptyArray(kids) {
		return nil, nil, nil, false
	}
	return typ, kids, rest, true
}

// emptyArray is true for a container with no elements: no children at all, or
// — js-debug's rendering of [] — nothing but bookkeeping and a "length" row.
func emptyArray(n *VarNode) bool {
	for _, c := range dataChildren(n) {
		if c.Var.Name != "length" {
			return false
		}
	}
	return true
}

// astOf builds a syntax tree when n is node-shaped. The second result reports
// a `children` list that the expander never fetched, which the document must
// call truncated rather than present as a leaf node.
func astOf(n *VarNode) (*VizASTNode, bool, bool) {
	typ, kids, rest, ok := astFields(n)
	if !ok {
		return nil, false, false
	}
	node := &VizASTNode{Type: unquote(typ.Var.Value)}
	for _, f := range rest {
		if isLeaf(f) {
			if node.Props == nil {
				node.Props = map[string]string{}
			}
			node.Props[fieldKey(f.Var.Name)] = unquote(f.Var.Value)
		}
	}
	trunc := len(kids.Children) == 0 && kids.Var.VariablesReference != 0
	elems, _ := arrayElems(kids)
	for _, e := range elems {
		if child, t, ok := astOf(e); ok {
			node.Children = append(node.Children, child)
			trunc = trunc || t
			continue
		}
		node.Children = append(node.Children, &VizASTNode{Type: e.Var.Type, Value: unquote(e.Var.Value)})
	}
	return node, trunc, true
}

// treeOf is the generic fallback: the variable tree exactly as fetched,
// bookkeeping entries included, because nothing about its shape was assumed.
func treeOf(n *VarNode) *VizTreeNode {
	t := &VizTreeNode{
		Name:       n.Var.Name,
		Value:      n.Var.Value,
		Type:       n.Var.Type,
		Unexpanded: len(n.Children) == 0 && n.Var.VariablesReference != 0,
	}
	for _, c := range n.Children {
		t.Children = append(t.Children, treeOf(c))
	}
	return t
}
