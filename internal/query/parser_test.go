package query

import (
	"testing"
)

func TestParseSelectFromWhereOrderBy(t *testing.T) {
	input := `SELECT hostname, disk.mount, disk.pct_used FROM tag:prod WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC`

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

	// FROM
	if q.From == nil {
		t.Fatal("expected FROM clause")
	}
	if q.From.All {
		t.Error("expected scoped FROM, got All")
	}
	if q.From.Scope.Kind != "tag" || q.From.Scope.Value != "prod" {
		t.Errorf("FROM: got %s:%s, want tag:prod", q.From.Scope.Kind, q.From.Scope.Value)
	}

	// WHERE
	if q.Where == nil {
		t.Fatal("expected WHERE clause")
	}
	if len(q.Where.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(q.Where.Conditions))
	}
	c := q.Where.Conditions[0]
	if c.Field != "disk.pct_used" || c.Operator != ">" || *c.Value.Number != 80 {
		t.Errorf("condition: got %s %s %v, want disk.pct_used > 80", c.Field, c.Operator, c.Value)
	}

	// ORDER BY
	if q.OrderBy == nil {
		t.Fatal("expected ORDER BY clause")
	}
	if q.OrderBy.Field != "disk.pct_used" || !q.OrderBy.Desc {
		t.Errorf("ORDER BY: got %s desc=%v, want disk.pct_used DESC", q.OrderBy.Field, q.OrderBy.Desc)
	}
}

func TestParseAggregation(t *testing.T) {
	input := `SELECT os, COUNT(hostname), AVG(memory.total_gb) FROM * GROUP BY os`

	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if len(q.Select) != 3 {
		t.Fatalf("expected 3 select exprs, got %d", len(q.Select))
	}

	// First select is a bare field.
	if q.Select[0].Field != "os" {
		t.Errorf("select[0]: got %q, want 'os'", q.Select[0].Field)
	}

	// Second select is COUNT(hostname).
	if q.Select[1].AggFunc == nil {
		t.Fatal("select[1]: expected AggFunc")
	}
	if q.Select[1].AggFunc.Name != "COUNT" || q.Select[1].AggFunc.Arg != "hostname" {
		t.Errorf("select[1]: got %s(%s), want COUNT(hostname)", q.Select[1].AggFunc.Name, q.Select[1].AggFunc.Arg)
	}

	// Third select is AVG(memory.total_gb).
	if q.Select[2].AggFunc == nil {
		t.Fatal("select[2]: expected AggFunc")
	}
	if q.Select[2].AggFunc.Name != "AVG" || q.Select[2].AggFunc.Arg != "memory.total_gb" {
		t.Errorf("select[2]: got %s(%s), want AVG(memory.total_gb)", q.Select[2].AggFunc.Name, q.Select[2].AggFunc.Arg)
	}

	// FROM *
	if q.From == nil || !q.From.All {
		t.Error("expected FROM *")
	}

	// GROUP BY
	if q.GroupBy == nil {
		t.Fatal("expected GROUP BY clause")
	}
	if len(q.GroupBy.Fields) != 1 || q.GroupBy.Fields[0] != "os" {
		t.Errorf("GROUP BY: got %v, want [os]", q.GroupBy.Fields)
	}
}

func TestParseGroupFromWhere(t *testing.T) {
	input := `SELECT hostname, cpu.cores FROM group:webservers WHERE cpu.cores >= 8`

	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// FROM group:webservers
	if q.From == nil || q.From.Scope == nil {
		t.Fatal("expected FROM group scope")
	}
	if q.From.Scope.Kind != "group" || q.From.Scope.Value != "webservers" {
		t.Errorf("FROM: got %s:%s, want group:webservers", q.From.Scope.Kind, q.From.Scope.Value)
	}

	// WHERE cpu.cores >= 8
	if q.Where == nil || len(q.Where.Conditions) != 1 {
		t.Fatal("expected 1 WHERE condition")
	}
	c := q.Where.Conditions[0]
	if c.Field != "cpu.cores" || c.Operator != ">=" || *c.Value.Number != 8 {
		t.Errorf("condition: got %s %s %v, want cpu.cores >= 8", c.Field, c.Operator, *c.Value.Number)
	}
}

func TestExtractModules(t *testing.T) {
	q, err := Parse(`SELECT hostname, disk.mount, disk.pct_used FROM tag:prod WHERE disk.pct_used > 80 ORDER BY disk.pct_used DESC`)
	if err != nil {
		t.Fatal(err)
	}
	modules := ExtractModules(q)
	if len(modules) != 1 || modules[0] != "disk" {
		t.Errorf("modules: got %v, want [disk]", modules)
	}
}

func TestExtractModulesMultiple(t *testing.T) {
	q, err := Parse(`SELECT os, COUNT(hostname), AVG(memory.total_gb) FROM * GROUP BY os`)
	if err != nil {
		t.Fatal(err)
	}
	modules := ExtractModules(q)
	if len(modules) != 1 || modules[0] != "memory" {
		t.Errorf("modules: got %v, want [memory]", modules)
	}
}

func TestExtractTarget(t *testing.T) {
	q, _ := Parse(`SELECT hostname FROM tag:prod`)
	target := ExtractTarget(q)
	if target.All || target.Kind != "tag" || target.Value != "prod" {
		t.Errorf("target: got %+v", target)
	}

	q2, _ := Parse(`SELECT hostname FROM *`)
	target2 := ExtractTarget(q2)
	if !target2.All {
		t.Error("expected All target")
	}
}

func TestToFilterProtos(t *testing.T) {
	q, _ := Parse(`SELECT hostname FROM * WHERE disk.pct_used > 80 AND os = 'linux'`)
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

func TestMatchesWhere(t *testing.T) {
	q, _ := Parse(`SELECT hostname FROM * WHERE disk.pct_used > 80 AND os = 'linux'`)

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

func TestAggregate(t *testing.T) {
	q, _ := Parse(`SELECT os, COUNT(hostname), AVG(memory.total_gb) FROM * GROUP BY os`)

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

	// First group: linux
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

	// Second group: windows
	if result[1].GroupKey["os"] != "windows" {
		t.Errorf("group[1] key: got %v, want windows", result[1].GroupKey["os"])
	}
	if result[1].Values["COUNT(hostname)"] != 1 {
		t.Errorf("COUNT(hostname): got %v, want 1", result[1].Values["COUNT(hostname)"])
	}
}

func TestSortRows(t *testing.T) {
	q, _ := Parse(`SELECT hostname, disk.pct_used FROM * ORDER BY disk.pct_used DESC`)

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

func TestParseMultipleWhereConditions(t *testing.T) {
	input := `SELECT hostname FROM * WHERE disk.pct_used > 80 AND os = 'linux'`
	q, err := Parse(input)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(q.Where.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(q.Where.Conditions))
	}
}
