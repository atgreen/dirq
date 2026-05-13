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

// TargetScope describes the target scope of a query.
type TargetScope struct {
	All      bool   // FROM *
	TagKey   string // tag key to match (e.g. "env", "group")
	TagValue string // tag value to match, empty = any value for that key
}

// ExtractTarget returns the target scope from the query's FROM clause.
// If no FROM clause is present, it defaults to all hosts.
//
//	FROM *              → All=true
//	FROM tag:env=prod   → TagKey="env", TagValue="prod"
//	FROM tag:env        → TagKey="env", TagValue="" (any value)
//	FROM group:web      → TagKey="group", TagValue="web"
func ExtractTarget(q *Query) TargetScope {
	if q.From == nil || q.From.All {
		return TargetScope{All: true}
	}
	scope := q.From.Scope
	if scope.Kind == "group" {
		return TargetScope{
			TagKey:   "group",
			TagValue: scope.Key, // group:webservers → key="group", value="webservers"
		}
	}
	// tag:env=prod or tag:env
	return TargetScope{
		TagKey:   scope.Key,
		TagValue: scope.Value,
	}
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
		} else {
			addField(s.Field)
		}
	}
	if q.Where != nil {
		for _, c := range q.Where.Conditions {
			addField(c.Field)
		}
	}
	if q.GroupBy != nil {
		for _, f := range q.GroupBy.Fields {
			addField(f)
		}
	}
	if q.OrderBy != nil {
		addField(q.OrderBy.Field)
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

// ToFilterProtos converts the WHERE clause conditions into Filter proto messages.
func ToFilterProtos(q *Query) []*dirqv1.Filter {
	if q.Where == nil {
		return nil
	}
	filters := make([]*dirqv1.Filter, 0, len(q.Where.Conditions))
	for _, c := range q.Where.Conditions {
		if c.In != nil {
			// IN clause: encode as operator="IN", value=comma-separated list.
			filters = append(filters, &dirqv1.Filter{
				Field:    c.Field,
				Operator: "IN",
				Value:    strings.Join(c.In.Values, ","),
			})
		} else {
			filters = append(filters, &dirqv1.Filter{
				Field:    c.Field,
				Operator: c.Operator,
				Value:    valueToString(c.Value),
			})
		}
	}
	return filters
}

// ─────────────────────────────────────────────────────────
// Array-aware filtering
// ─────────────────────────────────────────────────────────

// arrayModuleKeys maps module names to the array key within their collected data.
// When a WHERE condition references a field like "packages.name", we know that
// "packages" contains an array at key "packages", and we filter its elements.
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
			// No filters for this module — include as-is.
			result[module] = moduleData
			continue
		}

		arrayKey, isArray := arrayModuleKeys[module]
		if !isArray {
			// Scalar module — check conditions against the module data map.
			if md, ok := moduleData.(map[string]any); ok {
				if matchesScalarConditions(module, conds, md) {
					result[module] = moduleData
				}
			}
			continue
		}

		// Array module — filter the array elements.
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

// matchesScalarConditions checks whether a scalar module's data satisfies all conditions.
func matchesScalarConditions(module string, conds []*Condition, data map[string]any) bool {
	for _, c := range conds {
		field := stripModulePrefix(module, c.Field)
		val, ok := data[field]
		if !ok {
			return false
		}
		if !evalConditionValue(c, val) {
			return false
		}
	}
	return true
}

// matchesArrayEntry checks whether a single array element satisfies all conditions.
func matchesArrayEntry(module string, conds []*Condition, entry map[string]any) bool {
	for _, c := range conds {
		field := stripModulePrefix(module, c.Field)
		val, ok := entry[field]
		if !ok {
			return false
		}
		if !evalConditionValue(c, val) {
			return false
		}
	}
	return true
}

// evalConditionValue checks a single condition against a value.
func evalConditionValue(c *Condition, actual any) bool {
	// IN clause.
	if c.In != nil {
		actualStr := fmt.Sprintf("%v", actual)
		for _, v := range c.In.Values {
			if actualStr == v {
				return true
			}
		}
		return false
	}

	// Standard operators.
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
		switch op {
		case "=":
			return actualNum == expected
		case "!=":
			return actualNum != expected
		case ">":
			return actualNum > expected
		case "<":
			return actualNum < expected
		case ">=":
			return actualNum >= expected
		case "<=":
			return actualNum <= expected
		}
	}

	return false
}

// stripModulePrefix removes the module prefix from a dotted field path.
// "packages.name" with module "packages" returns "name".
func stripModulePrefix(module, field string) string {
	prefix := module + "."
	if strings.HasPrefix(field, prefix) {
		return field[len(prefix):]
	}
	return field
}

// ─────────────────────────────────────────────────────────
// Row-based evaluation (used for server-side aggregation)
// ─────────────────────────────────────────────────────────

// Row is a map of field names to values used for in-memory filtering and aggregation.
type Row map[string]any

// MatchesWhere returns true if the row satisfies all WHERE conditions in the query.
func MatchesWhere(q *Query, row Row) (bool, error) {
	if q.Where == nil {
		return true, nil
	}
	for _, cond := range q.Where.Conditions {
		if cond.In != nil {
			val, ok := row[cond.Field]
			if !ok {
				return false, nil
			}
			if !evalConditionValue(cond, val) {
				return false, nil
			}
			continue
		}
		val, ok := row[cond.Field]
		if !ok {
			return false, nil
		}
		match, err := evalCondition(cond, val)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}
	return true, nil
}

func evalCondition(cond *Condition, actual any) (bool, error) {
	op := cond.Operator

	if cond.Value == nil {
		return false, fmt.Errorf("condition has no value")
	}

	// String comparison.
	if cond.Value.String != nil {
		expected := *cond.Value.String
		actualStr := fmt.Sprintf("%v", actual)
		switch op {
		case "=":
			return actualStr == expected, nil
		case "!=":
			return actualStr != expected, nil
		case "LIKE":
			return matchLike(actualStr, expected), nil
		default:
			return false, fmt.Errorf("unsupported string operator: %s", op)
		}
	}

	// Numeric comparison.
	if cond.Value.Number != nil {
		expected := *cond.Value.Number
		actualNum, err := toFloat64(actual)
		if err != nil {
			return false, fmt.Errorf("field %s: %w", cond.Field, err)
		}
		switch op {
		case "=":
			return actualNum == expected, nil
		case "!=":
			return actualNum != expected, nil
		case ">":
			return actualNum > expected, nil
		case "<":
			return actualNum < expected, nil
		case ">=":
			return actualNum >= expected, nil
		case "<=":
			return actualNum <= expected, nil
		default:
			return false, fmt.Errorf("unsupported numeric operator: %s", op)
		}
	}

	return false, fmt.Errorf("condition has no value")
}

// matchLike provides basic SQL LIKE matching where % matches any sequence of characters.
func matchLike(s, pattern string) bool {
	pattern = strings.ToLower(pattern)
	s = strings.ToLower(s)

	if strings.HasPrefix(pattern, "%") && strings.HasSuffix(pattern, "%") {
		return strings.Contains(s, pattern[1:len(pattern)-1])
	}
	if strings.HasPrefix(pattern, "%") {
		return strings.HasSuffix(s, pattern[1:])
	}
	if strings.HasSuffix(pattern, "%") {
		return strings.HasPrefix(s, pattern[:len(pattern)-1])
	}
	return s == pattern
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

// Aggregate applies GROUP BY and aggregation functions to a set of rows.
func Aggregate(q *Query, rows []Row) ([]AggregatedRow, error) {
	if q.GroupBy == nil {
		return nil, fmt.Errorf("query has no GROUP BY clause")
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
	if q.OrderBy == nil {
		return
	}
	field := q.OrderBy.Field
	desc := q.OrderBy.Desc

	sort.SliceStable(rows, func(i, j int) bool {
		vi, _ := toFloat64(rows[i][field])
		vj, _ := toFloat64(rows[j][field])
		if desc {
			return vi > vj
		}
		return vi < vj
	})
}
