// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"sort"
	"strings"

	"github.com/atgreen/dirq/internal/db"
)

// SimpleFilter is a field/operator/value triple for agent-side filtering.
type SimpleFilter struct {
	Field    string
	Operator string
	Value    string
}

// ExtractTargetFilter converts a parsed query's FROM clause into a db.ListAgentsFilter.
func ExtractTargetFilter(q *Query) db.ListAgentsFilter {
	if q == nil || q.From == nil || q.From.All {
		return db.ListAgentsFilter{}
	}

	scope := q.From.Scope
	if scope == nil {
		return db.ListAgentsFilter{}
	}

	filter := db.ListAgentsFilter{}
	switch scope.Kind {
	case "tag":
		filter.Tag = scope.Kind
		filter.TagValue = scope.Value
	case "group":
		filter.Tag = "group"
		filter.TagValue = scope.Value
	}
	return filter
}

// ExtractModules returns the list of data modules needed by the query.
func ExtractModules(q *Query) []string {
	if q == nil {
		return nil
	}

	moduleSet := map[string]bool{}

	for _, f := range q.Select {
		if f.AggFunc != nil {
			mod := extractModule(f.AggFunc.Arg)
			if mod != "" {
				moduleSet[mod] = true
			}
		} else {
			mod := extractModule(f.Field)
			if mod != "" {
				moduleSet[mod] = true
			}
		}
	}

	if q.Where != nil {
		for _, c := range q.Where.Conditions {
			mod := extractModule(c.Field)
			if mod != "" {
				moduleSet[mod] = true
			}
		}
	}

	if q.OrderBy != nil {
		mod := extractModule(q.OrderBy.Field)
		if mod != "" {
			moduleSet[mod] = true
		}
	}

	if q.GroupBy != nil {
		for _, f := range q.GroupBy.Fields {
			mod := extractModule(f)
			if mod != "" {
				moduleSet[mod] = true
			}
		}
	}

	modules := make([]string, 0, len(moduleSet))
	for m := range moduleSet {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	if len(modules) == 0 {
		return []string{"cpu", "disk", "memory", "os_info"}
	}
	return modules
}

// ExtractFilters returns simple filter triples for agent-side evaluation.
func ExtractFilters(q *Query) []SimpleFilter {
	if q == nil || q.Where == nil {
		return nil
	}

	var filters []SimpleFilter
	for _, c := range q.Where.Conditions {
		filters = append(filters, SimpleFilter{
			Field:    c.Field,
			Operator: c.Operator,
			Value:    valueToString(c.Value),
		})
	}
	return filters
}

// extractModule gets the module name from a dotted field like "disk.pct_used".
func extractModule(field string) string {
	parts := strings.SplitN(field, ".", 2)
	if len(parts) == 2 {
		return parts[0]
	}
	return ""
}
