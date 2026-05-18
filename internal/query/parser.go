// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"fmt"
	"strconv"
	"strings"
)

// ─────────────────────────────────────────────────────────
// Tokens
// ─────────────────────────────────────────────────────────

type tokenKind int

const (
	tkEOF tokenKind = iota
	tkIdent
	tkNumber
	tkString
	tkComma
	tkDot
	tkLParen
	tkRParen
	tkStar
	tkColon
	tkEquals
	tkNotEquals  // !=
	tkLess       // <
	tkLessEq     // <=
	tkGreater    // >
	tkGreaterEq  // >=

	// Keywords (stored as tkIdent during lexing, promoted after lookup).
	tkSELECT
	tkFROM
	tkWHERE
	tkAND
	tkOR
	tkNOT
	tkORDER
	tkBY
	tkGROUP
	tkDESC
	tkASC
	tkLIKE
	tkIN
	tkIS
	tkNULL
	tkLIMIT
	tkCOUNT
	tkAVG
	tkMIN
	tkMAX
	tkSUM
)

var keywords = map[string]tokenKind{
	"SELECT": tkSELECT,
	"FROM":   tkFROM,
	"WHERE":  tkWHERE,
	"AND":    tkAND,
	"OR":     tkOR,
	"NOT":    tkNOT,
	"ORDER":  tkORDER,
	"BY":     tkBY,
	"GROUP":  tkGROUP,
	"DESC":   tkDESC,
	"ASC":    tkASC,
	"LIKE":   tkLIKE,
	"IN":     tkIN,
	"IS":     tkIS,
	"NULL":   tkNULL,
	"LIMIT":  tkLIMIT,
	"COUNT":  tkCOUNT,
	"AVG":    tkAVG,
	"MIN":    tkMIN,
	"MAX":    tkMAX,
	"SUM":    tkSUM,
}

type token struct {
	kind tokenKind
	text string // original text (identifiers) or unquoted string value
	pos  int    // byte offset in input
}

// ─────────────────────────────────────────────────────────
// Lexer
// ─────────────────────────────────────────────────────────

type lexer struct {
	input  string
	pos    int
	tokens []token
}

func lex(input string) ([]token, error) {
	l := &lexer{input: input}
	if err := l.scan(); err != nil {
		return nil, err
	}
	return l.tokens, nil
}

func (l *lexer) scan() error {
	for l.pos < len(l.input) {
		ch := l.input[l.pos]

		// Skip whitespace.
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			l.pos++
			continue
		}

		start := l.pos

		switch {
		case ch == ',':
			l.emit(tkComma, start, 1)
		case ch == '.':
			l.emit(tkDot, start, 1)
		case ch == '(':
			l.emit(tkLParen, start, 1)
		case ch == ')':
			l.emit(tkRParen, start, 1)
		case ch == '*':
			l.emit(tkStar, start, 1)
		case ch == ':':
			l.emit(tkColon, start, 1)
		case ch == '=':
			l.emit(tkEquals, start, 1)
		case ch == '!' && l.peek(1) == '=':
			l.emit(tkNotEquals, start, 2)
		case ch == '<' && l.peek(1) == '=':
			l.emit(tkLessEq, start, 2)
		case ch == '<':
			l.emit(tkLess, start, 1)
		case ch == '>' && l.peek(1) == '=':
			l.emit(tkGreaterEq, start, 2)
		case ch == '>':
			l.emit(tkGreater, start, 1)
		case ch == '\'' || ch == '"':
			if err := l.scanString(); err != nil {
				return err
			}
		case isDigit(ch):
			l.scanNumber()
		case isIdentStart(ch):
			l.scanIdent()
		default:
			return fmt.Errorf("unexpected character %q at position %d", string(ch), l.pos)
		}
	}

	l.tokens = append(l.tokens, token{kind: tkEOF, pos: l.pos})
	return nil
}

func (l *lexer) emit(kind tokenKind, start, length int) {
	l.tokens = append(l.tokens, token{
		kind: kind,
		text: l.input[start : start+length],
		pos:  start,
	})
	l.pos = start + length
}

func (l *lexer) peek(offset int) byte {
	i := l.pos + offset
	if i < len(l.input) {
		return l.input[i]
	}
	return 0
}

func (l *lexer) scanString() error {
	start := l.pos
	quote := l.input[l.pos] // ' or "
	l.pos++                 // skip opening quote
	var b strings.Builder
	for l.pos < len(l.input) {
		ch := l.input[l.pos]
		if ch == quote {
			if l.pos+1 < len(l.input) && l.input[l.pos+1] == quote {
				// Escaped quote (doubled).
				b.WriteByte(ch)
				l.pos += 2
				continue
			}
			l.pos++ // skip closing quote
			l.tokens = append(l.tokens, token{kind: tkString, text: b.String(), pos: start})
			return nil
		}
		b.WriteByte(ch)
		l.pos++
	}
	return fmt.Errorf("unterminated string starting at position %d", start)
}

func (l *lexer) scanNumber() {
	start := l.pos
	for l.pos < len(l.input) && isDigit(l.input[l.pos]) {
		l.pos++
	}
	if l.pos < len(l.input) && l.input[l.pos] == '.' {
		l.pos++
		for l.pos < len(l.input) && isDigit(l.input[l.pos]) {
			l.pos++
		}
	}
	l.tokens = append(l.tokens, token{kind: tkNumber, text: l.input[start:l.pos], pos: start})
}

func (l *lexer) scanIdent() {
	start := l.pos
	for l.pos < len(l.input) && isIdentContinue(l.input[l.pos]) {
		l.pos++
	}
	text := l.input[start:l.pos]
	upper := strings.ToUpper(text)
	kind := tkIdent
	if kw, ok := keywords[upper]; ok {
		kind = kw
		text = upper // normalize keyword casing
	}
	l.tokens = append(l.tokens, token{kind: kind, text: text, pos: start})
}

func isDigit(ch byte) bool       { return ch >= '0' && ch <= '9' }
func isIdentStart(ch byte) bool  { return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' }
func isIdentContinue(ch byte) bool {
	return isIdentStart(ch) || isDigit(ch) || ch == '-'
}

// ─────────────────────────────────────────────────────────
// Parser
// ─────────────────────────────────────────────────────────

type parser struct {
	tokens []token
	pos    int
}

// Parse parses a DirQ query string into a Query AST.
func Parse(input string) (*Query, error) {
	tokens, err := lex(input)
	if err != nil {
		return nil, err
	}
	p := &parser{tokens: tokens}
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if p.cur().kind != tkEOF {
		return nil, p.errorf("unexpected %q at position %d", p.cur().text, p.cur().pos)
	}
	return q, nil
}

func (p *parser) cur() token {
	if p.pos < len(p.tokens) {
		return p.tokens[p.pos]
	}
	return token{kind: tkEOF, pos: -1}
}

func (p *parser) advance() token {
	t := p.cur()
	if p.pos < len(p.tokens) {
		p.pos++
	}
	return t
}

func (p *parser) expect(kind tokenKind) (token, error) {
	t := p.cur()
	if t.kind != kind {
		return t, p.errorf("expected %s, got %q at position %d", kindName(kind), t.text, t.pos)
	}
	p.advance()
	return t, nil
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// ─────────────────────────────────────────────────────────
// Grammar
// ─────────────────────────────────────────────────────────

func (p *parser) parseQuery() (*Query, error) {
	if _, err := p.expect(tkSELECT); err != nil {
		return nil, err
	}

	q := &Query{}

	// SELECT list.
	sel, err := p.parseSelectList()
	if err != nil {
		return nil, err
	}
	q.Select = sel

	// FROM is accepted for backward compatibility but ignored.
	// Targeting is done via tag.* conditions in WHERE.
	if p.cur().kind == tkFROM {
		p.advance()
		if err := p.skipFrom(); err != nil {
			return nil, err
		}
	}

	// WHERE (optional).
	if p.cur().kind == tkWHERE {
		p.advance()
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		q.Where = expr
	}

	// GROUP BY (optional).
	if p.cur().kind == tkGROUP {
		p.advance()
		if _, err := p.expect(tkBY); err != nil {
			return nil, err
		}
		fields, err := p.parseFieldList()
		if err != nil {
			return nil, err
		}
		q.GroupBy = &GroupByClause{Fields: fields}
	}

	// ORDER BY (optional).
	if p.cur().kind == tkORDER {
		p.advance()
		if _, err := p.expect(tkBY); err != nil {
			return nil, err
		}
		orderBy, err := p.parseOrderByList()
		if err != nil {
			return nil, err
		}
		q.OrderBy = orderBy
	}

	// LIMIT (optional).
	if p.cur().kind == tkLIMIT {
		p.advance()
		t, err := p.expect(tkNumber)
		if err != nil {
			return nil, p.errorf("expected number after LIMIT")
		}
		n, _ := strconv.Atoi(t.text)
		q.Limit = n
	}

	return q, nil
}

func (p *parser) parseSelectList() ([]*SelectExpr, error) {
	var list []*SelectExpr
	for {
		se, err := p.parseSelectExpr()
		if err != nil {
			return nil, err
		}
		list = append(list, se)
		if p.cur().kind != tkComma {
			break
		}
		p.advance() // consume comma
	}
	return list, nil
}

func (p *parser) parseSelectExpr() (*SelectExpr, error) {
	// SELECT *
	if p.cur().kind == tkStar {
		p.advance()
		return &SelectExpr{Star: true}, nil
	}

	// Aggregation function: COUNT(...), AVG(...), etc.
	if isAggKeyword(p.cur().kind) {
		name := p.advance().text
		if _, err := p.expect(tkLParen); err != nil {
			return nil, err
		}
		arg, err := p.parseDottedField()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tkRParen); err != nil {
			return nil, err
		}
		return &SelectExpr{AggFunc: &AggFunc{Name: name, Arg: arg}}, nil
	}

	// Bare field.
	field, err := p.parseDottedField()
	if err != nil {
		return nil, err
	}
	return &SelectExpr{Field: field}, nil
}

// skipFrom consumes a legacy FROM clause (e.g. "FROM *", "FROM tag:env=prod")
// without producing AST nodes. This maintains backward compatibility.
func (p *parser) skipFrom() error {
	if p.cur().kind == tkStar {
		p.advance()
		return nil
	}
	// Consume: ident ":" ident ["=" ident|string]
	if p.cur().kind == tkIdent || isKeywordKind(p.cur().kind) {
		p.advance()
		if p.cur().kind == tkColon {
			p.advance()
			// Consume key.
			if p.cur().kind == tkIdent || isKeywordKind(p.cur().kind) || p.cur().kind == tkString {
				p.advance()
			}
			// Consume optional =value.
			if p.cur().kind == tkEquals {
				p.advance()
				if p.cur().kind == tkIdent || isKeywordKind(p.cur().kind) || p.cur().kind == tkString {
					p.advance()
				}
			}
		}
	}
	return nil
}

// ─────────────────────────────────────────────────────────
// Expression parsing (precedence climbing)
// ─────────────────────────────────────────────────────────

// parseExpr: OR has lowest precedence.
func (p *parser) parseExpr() (Expr, error) {
	left, err := p.parseAndExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkOR {
		p.advance()
		right, err := p.parseAndExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "OR", Left: left, Right: right}
	}
	return left, nil
}

// parseAndExpr: AND has higher precedence than OR.
func (p *parser) parseAndExpr() (Expr, error) {
	left, err := p.parseUnaryExpr()
	if err != nil {
		return nil, err
	}
	for p.cur().kind == tkAND {
		p.advance()
		right, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		left = &BinaryExpr{Op: "AND", Left: left, Right: right}
	}
	return left, nil
}

// parseUnaryExpr: NOT or parenthesized group or condition.
func (p *parser) parseUnaryExpr() (Expr, error) {
	if p.cur().kind == tkNOT {
		p.advance()
		expr, err := p.parseUnaryExpr()
		if err != nil {
			return nil, err
		}
		return &NotExpr{Expr: expr}, nil
	}
	if p.cur().kind == tkLParen {
		p.advance()
		expr, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tkRParen); err != nil {
			return nil, err
		}
		return expr, nil
	}
	return p.parseCondition()
}

// parseCondition: field op value | field [NOT] IN (...) | field [NOT] LIKE pattern | field IS [NOT] NULL
func (p *parser) parseCondition() (Expr, error) {
	field, err := p.parseDottedField()
	if err != nil {
		return nil, err
	}

	// field IS [NOT] NULL
	if p.cur().kind == tkIS {
		p.advance()
		negated := false
		if p.cur().kind == tkNOT {
			p.advance()
			negated = true
		}
		if _, err := p.expect(tkNULL); err != nil {
			return nil, err
		}
		return &IsNullExpr{Field: field, Negated: negated}, nil
	}

	// field NOT IN / field NOT LIKE
	if p.cur().kind == tkNOT {
		p.advance()
		switch p.cur().kind {
		case tkIN:
			return p.parseInExpr(field, true)
		case tkLIKE:
			return p.parseLikeExpr(field, true)
		default:
			return nil, p.errorf("expected IN or LIKE after NOT, got %q at position %d", p.cur().text, p.cur().pos)
		}
	}

	// field IN (...)
	if p.cur().kind == tkIN {
		return p.parseInExpr(field, false)
	}

	// field LIKE pattern
	if p.cur().kind == tkLIKE {
		return p.parseLikeExpr(field, false)
	}

	// field op value
	op, err := p.parseCompareOp()
	if err != nil {
		return nil, err
	}
	val, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	return &CompareExpr{Field: field, Operator: op, Value: val}, nil
}

func (p *parser) parseInExpr(field string, negated bool) (Expr, error) {
	p.advance() // consume IN
	if _, err := p.expect(tkLParen); err != nil {
		return nil, err
	}
	var values []string
	for {
		t, err := p.expect(tkString)
		if err != nil {
			return nil, p.errorf("expected string in IN list at position %d", p.cur().pos)
		}
		values = append(values, t.text)
		if p.cur().kind != tkComma {
			break
		}
		p.advance()
	}
	if _, err := p.expect(tkRParen); err != nil {
		return nil, err
	}
	return &InExpr{Field: field, Values: values, Negated: negated}, nil
}

func (p *parser) parseLikeExpr(field string, negated bool) (Expr, error) {
	p.advance() // consume LIKE
	t, err := p.expect(tkString)
	if err != nil {
		return nil, p.errorf("expected string pattern after LIKE at position %d", p.cur().pos)
	}
	return &LikeExpr{Field: field, Pattern: t.text, Negated: negated}, nil
}

func (p *parser) parseCompareOp() (string, error) {
	t := p.cur()
	switch t.kind {
	case tkEquals:
		p.advance()
		return "=", nil
	case tkNotEquals:
		p.advance()
		return "!=", nil
	case tkLess:
		p.advance()
		return "<", nil
	case tkLessEq:
		p.advance()
		return "<=", nil
	case tkGreater:
		p.advance()
		return ">", nil
	case tkGreaterEq:
		p.advance()
		return ">=", nil
	default:
		return "", p.errorf("expected comparison operator, got %q at position %d", t.text, t.pos)
	}
}

func (p *parser) parseValue() (Value, error) {
	t := p.cur()
	if t.kind == tkNumber {
		p.advance()
		n, err := strconv.ParseFloat(t.text, 64)
		if err != nil {
			return Value{}, p.errorf("invalid number %q", t.text)
		}
		return Value{Number: &n}, nil
	}
	if t.kind == tkString {
		p.advance()
		s := t.text
		return Value{String: &s}, nil
	}
	// Accept unquoted identifiers as string values (e.g., WHERE hostname = fedora).
	// The shell often strips single quotes, so this is a common user pattern.
	// Also consume dots so dotted values like ip-10-0-1-5.ec2.internal work.
	if t.kind == tkIdent {
		p.advance()
		s := t.text
		for p.cur().kind == tkDot {
			p.advance()
			next := p.cur()
			if next.kind != tkIdent && !isKeywordKind(next.kind) {
				break
			}
			p.advance()
			s += "." + next.text
		}
		return Value{String: &s}, nil
	}
	return Value{}, p.errorf("expected number or string, got %q at position %d", t.text, t.pos)
}

// parseDottedField reads an identifier, possibly with dot-separated parts.
// Also accepts keywords used as field names (e.g. "os", "group", "count").
func (p *parser) parseDottedField() (string, error) {
	t := p.cur()
	if t.kind != tkIdent && !isKeywordKind(t.kind) {
		return "", p.errorf("expected field name, got %q at position %d", t.text, t.pos)
	}
	p.advance()
	result := strings.ToLower(t.text)

	for p.cur().kind == tkDot {
		p.advance()
		part := p.cur()
		if part.kind != tkIdent && !isKeywordKind(part.kind) {
			return "", p.errorf("expected field name after '.', got %q at position %d", part.text, part.pos)
		}
		p.advance()
		result += "." + strings.ToLower(part.text)
	}
	return result, nil
}

func (p *parser) parseFieldList() ([]string, error) {
	var fields []string
	for {
		f, err := p.parseDottedField()
		if err != nil {
			return nil, err
		}
		fields = append(fields, f)
		if p.cur().kind != tkComma {
			break
		}
		p.advance()
	}
	return fields, nil
}

func (p *parser) parseOrderByList() ([]*OrderByField, error) {
	var fields []*OrderByField
	for {
		f, err := p.parseDottedField()
		if err != nil {
			return nil, err
		}
		desc := false
		if p.cur().kind == tkDESC {
			p.advance()
			desc = true
		} else if p.cur().kind == tkASC {
			p.advance()
		}
		fields = append(fields, &OrderByField{Field: f, Desc: desc})
		if p.cur().kind != tkComma {
			break
		}
		p.advance()
	}
	return fields, nil
}

// ─────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────

func isAggKeyword(k tokenKind) bool {
	return k == tkCOUNT || k == tkAVG || k == tkMIN || k == tkMAX || k == tkSUM
}

func isKeywordKind(k tokenKind) bool {
	return k >= tkSELECT && k <= tkSUM
}

func kindName(k tokenKind) string {
	names := map[tokenKind]string{
		tkEOF:       "end of input",
		tkIdent:     "identifier",
		tkNumber:    "number",
		tkString:    "string",
		tkComma:     "','",
		tkDot:       "'.'",
		tkLParen:    "'('",
		tkRParen:    "')'",
		tkStar:      "'*'",
		tkColon:     "':'",
		tkEquals:    "'='",
		tkNotEquals: "'!='",
		tkLess:      "'<'",
		tkLessEq:    "'<='",
		tkGreater:   "'>'",
		tkGreaterEq: "'>='",
		tkSELECT:    "SELECT",
		tkFROM:      "FROM",
		tkWHERE:     "WHERE",
		tkBY:        "BY",
		tkNULL:      "NULL",
		tkLIMIT:     "LIMIT",
	}
	if n, ok := names[k]; ok {
		return n
	}
	return fmt.Sprintf("token(%d)", k)
}

