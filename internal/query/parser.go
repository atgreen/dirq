// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

// dirqLexer defines the token types for the DirQ query DSL.
var dirqLexer = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Keyword", Pattern: `\b(?:SELECT|FROM|WHERE|AND|ORDER|BY|GROUP|DESC|ASC|LIKE|COUNT|AVG|MIN|MAX|SUM)\b`},
	{Name: "Ident", Pattern: `[a-zA-Z_][a-zA-Z0-9_]*`},
	{Name: "Number", Pattern: `[0-9]+(?:\.[0-9]+)?`},
	{Name: "String", Pattern: `'[^']*'`},
	{Name: "CompOp", Pattern: `>=|<=|!=`},
	{Name: "Punct", Pattern: `[(),.*:=<>]`},
	{Name: "Whitespace", Pattern: `\s+`},
})

var parser = participle.MustBuild[Query](
	participle.Lexer(dirqLexer),
	participle.Elide("Whitespace"),
	participle.Unquote("String"),
)

// Parse parses a DirQ query string into a Query AST.
func Parse(input string) (*Query, error) {
	return parser.ParseString("", input)
}
