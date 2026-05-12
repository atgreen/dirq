// Package query implements the DirQ query DSL parser and evaluator.
package query

// Query is the top-level AST node for a DirQ query.
//
// Example: SELECT hostname, disk.pct_used FROM tag:prod WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC
type Query struct {
	Select  []*SelectExpr `parser:"'SELECT' @@ (',' @@)*"`
	From    *FromClause   `parser:"('FROM' @@)?"`
	Where   *WhereClause  `parser:"('WHERE' @@)?"`
	GroupBy *GroupByClause `parser:"('GROUP' 'BY' @@)?"`
	OrderBy *OrderByClause `parser:"('ORDER' 'BY' @@)?"`
}

// SelectExpr is either a bare field name or an aggregation function call.
type SelectExpr struct {
	AggFunc *AggFunc `parser:"( @@"`
	Field   string   `parser:"| @(Ident ('.' Ident)*) )"`
}

// AggFunc represents COUNT(field), AVG(field), etc.
type AggFunc struct {
	Name string `parser:"@('COUNT' | 'AVG' | 'MIN' | 'MAX' | 'SUM')"`
	Arg  string `parser:"'(' @(Ident ('.' Ident)*) ')'"`
}

// FromClause represents the target scope: tag:value, group:value, or *.
type FromClause struct {
	All   bool   `parser:"( @'*'"`
	Scope *Scope `parser:"| @@ )"`
}

// Scope is a scoped target like tag:prod or group:webservers.
type Scope struct {
	Kind  string `parser:"@('tag' | 'group') ':'"`
	Value string `parser:"@Ident"`
}

// WhereClause is a list of conditions joined by AND.
type WhereClause struct {
	Conditions []*Condition `parser:"@@ ('AND' @@)*"`
}

// Condition is a single comparison: field op value.
type Condition struct {
	Field    string `parser:"@(Ident ('.' Ident)*)"`
	Operator string `parser:"@( CompOp | '>' | '<' | '=' | 'LIKE' )"`
	Value    *Value `parser:"@@"`
}

// Value holds either a number or a string literal.
type Value struct {
	Number *float64 `parser:"( @Number"`
	String *string  `parser:"| @String )"`
}

// GroupByClause lists the fields to group by.
type GroupByClause struct {
	Fields []string `parser:"@(Ident ('.' Ident)*) (',' @(Ident ('.' Ident)*))*"`
}

// OrderByClause specifies ordering.
type OrderByClause struct {
	Field string `parser:"@(Ident ('.' Ident)*)"`
	Desc  bool   `parser:"@'DESC'?"`
}
