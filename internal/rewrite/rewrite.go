// Package rewrite implements --routes URL rewrites such as {"/api/*": "/$1"}.
package rewrite

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

// Rule rewrites paths matching From into To. In From, `*` matches anything
// (including slashes) and `:name` matches one path segment. In To, `$1`, `$2`
// refer to captures in order and `:name` to named captures.
type Rule struct {
	From, To string
	re       *regexp.Regexp
	names    []string // capture index -> name ("" for *)
}

var tokenRe = regexp.MustCompile(`\*|:[A-Za-z_][A-Za-z0-9_]*`)

// Compile builds a rule.
func Compile(from, to string) (Rule, error) {
	if !strings.HasPrefix(from, "/") {
		return Rule{}, fmt.Errorf("route %q must start with /", from)
	}
	if !strings.HasPrefix(to, "/") {
		return Rule{}, fmt.Errorf("target %q for route %q must start with /", to, from)
	}
	var b strings.Builder
	b.WriteString("^")
	var names []string
	last := 0
	for _, m := range tokenRe.FindAllStringIndex(from, -1) {
		b.WriteString(regexp.QuoteMeta(from[last:m[0]]))
		tok := from[m[0]:m[1]]
		if tok == "*" {
			b.WriteString("(.*)")
			names = append(names, "")
		} else {
			b.WriteString("([^/]+)")
			names = append(names, tok[1:])
		}
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(from[last:]))
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return Rule{}, err
	}
	return Rule{From: from, To: to, re: re, names: names}, nil
}

var dollarRe = regexp.MustCompile(`\$(\d+)`)

// Apply rewrites path if it matches.
func (r Rule) Apply(path string) (string, bool) {
	m := r.re.FindStringSubmatch(path)
	if m == nil {
		return "", false
	}
	out := dollarRe.ReplaceAllStringFunc(r.To, func(s string) string {
		i, _ := strconv.Atoi(s[1:])
		if i >= 1 && i < len(m) {
			return m[i]
		}
		return ""
	})
	out = tokenRe.ReplaceAllStringFunc(out, func(s string) string {
		if s == "*" {
			return s
		}
		for i, n := range r.names {
			if n == s[1:] {
				return m[i+1]
			}
		}
		return s
	})
	return out, true
}

// Parse reads a routes file: a JSON object mapping patterns to targets,
// applied in file order.
func Parse(data []byte) ([]Rule, error) {
	doc, err := jsonx.Parse(data)
	if err != nil {
		return nil, err
	}
	obj, ok := doc.(*jsonx.Object)
	if !ok {
		return nil, fmt.Errorf(`routes file must be a JSON object like {"/api/*": "/$1"}`)
	}
	var rules []Rule
	for _, k := range obj.Keys() {
		v, _ := obj.Get(k)
		to, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("route %q: target must be a string", k)
		}
		rule, err := Compile(k, to)
		if err != nil {
			return nil, err
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// Rewrite applies the first matching rule. Any query string in the target is
// merged with the request's own query (request values are kept).
func Rewrite(rules []Rule, path, rawQuery string) (string, string, bool) {
	for _, r := range rules {
		out, ok := r.Apply(path)
		if !ok {
			continue
		}
		newPath, targetQuery, _ := strings.Cut(out, "?")
		if newPath == "" {
			newPath = "/"
		}
		if targetQuery == "" {
			return newPath, rawQuery, true
		}
		tq, err := url.ParseQuery(targetQuery)
		if err != nil {
			return newPath, rawQuery, true
		}
		oq, _ := url.ParseQuery(rawQuery)
		for k, vs := range oq {
			tq[k] = append(tq[k], vs...)
		}
		return newPath, tq.Encode(), true
	}
	return path, rawQuery, false
}
