package db

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

// reserved query parameters that are never treated as field filters.
var reserved = map[string]bool{
	"q": true, "_sort": true, "_order": true, "_page": true, "_limit": true,
	"_embed": true, "_expand": true,
}

// ListResult is the outcome of applying query parameters to a collection.
type ListResult struct {
	Items     []any
	Total     int  // items matching filters, before pagination
	Paginated bool // _page was given
	Page      int
	Limit     int
	LastPage  int
}

// QueryError is a client error in the query string.
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return e.Msg }

func qerr(format string, args ...any) error { return &QueryError{Msg: fmt.Sprintf(format, args...)} }

type filter struct {
	field string
	op    string // "", "gte", "lte", "ne", "like"
	vals  []string
	res   []*regexp.Regexp
}

// ParseFilters extracts field filters from the query string.
func ParseFilters(q url.Values) ([]filter, error) {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []filter
	for _, k := range keys {
		if reserved[k] {
			continue
		}
		f := filter{field: k, vals: q[k]}
		for _, op := range []string{"gte", "lte", "ne", "like"} {
			suffix := "_" + op
			if len(k) > len(suffix) && strings.HasSuffix(k, suffix) {
				f.field, f.op = strings.TrimSuffix(k, suffix), op
				break
			}
		}
		if f.op == "like" {
			for _, v := range f.vals {
				re, err := regexp.Compile("(?i)" + v)
				if err != nil {
					return nil, qerr("invalid regular expression in %s=%s: %v", k, v, err)
				}
				f.res = append(f.res, re)
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// Lookup follows a dotted path ("address.city", "tags.0") into v.
func Lookup(v any, path string) (any, bool) {
	cur := v
	for _, part := range strings.Split(path, ".") {
		switch t := cur.(type) {
		case *jsonx.Object:
			next, ok := t.Get(part)
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(t) {
				return nil, false
			}
			cur = t[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// ScalarString renders a scalar JSON value the way it would appear in a URL.
func ScalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	case nil:
		return "null", true
	case int:
		return strconv.Itoa(t), true
	}
	return "", false
}

func number(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case int:
		return float64(t), true
	}
	return 0, false
}

// scalarEquals compares a stored scalar with a query string value. Numbers
// compare numerically, so ?price=10 matches 10.0.
func scalarEquals(v any, q string) bool {
	if f, ok := number(v); ok {
		if qf, err := strconv.ParseFloat(q, 64); err == nil {
			return f == qf
		}
	}
	s, ok := ScalarString(v)
	return ok && s == q
}

// valueEquals is scalarEquals, but an array matches if any element matches.
func valueEquals(v any, q string) bool {
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if scalarEquals(e, q) {
				return true
			}
		}
		return false
	}
	return scalarEquals(v, q)
}

// compareToQuery orders a stored value against a query value: numerically when
// both are numbers, otherwise as strings (which suits ISO dates).
func compareToQuery(v any, q string) (int, bool) {
	if f, ok := number(v); ok {
		if qf, err := strconv.ParseFloat(q, 64); err == nil {
			switch {
			case f < qf:
				return -1, true
			case f > qf:
				return 1, true
			}
			return 0, true
		}
	}
	if s, ok := v.(string); ok {
		return strings.Compare(s, q), true
	}
	return 0, false
}

func (f filter) match(item any) bool {
	v, found := Lookup(item, f.field)
	switch f.op {
	case "":
		if !found {
			return false
		}
		for _, q := range f.vals {
			if valueEquals(v, q) {
				return true
			}
		}
		return false
	case "ne":
		if !found {
			return true
		}
		for _, q := range f.vals {
			if valueEquals(v, q) {
				return false
			}
		}
		return true
	case "gte", "lte":
		if !found {
			return false
		}
		for _, q := range f.vals {
			c, ok := compareToQuery(v, q)
			if !ok || (f.op == "gte" && c < 0) || (f.op == "lte" && c > 0) {
				return false
			}
		}
		return true
	case "like":
		if !found {
			return false
		}
		vals := []any{v}
		if arr, ok := v.([]any); ok {
			vals = arr
		}
		for _, re := range f.res {
			for _, e := range vals {
				if s, ok := ScalarString(e); ok && e != nil && re.MatchString(s) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// fullText reports whether any string or number inside v contains needle
// (needle must already be lower case).
func fullText(v any, needle string) bool {
	switch t := v.(type) {
	case *jsonx.Object:
		for _, k := range t.Keys() {
			val, _ := t.Get(k)
			if fullText(val, needle) {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if fullText(e, needle) {
				return true
			}
		}
	case string:
		return strings.Contains(strings.ToLower(t), needle)
	case json.Number:
		return strings.Contains(t.String(), needle)
	}
	return false
}

type sortKey struct {
	field string
	desc  bool
}

func parseSort(q url.Values) []sortKey {
	var fields []string
	for _, v := range q["_sort"] {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				fields = append(fields, f)
			}
		}
	}
	var orders []string
	for _, v := range q["_order"] {
		orders = append(orders, strings.Split(v, ",")...)
	}
	keys := make([]sortKey, 0, len(fields))
	for i, f := range fields {
		k := sortKey{field: f}
		if strings.HasPrefix(f, "-") {
			k.field, k.desc = f[1:], true
		}
		if i < len(orders) && strings.EqualFold(strings.TrimSpace(orders[i]), "desc") {
			k.desc = !k.desc
		}
		keys = append(keys, k)
	}
	return keys
}

func typeRank(v any) int {
	switch v.(type) {
	case json.Number, int:
		return 0
	case string:
		return 1
	case bool:
		return 2
	case nil:
		return 4
	default:
		return 3
	}
}

// CompareValues orders two JSON values: numbers, then strings, then booleans,
// then objects/arrays, then null.
func CompareValues(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return ra - rb
	}
	switch ra {
	case 0:
		fa, _ := number(a)
		fb, _ := number(b)
		switch {
		case fa < fb:
			return -1
		case fa > fb:
			return 1
		}
		return 0
	case 1:
		return strings.Compare(a.(string), b.(string))
	case 2:
		ba, bb := a.(bool), b.(bool)
		switch {
		case ba == bb:
			return 0
		case !ba:
			return -1
		}
		return 1
	}
	return 0
}

func positiveInt(q url.Values, name string) (int, bool, error) {
	s := q.Get(name)
	if s == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, false, qerr("%s must be a positive integer, got %q", name, s)
	}
	return n, true, nil
}

// ApplyList filters, searches, sorts and paginates items according to q.
// items is not modified.
func ApplyList(items []any, q url.Values) (ListResult, error) {
	filters, err := ParseFilters(q)
	if err != nil {
		return ListResult{}, err
	}
	page, hasPage, err := positiveInt(q, "_page")
	if err != nil {
		return ListResult{}, err
	}
	limit, hasLimit, err := positiveInt(q, "_limit")
	if err != nil {
		return ListResult{}, err
	}
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))

	out := make([]any, 0, len(items))
	for _, it := range items {
		ok := true
		for _, f := range filters {
			if !f.match(it) {
				ok = false
				break
			}
		}
		if ok && needle != "" && !fullText(it, needle) {
			ok = false
		}
		if ok {
			out = append(out, it)
		}
	}

	if keys := parseSort(q); len(keys) > 0 {
		sort.SliceStable(out, func(i, j int) bool {
			for _, k := range keys {
				a, aok := Lookup(out[i], k.field)
				b, bok := Lookup(out[j], k.field)
				// Missing fields always sort last, whatever the direction.
				if !aok || !bok {
					if aok != bok {
						return aok
					}
					continue
				}
				c := CompareValues(a, b)
				if c == 0 {
					continue
				}
				if k.desc {
					return c > 0
				}
				return c < 0
			}
			return false
		})
	}

	res := ListResult{Total: len(out)}
	if hasPage || hasLimit {
		if !hasPage {
			page = 1
		}
		if !hasLimit {
			limit = 10
		}
		res.Paginated = hasPage
		res.Page, res.Limit = page, limit
		res.LastPage = (len(out) + limit - 1) / limit
		if res.LastPage == 0 {
			res.LastPage = 1
		}
		start := (page - 1) * limit
		if start > len(out) {
			start = len(out)
		}
		end := start + limit
		if end > len(out) {
			end = len(out)
		}
		out = out[start:end]
	}
	res.Items = out
	return res, nil
}

// LinkHeader builds an RFC 8288 Link header for a paginated result.
func LinkHeader(base *url.URL, host string, res ListResult) string {
	if !res.Paginated {
		return ""
	}
	mk := func(page int, rel string) string {
		u := *base
		u.Scheme, u.Host = "http", host
		q := u.Query()
		q.Set("_page", strconv.Itoa(page))
		q.Set("_limit", strconv.Itoa(res.Limit))
		u.RawQuery = q.Encode()
		return fmt.Sprintf("<%s>; rel=%q", u.String(), rel)
	}
	links := []string{mk(1, "first")}
	if res.Page > 1 {
		prev := res.Page - 1
		if prev > res.LastPage {
			prev = res.LastPage
		}
		links = append(links, mk(prev, "prev"))
	}
	if res.Page < res.LastPage {
		links = append(links, mk(res.Page+1, "next"))
	}
	links = append(links, mk(res.LastPage, "last"))
	return strings.Join(links, ", ")
}

// Singular turns a collection name into the prefix of its foreign key:
// users -> user, categories -> category, addresses -> address.
func Singular(name string) string {
	switch {
	case strings.HasSuffix(name, "ies") && len(name) > 3:
		return name[:len(name)-3] + "y"
	case strings.HasSuffix(name, "sses"), strings.HasSuffix(name, "shes"),
		strings.HasSuffix(name, "ches"), strings.HasSuffix(name, "xes"):
		return name[:len(name)-2]
	case strings.HasSuffix(name, "s") && !strings.HasSuffix(name, "ss") && len(name) > 1:
		return name[:len(name)-1]
	}
	return name
}

// ForeignKeys lists the field names, most likely first, that may point at an
// item of the named collection: "userId" for users, and both "branchId" and
// "brancheId" style guesses for irregular plurals.
func ForeignKeys(collection string) []string {
	keys := []string{Singular(collection) + "Id"}
	if strings.HasSuffix(collection, "s") && len(collection) > 1 {
		if k := collection[:len(collection)-1] + "Id"; k != keys[0] {
			keys = append(keys, k)
		}
	}
	return keys
}

// pluralCandidates lists collection names that a singular _expand name may refer to.
func pluralCandidates(name string) []string {
	c := []string{name + "s", name + "es"}
	if strings.HasSuffix(name, "y") && len(name) > 1 {
		c = append(c, name[:len(name)-1]+"ies")
	}
	return append(c, name)
}
