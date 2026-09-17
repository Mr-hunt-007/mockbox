package db

import (
	"net/url"
	"strings"
	"testing"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

const fixture = `[
  {"id": 1, "name": "Ada", "role": "admin", "age": 36, "joined": "2020-01-05", "tags": ["math", "poetry"], "address": {"city": "London"}},
  {"id": 2, "name": "alan", "role": "user", "age": 41, "joined": "2021-06-01", "tags": ["crypto"], "address": {"city": "Wilmslow"}},
  {"id": 3, "name": "Grace", "role": "admin", "age": 85, "joined": "2019-03-10", "tags": [], "address": {"city": "Arlington"}},
  {"id": 4, "name": "Linus", "role": "user", "joined": "2022-11-30", "active": false}
]`

func items(t *testing.T) []any {
	t.Helper()
	v, err := jsonx.Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	return v.([]any)
}

func ids(list []any) string {
	var out []string
	for _, it := range list {
		v, _ := it.(*jsonx.Object).Get("id")
		s, _ := ScalarString(v)
		out = append(out, s)
	}
	return strings.Join(out, ",")
}

func TestApplyListFilters(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"", "1,2,3,4"},
		{"role=admin", "1,3"},
		{"role=admin&role=user", "1,2,3,4"},
		{"address.city=London", "1"},
		{"age=36", "1"},
		{"age=36.0", "1"},
		{"age_gte=41", "2,3"},
		{"age_lte=41", "1,2"},
		{"age_gte=37&age_lte=85", "2,3"},
		{"joined_gte=2021-01-01", "2,4"},
		{"role_ne=admin", "2,4"},
		{"age_ne=36", "2,3,4"}, // missing field counts as not equal
		{"role_ne=admin&role_ne=user", ""},
		{"name_like=^a", "1,2"}, // case-insensitive
		{"name_like=^gr&name_like=nus$", "3,4"},
		{"tags=crypto", "2"},
		{"tags_like=poe", "1"},
		{"active=false", "4"},
		{"q=ARLING", "3"},
		{"q=85", "3"},
		{"q=nomatch", ""},
		{"role=admin&q=grace", "3"},
		{"missing=1", ""},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			res, err := ApplyList(items(t), q)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(res.Items); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
			if res.Total != len(res.Items) {
				t.Errorf("total %d, items %d", res.Total, len(res.Items))
			}
		})
	}
}

func TestApplyListSort(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"_sort=age", "1,2,3,4"},  // missing age sorts last
		{"_sort=-age", "3,2,1,4"}, // still last when descending
		{"_sort=age&_order=desc", "3,2,1,4"},
		{"_sort=role,-age", "3,1,2,4"},
		{"_sort=name", "1,3,4,2"}, // byte order: upper case first
		{"_sort=address.city", "3,1,2,4"},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			res, err := ApplyList(items(t), q)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(res.Items); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestApplyListPagination(t *testing.T) {
	tests := []struct {
		query     string
		want      string
		total     int
		paginated bool
		last      int
	}{
		{"_page=1&_limit=3", "1,2,3", 4, true, 2},
		{"_page=2&_limit=3", "4", 4, true, 2},
		{"_page=9&_limit=3", "", 4, true, 2},
		{"_limit=2", "1,2", 4, false, 2},
		{"_page=1", "1,2,3,4", 4, true, 1}, // default limit 10
		{"role=admin&_page=2&_limit=1", "3", 2, true, 2},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			q, _ := url.ParseQuery(tt.query)
			res, err := ApplyList(items(t), q)
			if err != nil {
				t.Fatal(err)
			}
			if got := ids(res.Items); got != tt.want {
				t.Errorf("items %q want %q", got, tt.want)
			}
			if res.Total != tt.total || res.Paginated != tt.paginated || res.LastPage != tt.last {
				t.Errorf("total=%d paginated=%v last=%d", res.Total, res.Paginated, res.LastPage)
			}
		})
	}
}

func TestApplyListErrors(t *testing.T) {
	for _, query := range []string{"_page=0", "_page=abc", "_limit=-1", "name_like=("} {
		q, _ := url.ParseQuery(query)
		if _, err := ApplyList(items(t), q); err == nil {
			t.Errorf("%s: expected error", query)
		} else if _, ok := err.(*QueryError); !ok {
			t.Errorf("%s: want *QueryError, got %T", query, err)
		}
	}
}

func TestLinkHeader(t *testing.T) {
	base, _ := url.Parse("/users?role=admin&_page=2&_limit=1")
	link := LinkHeader(base, "localhost:3000", ListResult{Paginated: true, Page: 2, Limit: 1, LastPage: 3})
	want := []string{
		`<http://localhost:3000/users?_limit=1&_page=1&role=admin>; rel="first"`,
		`<http://localhost:3000/users?_limit=1&_page=1&role=admin>; rel="prev"`,
		`<http://localhost:3000/users?_limit=1&_page=3&role=admin>; rel="next"`,
		`<http://localhost:3000/users?_limit=1&_page=3&role=admin>; rel="last"`,
	}
	if link != strings.Join(want, ", ") {
		t.Fatalf("link:\n%s", link)
	}
	if LinkHeader(base, "h", ListResult{Paginated: false}) != "" {
		t.Fatal("unpaginated result must not get a Link header")
	}
	first := LinkHeader(base, "h", ListResult{Paginated: true, Page: 1, Limit: 1, LastPage: 1})
	if strings.Contains(first, "prev") || strings.Contains(first, "next") {
		t.Fatalf("single page should have only first/last: %s", first)
	}
}

func TestSingularAndForeignKeys(t *testing.T) {
	tests := map[string]string{
		"users": "user", "categories": "category", "addresses": "address",
		"boxes": "box", "branches": "branch", "class": "class", "data": "data",
	}
	for in, want := range tests {
		if got := Singular(in); got != want {
			t.Errorf("Singular(%q) = %q want %q", in, got, want)
		}
	}
	if got := strings.Join(ForeignKeys("caches"), ","); got != "cachId,cacheId" {
		t.Errorf("ForeignKeys(caches) = %s", got)
	}
	if got := strings.Join(ForeignKeys("users"), ","); got != "userId" {
		t.Errorf("ForeignKeys(users) = %s", got)
	}
}
