// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

// Package query implements the DirQ query DSL parser and evaluator.
package query

// Query is the top-level AST node for a DirQ query.
//
// Example: SELECT hostname, disk.pct_used WHERE tag.env = 'prod' AND disk.pct_used > 80 ORDER BY disk.pct_used DESC LIMIT 10
type Query struct {
	Select  []*SelectExpr
	Where   Expr // nil means no WHERE clause
	GroupBy *GroupByClause
	OrderBy []*OrderByField
	Limit   int // 0 means no limit
}

// SelectExpr is either a bare field name, a wildcard (*), or an aggregation function.
type SelectExpr struct {
	Star    bool     // SELECT *
	AggFunc *AggFunc // COUNT(field), etc.
	Field   string   // bare field: hostname, disk.pct_used
}

// AggFunc represents COUNT(field), AVG(field), etc.
type AggFunc struct {
	Name string // COUNT, AVG, MIN, MAX, SUM
	Arg  string // field name
}

// ─────────────────────────────────────────────────────────
// Expression tree (WHERE clause)
// ─────────────────────────────────────────────────────────

// Expr is a WHERE clause expression node.
type Expr interface {
	exprNode()
}

// BinaryExpr represents AND / OR.
type BinaryExpr struct {
	Op    string // "AND" or "OR"
	Left  Expr
	Right Expr
}

// NotExpr represents NOT expr.
type NotExpr struct {
	Expr Expr
}

// CompareExpr represents field op value (e.g. disk.pct_used > 80).
type CompareExpr struct {
	Field    string
	Operator string // =, !=, >, <, >=, <=
	Value    Value
}

// LikeExpr represents field LIKE pattern or field NOT LIKE pattern.
type LikeExpr struct {
	Field   string
	Pattern string
	Negated bool
}

// InExpr represents field IN ('a', 'b') or field NOT IN ('a', 'b').
type InExpr struct {
	Field   string
	Values  []string
	Negated bool
}

// IsNullExpr represents field IS NULL or field IS NOT NULL.
type IsNullExpr struct {
	Field   string
	Negated bool // true = IS NOT NULL
}

func (*BinaryExpr) exprNode()  {}
func (*NotExpr) exprNode()     {}
func (*CompareExpr) exprNode() {}
func (*LikeExpr) exprNode()    {}
func (*InExpr) exprNode()      {}
func (*IsNullExpr) exprNode()  {}

// Value holds either a number or a string literal.
type Value struct {
	Number *float64
	String *string
}

// GroupByClause lists the fields to group by.
type GroupByClause struct {
	Fields []string
}

// OrderByField specifies a single ordering field with direction.
type OrderByField struct {
	Field string
	Desc  bool
}

// ─────────────────────────────────────────────────────────
// Legacy compatibility types
// ─────────────────────────────────────────────────────────

// Condition is a flat filter condition used for proto-based agent pushdown.
// Complex expressions (OR, NOT) cannot be represented here — they are
// evaluated server-side instead.
type Condition struct {
	Field    string
	Operator string
	Value    *Value
	In       *InClause
}

// InClause holds values for an IN expression.
type InClause struct {
	Values []string
}
