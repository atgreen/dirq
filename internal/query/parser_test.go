// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package query

import (
	"testing"
)

func TestParseSelectWhereOrderBy(t *testing.T) {
	input := `SELECT hostname, disk.mount, disk.pct_used WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC`

	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// SELECT
	if len(q.Select) != 3 {
		t.Fatalf("expected 3 select exprs, got %d", len(q.Select))
	}
	wantFields := []string{"hostname", "disk.mount", "disk.pct_used"}
	for i, want := range wantFields {
		if q.Select[i].Field != want {
			t.Errorf("select[%d]: got %q, want %q", i, q.Select[i].Field, want)
		}
	}

	// WHERE
	if q.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	cmp, ok := q.Where.(*CompareExpr)
	if !ok {
		t.Fatalf("expected CompareExpr, got %T", q.Where)
	}
	if cmp.Field != "disk.pct_used" || cmp.Operator != ">" || *cmp.Value.Number != 80 {
		t.Errorf("condition: got %s %s %v, want disk.pct_used > 80", cmp.Field, cmp.Operator, *cmp.Value.Number)
	}

	// ORDER BY
	if len(q.OrderBy) != 1 {
		t.Fatalf("expected 1 ORDER BY field, got %d", len(q.OrderBy))
	}
	if q.OrderBy[0].Field != "disk.pct_used" || !q.OrderBy[0].Desc {
		t.Errorf("ORDER BY: got %s desc=%v, want disk.pct_used DESC", q.OrderBy[0].Field, q.OrderBy[0].Desc)
	}
}

func TestParseCaseInsensitive(t *testing.T) {
	inputs := []string{
		`select hostname where os = 'linux'`,
		`Select Hostname Where Os = 'linux'`,
		`SELECT hostname WHERE os = 'linux'`,
		`sElEcT hostname wHeRe os = 'linux'`,
	}
	for _, input := range inputs {
		q, err := Parse(input)
		if err != nil {
			t.Errorf("failed to parse %q: %v", input, err)
			continue
		}
		if len(q.Select) != 1 || q.Select[0].Field != "hostname" {
			t.Errorf("parse %q: wrong select", input)
		}
	}
}

func TestParseAggregation(t *testing.T) {
	input := `SELECT os, COUNT(hostname), AVG(memory.total_gb) GROUP BY os`

	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(q.Select) != 3 {
		t.Fatalf("expected 3 select exprs, got %d", len(q.Select))
	}

	if q.Select[0].Field != "os" {
		t.Errorf("select[0]: got %q, want 'os'", q.Select[0].Field)
	}

	if q.Select[1].AggFunc == nil {
		t.Fatal("select[1]: expected AggFunc")
	}
	if q.Select[1].AggFunc.Name != "COUNT" || q.Select[1].AggFunc.Arg != "hostname" {
		t.Errorf("select[1]: got %s(%s), want COUNT(hostname)", q.Select[1].AggFunc.Name, q.Select[1].AggFunc.Arg)
	}

	if q.Select[2].AggFunc == nil {
		t.Fatal("select[2]: expected AggFunc")
	}
	if q.Select[2].AggFunc.Name != "AVG" || q.Select[2].AggFunc.Arg != "memory.total_gb" {
		t.Errorf("select[2]: got %s(%s), want AVG(memory.total_gb)", q.Select[2].AggFunc.Name, q.Select[2].AggFunc.Arg)
	}

	if q.GroupBy == nil {
		t.Fatal("expected GROUP BY clause")
	}
	if len(q.GroupBy.Fields) != 1 || q.GroupBy.Fields[0] != "os" {
		t.Errorf("GROUP BY: got %v, want [os]", q.GroupBy.Fields)
	}
}

func TestParseTagWhere(t *testing.T) {
	input := `SELECT hostname, cpu.cores WHERE tag.group = 'webservers' AND cpu.cores >= 8`

	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if q.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	and, ok := q.Where.(*BinaryExpr)
	if !ok || and.Op != "AND" {
		t.Fatalf("expected AND, got %T", q.Where)
	}
	tagCmp, ok := and.Left.(*CompareExpr)
	if !ok || tagCmp.Field != "tag.group" || *tagCmp.Value.String != "webservers" {
		t.Error("left side should be tag.group = webservers")
	}
	dataCmp, ok := and.Right.(*CompareExpr)
	if !ok || dataCmp.Field != "cpu.cores" || *dataCmp.Value.Number != 8 {
		t.Error("right side should be cpu.cores >= 8")
	}
}

func TestParseSelectStar(t *testing.T) {
	q, err := Parse(`SELECT *`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(q.Select) != 1 || !q.Select[0].Star {
		t.Error("expected SELECT *")
	}
}

func TestParseOR(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE os = 'linux' OR os = 'windows'`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	bin, ok := q.Where.(*BinaryExpr)
	if !ok || bin.Op != "OR" {
		t.Fatalf("expected OR BinaryExpr, got %T", q.Where)
	}
	left, ok := bin.Left.(*CompareExpr)
	if !ok || left.Field != "os" || *left.Value.String != "linux" {
		t.Error("left side wrong")
	}
	right, ok := bin.Right.(*CompareExpr)
	if !ok || right.Field != "os" || *right.Value.String != "windows" {
		t.Error("right side wrong")
	}
}

func TestParseANDBindsTighterThanOR(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE os = 'linux' OR cpu.cores > 4 AND memory.total_gb > 8`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	or, ok := q.Where.(*BinaryExpr)
	if !ok || or.Op != "OR" {
		t.Fatalf("expected OR at top, got %T", q.Where)
	}
	and, ok := or.Right.(*BinaryExpr)
	if !ok || and.Op != "AND" {
		t.Fatalf("expected AND on right, got %T", or.Right)
	}
}

func TestParseParentheses(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE (os = 'linux' OR os = 'freebsd') AND cpu.cores > 4`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	and, ok := q.Where.(*BinaryExpr)
	if !ok || and.Op != "AND" {
		t.Fatalf("expected AND at top, got %T", q.Where)
	}
	or, ok := and.Left.(*BinaryExpr)
	if !ok || or.Op != "OR" {
		t.Fatalf("expected OR on left, got %T", and.Left)
	}
}

func TestParseNOT(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE NOT os = 'windows'`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	not, ok := q.Where.(*NotExpr)
	if !ok {
		t.Fatalf("expected NotExpr, got %T", q.Where)
	}
	cmp, ok := not.Expr.(*CompareExpr)
	if !ok || *cmp.Value.String != "windows" {
		t.Error("NOT contents wrong")
	}
}

func TestParseNOTIN(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE os NOT IN ('windows', 'macos')`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	in, ok := q.Where.(*InExpr)
	if !ok {
		t.Fatalf("expected InExpr, got %T", q.Where)
	}
	if !in.Negated {
		t.Error("expected negated IN")
	}
	if len(in.Values) != 2 {
		t.Errorf("expected 2 values, got %d", len(in.Values))
	}
}

func TestParseISNULL(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE cpu.model IS NOT NULL`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	isn, ok := q.Where.(*IsNullExpr)
	if !ok {
		t.Fatalf("expected IsNullExpr, got %T", q.Where)
	}
	if isn.Field != "cpu.model" || !isn.Negated {
		t.Error("IS NOT NULL wrong")
	}
}

func TestParseLimit(t *testing.T) {
	q, err := Parse(`SELECT hostname LIMIT 10`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Limit != 10 {
		t.Errorf("expected LIMIT 10, got %d", q.Limit)
	}
}

func TestParseMultipleOrderBy(t *testing.T) {
	q, err := Parse(`SELECT hostname, os ORDER BY os ASC, hostname DESC`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(q.OrderBy) != 2 {
		t.Fatalf("expected 2 ORDER BY fields, got %d", len(q.OrderBy))
	}
	if q.OrderBy[0].Field != "os" || q.OrderBy[0].Desc {
		t.Errorf("order[0]: got %s desc=%v, want os ASC", q.OrderBy[0].Field, q.OrderBy[0].Desc)
	}
	if q.OrderBy[1].Field != "hostname" || !q.OrderBy[1].Desc {
		t.Errorf("order[1]: got %s desc=%v, want hostname DESC", q.OrderBy[1].Field, q.OrderBy[1].Desc)
	}
}

func TestParseLegacyFromIgnored(t *testing.T) {
	// FROM should be accepted but ignored for backward compatibility.
	q, err := Parse(`SELECT hostname FROM * WHERE os = 'linux'`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	cmp, ok := q.Where.(*CompareExpr)
	if !ok || *cmp.Value.String != "linux" {
		t.Error("WHERE not parsed correctly after FROM *")
	}
}

func TestParseLegacyFromTagIgnored(t *testing.T) {
	q, err := Parse(`SELECT hostname FROM tag:env=prod WHERE disk.pct_used > 80`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.Where == nil {
		t.Fatal("expected WHERE clause")
	}
}

func TestParseLike(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE hostname LIKE 'web%'`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	like, ok := q.Where.(*LikeExpr)
	if !ok {
		t.Fatalf("expected LikeExpr, got %T", q.Where)
	}
	if like.Field != "hostname" || like.Pattern != "web%" || like.Negated {
		t.Errorf("LIKE: got field=%s pattern=%s negated=%v", like.Field, like.Pattern, like.Negated)
	}
}

func TestParseNotLike(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE hostname NOT LIKE '%test%'`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	like, ok := q.Where.(*LikeExpr)
	if !ok {
		t.Fatalf("expected LikeExpr, got %T", q.Where)
	}
	if !like.Negated {
		t.Error("expected negated LIKE")
	}
}

func TestExtractModules(t *testing.T) {
	q, err := Parse(`SELECT hostname, disk.mount, disk.pct_used WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC`)
	if err != nil {
		t.Fatal(err)
	}
	modules := ExtractModules(q)
	if len(modules) != 1 || modules[0] != "disk" {
		t.Errorf("modules: got %v, want [disk]", modules)
	}
}

func TestExtractModulesSkipsTags(t *testing.T) {
	q, err := Parse(`SELECT hostname WHERE tag.env = 'prod' AND disk.pct_used > 80`)
	if err != nil {
		t.Fatal(err)
	}
	modules := ExtractModules(q)
	if len(modules) != 1 || modules[0] != "disk" {
		t.Errorf("modules: got %v, want [disk] (tag.* should be excluded)", modules)
	}
}

func TestExtractModulesMultiple(t *testing.T) {
	q, err := Parse(`SELECT os, COUNT(hostname), AVG(memory.total_gb) GROUP BY os`)
	if err != nil {
		t.Fatal(err)
	}
	modules := ExtractModules(q)
	if len(modules) != 1 || modules[0] != "memory" {
		t.Errorf("modules: got %v, want [memory]", modules)
	}
}

func TestToFilterProtos(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE disk.pct_used > 80 AND os = 'linux'`)
	filters := ToFilterProtos(q)
	if len(filters) != 2 {
		t.Fatalf("expected 2 filters, got %d", len(filters))
	}
	if filters[0].Field != "disk.pct_used" || filters[0].Operator != ">" || filters[0].Value != "80" {
		t.Errorf("filter[0]: %+v", filters[0])
	}
	if filters[1].Field != "os" || filters[1].Operator != "=" || filters[1].Value != "linux" {
		t.Errorf("filter[1]: %+v", filters[1])
	}
}

func TestToFilterProtosStripsTagConditions(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE tag.env = 'prod' AND disk.pct_used > 80`)
	filters := ToFilterProtos(q)
	if len(filters) != 1 {
		t.Fatalf("expected 1 filter (tag stripped), got %d", len(filters))
	}
	if filters[0].Field != "disk.pct_used" {
		t.Errorf("expected disk.pct_used filter, got %s", filters[0].Field)
	}
}

func TestToFilterProtosAllTags(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE tag.env = 'prod'`)
	filters := ToFilterProtos(q)
	if filters != nil {
		t.Errorf("expected nil filters for tag-only query, got %d", len(filters))
	}
}

func TestToFilterProtosComplexReturnsNil(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE os = 'linux' OR os = 'windows'`)
	filters := ToFilterProtos(q)
	if filters != nil {
		t.Errorf("expected nil for OR expression, got %d filters", len(filters))
	}
}

func TestMatchesAgentTags(t *testing.T) {
	tags := map[string]string{"env": "prod", "group": "webservers"}

	tests := []struct {
		query string
		want  bool
	}{
		{`SELECT * WHERE tag.env = 'prod'`, true},
		{`SELECT * WHERE tag.env = 'staging'`, false},
		{`SELECT * WHERE tag.env = 'prod' AND tag.group = 'webservers'`, true},
		{`SELECT * WHERE tag.env = 'prod' AND tag.group = 'dbservers'`, false},
		{`SELECT * WHERE tag.env = 'prod' OR tag.env = 'staging'`, true},
		{`SELECT * WHERE tag.env = 'staging' OR tag.env = 'dev'`, false},
		{`SELECT * WHERE tag.env IN ('prod', 'staging')`, true},
		{`SELECT * WHERE tag.env NOT IN ('staging', 'dev')`, true},
		{`SELECT * WHERE tag.env != 'staging'`, true},
		{`SELECT * WHERE tag.region IS NULL`, true},     // no "region" tag
		{`SELECT * WHERE tag.env IS NOT NULL`, true},     // "env" exists
		{`SELECT * WHERE tag.env LIKE 'pro%'`, true},
		{`SELECT * WHERE tag.env NOT LIKE 'stag%'`, true},
		// Data conditions are conservatively true.
		{`SELECT * WHERE disk.pct_used > 80`, true},
		{`SELECT * WHERE tag.env = 'prod' AND disk.pct_used > 80`, true},
		{`SELECT * WHERE tag.env = 'staging' AND disk.pct_used > 80`, false},
		{`SELECT * WHERE tag.env = 'staging' OR disk.pct_used > 80`, true},
	}

	for _, tt := range tests {
		q, err := Parse(tt.query)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.query, err)
		}
		got := MatchesAgentTags(q.Where, tags)
		if got != tt.want {
			t.Errorf("MatchesAgentTags(%q) = %v, want %v", tt.query, got, tt.want)
		}
	}
}

func TestStripTagFields(t *testing.T) {
	// Pure tag → nil.
	q, _ := Parse(`SELECT * WHERE tag.env = 'prod'`)
	if StripTagFields(q.Where) != nil {
		t.Error("pure tag condition should strip to nil")
	}

	// Tag AND data → data only.
	q, _ = Parse(`SELECT * WHERE tag.env = 'prod' AND disk.pct_used > 80`)
	stripped := StripTagFields(q.Where)
	cmp, ok := stripped.(*CompareExpr)
	if !ok || cmp.Field != "disk.pct_used" {
		t.Errorf("expected disk.pct_used, got %T %+v", stripped, stripped)
	}

	// Pure data → unchanged.
	q, _ = Parse(`SELECT * WHERE disk.pct_used > 80 AND os = 'linux'`)
	stripped = StripTagFields(q.Where)
	and, ok := stripped.(*BinaryExpr)
	if !ok || and.Op != "AND" {
		t.Error("pure data AND should be unchanged")
	}

	// OR with mixed tag/data → kept intact.
	q, _ = Parse(`SELECT * WHERE tag.env = 'prod' OR disk.pct_used > 80`)
	stripped = StripTagFields(q.Where)
	_, ok = stripped.(*BinaryExpr)
	if !ok {
		t.Error("OR with mixed should be kept intact")
	}
}

func TestMatchesWhere(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE disk.pct_used > 80 AND os = 'linux'`)

	row1 := Row{"disk.pct_used": 95.0, "os": "linux"}
	ok, err := MatchesWhere(q, row1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected row1 to match")
	}

	row2 := Row{"disk.pct_used": 50.0, "os": "linux"}
	ok, err = MatchesWhere(q, row2)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected row2 not to match")
	}

	row3 := Row{"disk.pct_used": 95.0, "os": "windows"}
	ok, err = MatchesWhere(q, row3)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("expected row3 not to match")
	}
}

func TestMatchesWhereOR(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE os = 'linux' OR os = 'freebsd'`)

	ok, _ := MatchesWhere(q, Row{"os": "linux"})
	if !ok {
		t.Error("linux should match")
	}
	ok, _ = MatchesWhere(q, Row{"os": "freebsd"})
	if !ok {
		t.Error("freebsd should match")
	}
	ok, _ = MatchesWhere(q, Row{"os": "windows"})
	if ok {
		t.Error("windows should not match")
	}
}

func TestMatchesWhereNOT(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE NOT os = 'windows'`)

	ok, _ := MatchesWhere(q, Row{"os": "linux"})
	if !ok {
		t.Error("linux should match NOT windows")
	}
	ok, _ = MatchesWhere(q, Row{"os": "windows"})
	if ok {
		t.Error("windows should not match NOT windows")
	}
}

func TestMatchesWhereISNULL(t *testing.T) {
	q, _ := Parse(`SELECT hostname WHERE cpu.model IS NULL`)

	ok, _ := MatchesWhere(q, Row{"hostname": "a"})
	if !ok {
		t.Error("missing field should match IS NULL")
	}
	ok, _ = MatchesWhere(q, Row{"cpu.model": "i7"})
	if ok {
		t.Error("present field should not match IS NULL")
	}
}

func TestAggregate(t *testing.T) {
	q, _ := Parse(`SELECT os, COUNT(hostname), AVG(memory.total_gb) GROUP BY os`)

	rows := []Row{
		{"os": "linux", "hostname": "web1", "memory.total_gb": 16.0},
		{"os": "linux", "hostname": "web2", "memory.total_gb": 32.0},
		{"os": "windows", "hostname": "win1", "memory.total_gb": 64.0},
	}

	result, err := Aggregate(q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(result))
	}

	if result[0].GroupKey["os"] != "linux" {
		t.Errorf("group[0] key: got %v, want linux", result[0].GroupKey["os"])
	}
	if result[0].Values["COUNT(hostname)"] != 2 {
		t.Errorf("COUNT(hostname): got %v, want 2", result[0].Values["COUNT(hostname)"])
	}
	avg := result[0].Values["AVG(memory.total_gb)"].(float64)
	if avg != 24.0 {
		t.Errorf("AVG(memory.total_gb): got %v, want 24.0", avg)
	}

	if result[1].GroupKey["os"] != "windows" {
		t.Errorf("group[1] key: got %v, want windows", result[1].GroupKey["os"])
	}
	if result[1].Values["COUNT(hostname)"] != 1 {
		t.Errorf("COUNT(hostname): got %v, want 1", result[1].Values["COUNT(hostname)"])
	}
}

func TestSortRows(t *testing.T) {
	q, _ := Parse(`SELECT hostname, disk.pct_used ORDER BY disk.pct_used DESC`)

	rows := []Row{
		{"hostname": "a", "disk.pct_used": 50.0},
		{"hostname": "b", "disk.pct_used": 90.0},
		{"hostname": "c", "disk.pct_used": 75.0},
	}

	SortRows(q, rows)

	if rows[0]["hostname"] != "b" || rows[1]["hostname"] != "c" || rows[2]["hostname"] != "a" {
		t.Errorf("sort order wrong: %v", rows)
	}
}

func TestSortRowsString(t *testing.T) {
	q, _ := Parse(`SELECT hostname ORDER BY hostname`)

	rows := []Row{
		{"hostname": "charlie"},
		{"hostname": "alpha"},
		{"hostname": "bravo"},
	}

	SortRows(q, rows)

	if rows[0]["hostname"] != "alpha" || rows[1]["hostname"] != "bravo" || rows[2]["hostname"] != "charlie" {
		t.Errorf("string sort wrong: %v", rows)
	}
}

func TestParseMultipleWhereConditions(t *testing.T) {
	input := `SELECT hostname WHERE disk.pct_used > 80 AND os = 'linux'`
	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	and, ok := q.Where.(*BinaryExpr)
	if !ok || and.Op != "AND" {
		t.Fatal("expected AND expression")
	}
	_ = and
}

func TestHasAggregates(t *testing.T) {
	// Query with aggregate function.
	q, err := Parse(`SELECT COUNT(hostname)`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !q.HasAggregates() {
		t.Error("expected HasAggregates() = true for COUNT(hostname)")
	}

	// Query without aggregate function.
	q, err = Parse(`SELECT hostname, cpu.cores`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if q.HasAggregates() {
		t.Error("expected HasAggregates() = false for bare fields")
	}
}

func TestBareAggregate_Count(t *testing.T) {
	q, err := Parse(`SELECT COUNT(hostname)`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	rows := []Row{
		{"hostname": "web1"},
		{"hostname": "web2"},
		{"hostname": "web3"},
	}

	result, err := Aggregate(q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(result))
	}
	if result[0].Values["COUNT(hostname)"] != 3 {
		t.Errorf("COUNT(hostname): got %v, want 3", result[0].Values["COUNT(hostname)"])
	}
}

func TestBareAggregate_Avg(t *testing.T) {
	q, err := Parse(`SELECT AVG(cpu.cores)`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	rows := []Row{
		{"cpu.cores": 4.0},
		{"cpu.cores": 8.0},
		{"cpu.cores": 12.0},
	}

	result, err := Aggregate(q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(result))
	}
	avg, ok := result[0].Values["AVG(cpu.cores)"].(float64)
	if !ok {
		t.Fatalf("AVG(cpu.cores): expected float64, got %T", result[0].Values["AVG(cpu.cores)"])
	}
	if avg != 8.0 {
		t.Errorf("AVG(cpu.cores): got %v, want 8.0", avg)
	}
}

func TestBareAggregate_Multiple(t *testing.T) {
	q, err := Parse(`SELECT COUNT(hostname), AVG(cpu.cores), MAX(cpu.cores)`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	rows := []Row{
		{"hostname": "web1", "cpu.cores": 4.0},
		{"hostname": "web2", "cpu.cores": 8.0},
		{"hostname": "web3", "cpu.cores": 16.0},
	}

	result, err := Aggregate(q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(result))
	}
	if result[0].Values["COUNT(hostname)"] != 3 {
		t.Errorf("COUNT(hostname): got %v, want 3", result[0].Values["COUNT(hostname)"])
	}
	avg, ok := result[0].Values["AVG(cpu.cores)"].(float64)
	if !ok {
		t.Fatalf("AVG(cpu.cores): expected float64, got %T", result[0].Values["AVG(cpu.cores)"])
	}
	want := (4.0 + 8.0 + 16.0) / 3.0
	if avg != want {
		t.Errorf("AVG(cpu.cores): got %v, want %v", avg, want)
	}
	max, ok := result[0].Values["MAX(cpu.cores)"].(float64)
	if !ok {
		t.Fatalf("MAX(cpu.cores): expected float64, got %T", result[0].Values["MAX(cpu.cores)"])
	}
	if max != 16.0 {
		t.Errorf("MAX(cpu.cores): got %v, want 16.0", max)
	}
}

func TestBareAggregate_EmptyRows(t *testing.T) {
	q, err := Parse(`SELECT COUNT(hostname), AVG(cpu.cores)`)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	var rows []Row

	result, err := Aggregate(q, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 result row, got %d", len(result))
	}
	if result[0].Values["COUNT(hostname)"] != 0 {
		t.Errorf("COUNT(hostname) over empty rows: got %v, want 0", result[0].Values["COUNT(hostname)"])
	}
	avg, ok := result[0].Values["AVG(cpu.cores)"].(float64)
	if !ok {
		t.Fatalf("AVG(cpu.cores): expected float64, got %T", result[0].Values["AVG(cpu.cores)"])
	}
	if avg != 0.0 {
		t.Errorf("AVG(cpu.cores) over empty rows: got %v, want 0.0", avg)
	}
}

func TestMatchLikeUnderscore(t *testing.T) {
	if !matchLike("web1", "web_") {
		t.Error("web_ should match web1")
	}
	if matchLike("web12", "web_") {
		t.Error("web_ should not match web12")
	}
}

func TestMatchLikeMultiplePercent(t *testing.T) {
	if !matchLike("us-east-1a", "%east%a") {
		t.Error("pattern should match us-east-1a")
	}
	if matchLike("us-east-1b", "%east%a") {
		t.Error("pattern should not match us-east-1b")
	}
}

func TestParseErrorMessage(t *testing.T) {
	_, err := Parse(`SELCT hostname`)
	if err == nil {
		t.Fatal("expected error for misspelled SELECT")
	}
	if err.Error() == "" {
		t.Error("error message is empty")
	}
}
