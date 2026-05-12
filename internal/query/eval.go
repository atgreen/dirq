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
	All   bool   // FROM *
	Kind  string // "tag" or "group"
	Value string // e.g. "prod", "webservers"
}

// ExtractTarget returns the target scope from the query's FROM clause.
// If no FROM clause is present, it defaults to all hosts.
func ExtractTarget(q *Query) TargetScope {
	if q.From == nil || q.From.All {
		return TargetScope{All: true}
	}
	return TargetScope{
		Kind:  q.From.Scope.Kind,
		Value: q.From.Scope.Value,
	}
}

// ToFilterProtos converts the WHERE clause conditions into Filter proto messages.
func ToFilterProtos(q *Query) []*dirqv1.Filter {
	if q.Where == nil {
		return nil
	}
	filters := make([]*dirqv1.Filter, 0, len(q.Where.Conditions))
	for _, c := range q.Where.Conditions {
		filters = append(filters, &dirqv1.Filter{
			Field:    c.Field,
			Operator: c.Operator,
			Value:    valueToString(c.Value),
		})
	}
	return filters
}

// Row is a map of field names to values used for in-memory filtering and aggregation.
type Row map[string]any

// MatchesWhere returns true if the row satisfies all WHERE conditions in the query.
func MatchesWhere(q *Query, row Row) (bool, error) {
	if q.Where == nil {
		return true, nil
	}
	for _, cond := range q.Where.Conditions {
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
	if v.Number != nil {
		return strconv.FormatFloat(*v.Number, 'f', -1, 64)
	}
	if v.String != nil {
		return *v.String
	}
	return ""
}

// AggregatedRow holds the result of a GROUP BY aggregation for one group.
type AggregatedRow struct {
	GroupKey map[string]any
	Values  map[string]any // aggregation results keyed by display name, e.g. "COUNT(hostname)"
}

// Aggregate applies GROUP BY and aggregation functions to a set of rows.
// It returns one AggregatedRow per distinct group.
func Aggregate(q *Query, rows []Row) ([]AggregatedRow, error) {
	if q.GroupBy == nil {
		return nil, fmt.Errorf("query has no GROUP BY clause")
	}

	// Group the rows.
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

	// Compute aggregations per group.
	result := make([]AggregatedRow, 0, len(groups))
	for _, gk := range groupOrder {
		g := groups[gk]
		ar := AggregatedRow{
			GroupKey: g.key,
			Values:  make(map[string]any),
		}

		// Copy group key fields into values.
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
