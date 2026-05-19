// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	dirqv1 "github.com/atgreen/dirq/proto/dirq/v1"
)

// IsTagField returns true if the field name is in the tag.* namespace.
func IsTagField(field string) bool {
	return strings.HasPrefix(field, "tag.")
}

// tagKeyFromField extracts the tag key from a tag.* field (e.g. "tag.env" → "env").
func tagKeyFromField(field string) string {
	return strings.TrimPrefix(field, "tag.")
}

// MatchesAgentTags evaluates tag.* conditions in the expression against an
// agent's tag map. Non-tag conditions (data fields) are treated as true —
// this conservatively includes agents that might match data conditions.
// Returns true if the agent should receive the query.
func MatchesAgentTags(expr Expr, tags map[string]string) bool {
	if expr == nil {
		return true
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		left := MatchesAgentTags(e.Left, tags)
		right := MatchesAgentTags(e.Right, tags)
		if e.Op == "AND" {
			return left && right
		}
		return left || right
	case *NotExpr:
		return !MatchesAgentTags(e.Expr, tags)
	case *CompareExpr:
		if !IsTagField(e.Field) {
			return true // data field — can't evaluate, assume match
		}
		key := tagKeyFromField(e.Field)
		actual, exists := tags[key]
		if !exists {
			return e.Operator == "!="
		}
		if e.Value.String != nil {
			switch e.Operator {
			case "=":
				return actual == *e.Value.String
			case "!=":
				return actual != *e.Value.String
			}
		}
		return false
	case *LikeExpr:
		if !IsTagField(e.Field) {
			return true
		}
		key := tagKeyFromField(e.Field)
		actual, exists := tags[key]
		if !exists {
			return e.Negated
		}
		result := matchLike(actual, e.Pattern)
		if e.Negated {
			return !result
		}
		return result
	case *InExpr:
		if !IsTagField(e.Field) {
			return true
		}
		key := tagKeyFromField(e.Field)
		actual, exists := tags[key]
		if !exists {
			return e.Negated
		}
		found := false
		for _, v := range e.Values {
			if actual == v {
				found = true
				break
			}
		}
		if e.Negated {
			return !found
		}
		return found
	case *IsNullExpr:
		if !IsTagField(e.Field) {
			return true
		}
		key := tagKeyFromField(e.Field)
		_, exists := tags[key]
		if e.Negated {
			return exists
		}
		return !exists
	}
	return true
}

// StripTagFields returns a new expression with tag.* conditions removed.
// For AND chains, tag conditions are dropped. For OR nodes that mix tag
// and data conditions, the expression is kept intact (agents will receive
// it and data conditions will be evaluated; tag conditions always pass
// since the agent was already pre-filtered).
// Returns nil if the entire expression was tag-only.
func StripTagFields(expr Expr) Expr {
	if expr == nil {
		return nil
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		if e.Op == "AND" {
			left := StripTagFields(e.Left)
			right := StripTagFields(e.Right)
			if left == nil {
				return right
			}
			if right == nil {
				return left
			}
			return &BinaryExpr{Op: "AND", Left: left, Right: right}
		}
		// OR: keep both sides. Tag conditions on agents that passed
		// pre-filtering will just evaluate to true harmlessly.
		return e
	case *NotExpr:
		inner := StripTagFields(e.Expr)
		if inner == nil {
			return nil
		}
		return &NotExpr{Expr: inner}
	case *CompareExpr:
		if IsTagField(e.Field) {
			return nil
		}
		return e
	case *LikeExpr:
		if IsTagField(e.Field) {
			return nil
		}
		return e
	case *InExpr:
		if IsTagField(e.Field) {
			return nil
		}
		return e
	case *IsNullExpr:
		if IsTagField(e.Field) {
			return nil
		}
		return e
	}
	return expr
}

// IsHostnameField returns true if the field is the bare "hostname" field.
func IsHostnameField(field string) bool {
	return field == "hostname"
}

// HasHostnameCondition returns true if the expression contains a bare hostname condition.
func HasHostnameCondition(expr Expr) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		return HasHostnameCondition(e.Left) || HasHostnameCondition(e.Right)
	case *NotExpr:
		return HasHostnameCondition(e.Expr)
	case *CompareExpr:
		return IsHostnameField(e.Field)
	case *LikeExpr:
		return IsHostnameField(e.Field)
	case *InExpr:
		return IsHostnameField(e.Field)
	case *IsNullExpr:
		return IsHostnameField(e.Field)
	}
	return false
}

// MatchesAgentRecord evaluates tag.* and hostname conditions against an agent's
// record. Non-tag, non-hostname conditions are treated as true (conservative).
func MatchesAgentRecord(expr Expr, tags map[string]string, hostname string) bool {
	if expr == nil {
		return true
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		left := MatchesAgentRecord(e.Left, tags, hostname)
		right := MatchesAgentRecord(e.Right, tags, hostname)
		if e.Op == "AND" {
			return left && right
		}
		return left || right
	case *NotExpr:
		return !MatchesAgentRecord(e.Expr, tags, hostname)
	case *CompareExpr:
		if IsHostnameField(e.Field) {
			if e.Value.String != nil {
				switch e.Operator {
				case "=":
					return hostname == *e.Value.String
				case "!=":
					return hostname != *e.Value.String
				}
			}
			return false
		}
		if !IsTagField(e.Field) {
			return true
		}
		return matchTagCompare(e, tags)
	case *LikeExpr:
		if IsHostnameField(e.Field) {
			result := matchLike(hostname, e.Pattern)
			if e.Negated {
				return !result
			}
			return result
		}
		if !IsTagField(e.Field) {
			return true
		}
		return matchTagLike(e, tags)
	case *InExpr:
		if IsHostnameField(e.Field) {
			found := false
			for _, v := range e.Values {
				if hostname == v {
					found = true
					break
				}
			}
			if e.Negated {
				return !found
			}
			return found
		}
		if !IsTagField(e.Field) {
			return true
		}
		return matchTagIn(e, tags)
	case *IsNullExpr:
		if IsHostnameField(e.Field) {
			if e.Negated {
				return hostname != ""
			}
			return hostname == ""
		}
		if !IsTagField(e.Field) {
			return true
		}
		return matchTagIsNull(e, tags)
	}
	return true
}

// matchTagCompare, matchTagLike, matchTagIn, matchTagIsNull extract the
// tag-matching logic from MatchesAgentTags for reuse in MatchesAgentRecord.

func matchTagCompare(e *CompareExpr, tags map[string]string) bool {
	key := tagKeyFromField(e.Field)
	actual, exists := tags[key]
	if !exists {
		return e.Operator == "!="
	}
	if e.Value.String != nil {
		switch e.Operator {
		case "=":
			return actual == *e.Value.String
		case "!=":
			return actual != *e.Value.String
		}
	}
	return false
}

func matchTagLike(e *LikeExpr, tags map[string]string) bool {
	key := tagKeyFromField(e.Field)
	actual, exists := tags[key]
	if !exists {
		return e.Negated
	}
	result := matchLike(actual, e.Pattern)
	if e.Negated {
		return !result
	}
	return result
}

func matchTagIn(e *InExpr, tags map[string]string) bool {
	key := tagKeyFromField(e.Field)
	actual, exists := tags[key]
	if !exists {
		return e.Negated
	}
	found := false
	for _, v := range e.Values {
		if actual == v {
			found = true
			break
		}
	}
	if e.Negated {
		return !found
	}
	return found
}

func matchTagIsNull(e *IsNullExpr, tags map[string]string) bool {
	key := tagKeyFromField(e.Field)
	_, exists := tags[key]
	if e.Negated {
		return exists
	}
	return !exists
}

// HasTagConditions returns true if the expression contains any tag.* references.
func HasTagConditions(expr Expr) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		return HasTagConditions(e.Left) || HasTagConditions(e.Right)
	case *NotExpr:
		return HasTagConditions(e.Expr)
	case *CompareExpr:
		return IsTagField(e.Field)
	case *LikeExpr:
		return IsTagField(e.Field)
	case *InExpr:
		return IsTagField(e.Field)
	case *IsNullExpr:
		return IsTagField(e.Field)
	}
	return false
}

// HasFieldConditions returns true if the expression contains conditions on
// agent-reported fields (e.g., os_info.os) that require querying agents to resolve.
func HasFieldConditions(expr Expr) bool {
	if expr == nil {
		return false
	}
	switch e := expr.(type) {
	case *BinaryExpr:
		return HasFieldConditions(e.Left) || HasFieldConditions(e.Right)
	case *NotExpr:
		return HasFieldConditions(e.Expr)
	case *CompareExpr:
		return !IsTagField(e.Field) && strings.Contains(e.Field, ".")
	case *LikeExpr:
		return !IsTagField(e.Field) && strings.Contains(e.Field, ".")
	case *InExpr:
		return !IsTagField(e.Field) && strings.Contains(e.Field, ".")
	case *IsNullExpr:
		return !IsTagField(e.Field) && strings.Contains(e.Field, ".")
	}
	return false
}

// ExtractModules returns the set of module names referenced in the query.
func ExtractModules(q *Query) []string {
	seen := make(map[string]bool)

	addField := func(f string) {
		parts := strings.SplitN(f, ".", 2)
		if len(parts) == 2 {
			seen[parts[0]] = true
		}
	}

	for _, s := range q.Select {
		if s.AggFunc != nil {
			addField(s.AggFunc.Arg)
		} else if !s.Star {
			addField(s.Field)
		}
	}
	if q.Where != nil {
		extractModulesFromExpr(q.Where, seen)
	}
	if q.GroupBy != nil {
		for _, f := range q.GroupBy.Fields {
			addField(f)
		}
	}
	if q.OrderBy != nil {
		for _, o := range q.OrderBy {
			addField(o.Field)
		}
	}

	modules := make([]string, 0, len(seen))
	for m := range seen {
		modules = append(modules, m)
	}
	sort.Strings(modules)
	if len(modules) == 0 {
		return []string{"cpu", "disk", "memory", "os_info"}
	}
	return modules
}

func extractModulesFromExpr(expr Expr, seen map[string]bool) {
	switch e := expr.(type) {
	case *BinaryExpr:
		extractModulesFromExpr(e.Left, seen)
		extractModulesFromExpr(e.Right, seen)
	case *NotExpr:
		extractModulesFromExpr(e.Expr, seen)
	case *CompareExpr:
		addFieldToSet(e.Field, seen)
	case *LikeExpr:
		addFieldToSet(e.Field, seen)
	case *InExpr:
		addFieldToSet(e.Field, seen)
	case *IsNullExpr:
		addFieldToSet(e.Field, seen)
	}
}

func addFieldToSet(f string, seen map[string]bool) {
	if IsTagField(f) {
		return // tag.* is metadata, not a data module
	}
	parts := strings.SplitN(f, ".", 2)
	if len(parts) == 2 {
		seen[parts[0]] = true
	}
}

// ─────────────────────────────────────────────────────────
// Proto filter pushdown (flat AND-only conditions)
// ─────────────────────────────────────────────────────────

// ToFilterProtos converts the WHERE clause into flat Filter proto messages
// for agent-side pushdown. Tag conditions are stripped (already handled
// server-side). Only works for pure AND chains of simple data conditions.
// Returns nil if the expression is too complex (OR, NOT, etc.),
// meaning the server should do the filtering.
func ToFilterProtos(q *Query) []*dirqv1.Filter {
	if q.Where == nil {
		return nil
	}
	dataExpr := StripTagFields(q.Where)
	if dataExpr == nil {
		return nil // all conditions were tag-based
	}
	conds := flattenANDs(dataExpr)
	if conds == nil {
		return nil // too complex for pushdown
	}
	return append(make([]*dirqv1.Filter, 0, len(conds)), conds...)
}

// flattenANDs extracts a list of simple filters from a pure AND chain.
// Returns nil if the expression contains OR, NOT, or other complex nodes.
func flattenANDs(expr Expr) []*dirqv1.Filter {
	switch e := expr.(type) {
	case *BinaryExpr:
		if e.Op != "AND" {
			return nil
		}
		left := flattenANDs(e.Left)
		right := flattenANDs(e.Right)
		if left == nil || right == nil {
			return nil
		}
		return append(left, right...)
	case *CompareExpr:
		return []*dirqv1.Filter{{
			Field:    e.Field,
			Operator: e.Operator,
			Value:    valueToString(&e.Value),
		}}
	case *LikeExpr:
		if e.Negated {
			return nil
		}
		return []*dirqv1.Filter{{
			Field:    e.Field,
			Operator: "LIKE",
			Value:    e.Pattern,
		}}
	case *InExpr:
		if e.Negated {
			return nil
		}
		return []*dirqv1.Filter{{
			Field:    e.Field,
			Operator: "IN",
			Value:    strings.Join(e.Values, ","),
		}}
	default:
		return nil
	}
}

// ─────────────────────────────────────────────────────────
// Array-aware filtering
// ─────────────────────────────────────────────────────────

// arrayModuleKeys maps module names to the array key within their collected data.
var arrayModuleKeys = map[string]string{
	"packages": "packages",
	"services": "services",
	"disk":     "partitions",
	"network":  "interfaces",
}

// FilterCollectedData applies WHERE conditions to the collected module data.
// For array modules (packages, services, disk, network), it filters the array
// elements — only entries matching the conditions are kept.
// For scalar modules (cpu, memory, os_info), it's a pass/fail check.
//
// Returns the filtered data map (modules with no matching data are removed).
func FilterCollectedData(conditions []*Condition, data map[string]any) map[string]any {
	if len(conditions) == 0 {
		return data
	}

	// Group conditions by module.
	moduleConditions := map[string][]*Condition{}
	for _, c := range conditions {
		parts := strings.SplitN(c.Field, ".", 2)
		if len(parts) == 2 {
			moduleConditions[parts[0]] = append(moduleConditions[parts[0]], c)
		}
	}

	result := make(map[string]any, len(data))
	for module, moduleData := range data {
		conds, hasConds := moduleConditions[module]
		if !hasConds {
			result[module] = moduleData
			continue
		}

		arrayKey, isArray := arrayModuleKeys[module]
		if !isArray {
			if md, ok := moduleData.(map[string]any); ok {
				if matchesScalarConditions(module, conds, md) {
					result[module] = moduleData
				}
			}
			continue
		}

		md, ok := moduleData.(map[string]any)
		if !ok {
			continue
		}
		arr, ok := md[arrayKey]
		if !ok {
			continue
		}

		items, ok := arr.([]any)
		if !ok {
			continue
		}

		var filtered []any
		for _, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if matchesArrayEntry(module, conds, entry) {
				filtered = append(filtered, entry)
			}
		}

		if len(filtered) > 0 {
			result[module] = map[string]any{
				arrayKey: filtered,
			}
		}
	}

	return result
}

// AllFilteredModulesPresent returns true if every module referenced in a WHERE
// condition still has data after filtering.
func AllFilteredModulesPresent(conditions []*Condition, data map[string]any) bool {
	for _, c := range conditions {
		parts := strings.SplitN(c.Field, ".", 2)
		if len(parts) == 2 {
			if _, ok := data[parts[0]]; !ok {
				return false
			}
		}
	}
	return true
}

func matchesScalarConditions(module string, conds []*Condition, data map[string]any) bool {
	for _, c := range conds {
		field := stripModulePrefix(module, c.Field)
		val, ok := data[field]
		if !ok {
			return false
		}
		if !evalLegacyCondition(c, val) {
			return false
		}
	}
	return true
}

func matchesArrayEntry(module string, conds []*Condition, entry map[string]any) bool {
	for _, c := range conds {
		field := stripModulePrefix(module, c.Field)
		val, ok := entry[field]
		if !ok {
			return false
		}
		if !evalLegacyCondition(c, val) {
			return false
		}
	}
	return true
}

// evalLegacyCondition checks a single flat Condition against a value.
func evalLegacyCondition(c *Condition, actual any) bool {
	if c.In != nil {
		actualStr := fmt.Sprintf("%v", actual)
		for _, v := range c.In.Values {
			if actualStr == v {
				return true
			}
		}
		return false
	}

	op := c.Operator
	if c.Value == nil {
		return false
	}

	if c.Value.String != nil {
		expected := *c.Value.String
		actualStr := fmt.Sprintf("%v", actual)
		switch op {
		case "=":
			return actualStr == expected
		case "!=":
			return actualStr != expected
		case "LIKE":
			return matchLike(actualStr, expected)
		}
		return false
	}

	if c.Value.Number != nil {
		expected := *c.Value.Number
		actualNum, err := toFloat64(actual)
		if err != nil {
			return false
		}
		return compareNumbers(actualNum, op, expected)
	}

	return false
}

func stripModulePrefix(module, field string) string {
	prefix := module + "."
	if strings.HasPrefix(field, prefix) {
		return field[len(prefix):]
	}
	return field
}

// ─────────────────────────────────────────────────────────
// Expression-tree evaluation
// ─────────────────────────────────────────────────────────

// Row is a map of field names to values used for in-memory filtering and aggregation.
type Row map[string]any

// MatchesWhere returns true if the row satisfies the WHERE expression.
func MatchesWhere(q *Query, row Row) (bool, error) {
	if q.Where == nil {
		return true, nil
	}
	return evalExpr(q.Where, row)
}

func evalExpr(expr Expr, row Row) (bool, error) {
	switch e := expr.(type) {
	case *BinaryExpr:
		left, err := evalExpr(e.Left, row)
		if err != nil {
			return false, err
		}
		if e.Op == "AND" {
			if !left {
				return false, nil // short-circuit
			}
			return evalExpr(e.Right, row)
		}
		// OR
		if left {
			return true, nil // short-circuit
		}
		return evalExpr(e.Right, row)

	case *NotExpr:
		val, err := evalExpr(e.Expr, row)
		if err != nil {
			return false, err
		}
		return !val, nil

	case *CompareExpr:
		val, ok := row[e.Field]
		if !ok {
			return false, nil
		}
		return evalCompare(e, val)

	case *LikeExpr:
		val, ok := row[e.Field]
		if !ok {
			return false, nil
		}
		actualStr := fmt.Sprintf("%v", val)
		result := matchLike(actualStr, e.Pattern)
		if e.Negated {
			return !result, nil
		}
		return result, nil

	case *InExpr:
		val, ok := row[e.Field]
		if !ok {
			return false, nil
		}
		actualStr := fmt.Sprintf("%v", val)
		found := false
		for _, v := range e.Values {
			if actualStr == v {
				found = true
				break
			}
		}
		if e.Negated {
			return !found, nil
		}
		return found, nil

	case *IsNullExpr:
		_, ok := row[e.Field]
		if e.Negated {
			return ok, nil // IS NOT NULL → field exists
		}
		return !ok, nil // IS NULL → field missing

	default:
		return false, fmt.Errorf("unknown expression type %T", expr)
	}
}

func evalCompare(e *CompareExpr, actual any) (bool, error) {
	if e.Value.String != nil {
		expected := *e.Value.String
		actualStr := fmt.Sprintf("%v", actual)
		switch e.Operator {
		case "=":
			return actualStr == expected, nil
		case "!=":
			return actualStr != expected, nil
		default:
			return false, fmt.Errorf("unsupported string operator: %s", e.Operator)
		}
	}

	if e.Value.Number != nil {
		expected := *e.Value.Number
		actualNum, err := toFloat64(actual)
		if err != nil {
			return false, fmt.Errorf("field %s: %w", e.Field, err)
		}
		return compareNumbers(actualNum, e.Operator, expected), nil
	}

	return false, fmt.Errorf("condition has no value")
}

func compareNumbers(actual float64, op string, expected float64) bool {
	switch op {
	case "=":
		return actual == expected
	case "!=":
		return actual != expected
	case ">":
		return actual > expected
	case "<":
		return actual < expected
	case ">=":
		return actual >= expected
	case "<=":
		return actual <= expected
	default:
		return false
	}
}

// matchLike provides SQL LIKE matching where % matches any sequence and _ matches one character.
func matchLike(s, pattern string) bool {
	return matchLikeDP(strings.ToLower(s), strings.ToLower(pattern), 0, 0)
}

func matchLikeDP(s, p string, si, pi int) bool {
	for pi < len(p) {
		if p[pi] == '%' {
			pi++
			// Skip consecutive %
			for pi < len(p) && p[pi] == '%' {
				pi++
			}
			if pi == len(p) {
				return true
			}
			for i := si; i <= len(s); i++ {
				if matchLikeDP(s, p, i, pi) {
					return true
				}
			}
			return false
		}
		if si >= len(s) {
			return false
		}
		if p[pi] == '_' || p[pi] == s[si] {
			si++
			pi++
		} else {
			return false
		}
	}
	return si == len(s)
}

func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		return n, nil
	case float32:
		return float64(n), nil
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case int32:
		return float64(n), nil
	case string:
		return strconv.ParseFloat(n, 64)
	default:
		return 0, fmt.Errorf("cannot convert %T to float64", v)
	}
}

func valueToString(v *Value) string {
	if v == nil {
		return ""
	}
	if v.Number != nil {
		return strconv.FormatFloat(*v.Number, 'f', -1, 64)
	}
	if v.String != nil {
		return *v.String
	}
	return ""
}

// ─────────────────────────────────────────────────────────
// Aggregation
// ─────────────────────────────────────────────────────────

// AggregatedRow holds the result of a GROUP BY aggregation for one group.
type AggregatedRow struct {
	GroupKey map[string]any
	Values  map[string]any
}

// Aggregate applies aggregation functions to a set of rows. If a GROUP BY
// clause is present, rows are grouped first. If no GROUP BY is present
// (bare aggregate, e.g. SELECT COUNT(hostname)), all rows are treated as
// a single group.
func Aggregate(q *Query, rows []Row) ([]AggregatedRow, error) {
	if q.GroupBy == nil {
		return aggregateAll(q, rows)
	}

	type group struct {
		key  map[string]any
		rows []Row
	}
	groups := make(map[string]*group)
	var groupOrder []string

	for _, row := range rows {
		keyParts := make([]string, 0, len(q.GroupBy.Fields))
		keyMap := make(map[string]any, len(q.GroupBy.Fields))
		for _, f := range q.GroupBy.Fields {
			v := row[f]
			keyParts = append(keyParts, fmt.Sprintf("%v", v))
			keyMap[f] = v
		}
		gk := strings.Join(keyParts, "\x00")
		if _, ok := groups[gk]; !ok {
			groups[gk] = &group{key: keyMap}
			groupOrder = append(groupOrder, gk)
		}
		groups[gk].rows = append(groups[gk].rows, row)
	}

	result := make([]AggregatedRow, 0, len(groups))
	for _, gk := range groupOrder {
		g := groups[gk]
		ar := AggregatedRow{
			GroupKey: g.key,
			Values:  make(map[string]any),
		}
		for k, v := range g.key {
			ar.Values[k] = v
		}
		for _, sel := range q.Select {
			if sel.AggFunc == nil {
				continue
			}
			displayName := sel.AggFunc.Name + "(" + sel.AggFunc.Arg + ")"
			val, err := computeAgg(sel.AggFunc, g.rows)
			if err != nil {
				return nil, fmt.Errorf("aggregation %s: %w", displayName, err)
			}
			ar.Values[displayName] = val
		}
		result = append(result, ar)
	}

	return result, nil
}

// aggregateAll computes aggregate functions over all rows as a single group
// (bare aggregate without GROUP BY).
func aggregateAll(q *Query, rows []Row) ([]AggregatedRow, error) {
	ar := AggregatedRow{
		Values: make(map[string]any),
	}
	for _, sel := range q.Select {
		if sel.AggFunc == nil {
			continue
		}
		displayName := sel.AggFunc.Name + "(" + sel.AggFunc.Arg + ")"
		val, err := computeAgg(sel.AggFunc, rows)
		if err != nil {
			return nil, fmt.Errorf("aggregation %s: %w", displayName, err)
		}
		ar.Values[displayName] = val
	}
	return []AggregatedRow{ar}, nil
}

func computeAgg(fn *AggFunc, rows []Row) (any, error) {
	switch fn.Name {
	case "COUNT":
		count := 0
		for _, r := range rows {
			if _, ok := r[fn.Arg]; ok {
				count++
			}
		}
		return count, nil
	case "SUM":
		var sum float64
		for _, r := range rows {
			v, ok := r[fn.Arg]
			if !ok {
				continue
			}
			n, err := toFloat64(v)
			if err != nil {
				return nil, err
			}
			sum += n
		}
		return sum, nil
	case "AVG":
		var sum float64
		var count int
		for _, r := range rows {
			v, ok := r[fn.Arg]
			if !ok {
				continue
			}
			n, err := toFloat64(v)
			if err != nil {
				return nil, err
			}
			sum += n
			count++
		}
		if count == 0 {
			return 0.0, nil
		}
		return sum / float64(count), nil
	case "MIN":
		min := math.MaxFloat64
		for _, r := range rows {
			v, ok := r[fn.Arg]
			if !ok {
				continue
			}
			n, err := toFloat64(v)
			if err != nil {
				return nil, err
			}
			if n < min {
				min = n
			}
		}
		if min == math.MaxFloat64 {
			return 0.0, nil
		}
		return min, nil
	case "MAX":
		max := -math.MaxFloat64
		for _, r := range rows {
			v, ok := r[fn.Arg]
			if !ok {
				continue
			}
			n, err := toFloat64(v)
			if err != nil {
				return nil, err
			}
			if n > max {
				max = n
			}
		}
		if max == -math.MaxFloat64 {
			return 0.0, nil
		}
		return max, nil
	default:
		return nil, fmt.Errorf("unknown aggregation function: %s", fn.Name)
	}
}

// SortRows sorts rows according to the ORDER BY clause.
func SortRows(q *Query, rows []Row) {
	if len(q.OrderBy) == 0 {
		return
	}

	sort.SliceStable(rows, func(i, j int) bool {
		for _, ob := range q.OrderBy {
			cmp := compareRowValues(rows[i][ob.Field], rows[j][ob.Field])
			if cmp == 0 {
				continue
			}
			if ob.Desc {
				return cmp > 0
			}
			return cmp < 0
		}
		return false
	})
}

// compareRowValues compares two values for sorting. Supports both strings and numbers.
func compareRowValues(a, b any) int {
	// Try numeric comparison first.
	na, errA := toFloat64(a)
	nb, errB := toFloat64(b)
	if errA == nil && errB == nil {
		if na < nb {
			return -1
		}
		if na > nb {
			return 1
		}
		return 0
	}
	// Fall back to string comparison.
	sa := fmt.Sprintf("%v", a)
	sb := fmt.Sprintf("%v", b)
	if sa < sb {
		return -1
	}
	if sa > sb {
		return 1
	}
	return 0
}
