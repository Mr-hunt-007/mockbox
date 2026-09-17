// Package openapi serves mock responses for an OpenAPI 3 JSON document.
package openapi

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Mr-hunt-007/mockbox/internal/httpx"
	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// Operation is one path + method from the spec.
type Operation struct {
	Method   string // upper case
	Path     string // as written in the spec, e.g. /users/{id}
	op       *jsonx.Object
	segments []segment
	literals int
}

type segment struct {
	literal string
	re      *regexp.Regexp // nil for literal segments
	names   []string
}

// Spec is a loaded OpenAPI document.
type Spec struct {
	root     *jsonx.Object
	ops      []*Operation
	BasePath string // path of servers[0].url, e.g. /v1, or ""
}

// Route is one entry of the route table.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// IsSpec reports whether a decoded document looks like an OpenAPI or Swagger document.
func IsSpec(doc any) bool {
	o, ok := doc.(*jsonx.Object)
	if !ok {
		return false
	}
	_, a := o.Get("openapi")
	_, b := o.Get("swagger")
	return a || b
}

// Load validates the document and compiles its paths.
func Load(doc any) (*Spec, error) {
	root, ok := doc.(*jsonx.Object)
	if !ok {
		return nil, errors.New("OpenAPI document must be a JSON object")
	}
	if v, ok := root.Get("swagger"); ok {
		return nil, fmt.Errorf("Swagger %v documents are not supported; convert to OpenAPI 3 first", v)
	}
	ver, _ := root.Get("openapi")
	vs, _ := ver.(string)
	if !strings.HasPrefix(vs, "3.") {
		return nil, fmt.Errorf("unsupported openapi version %v (mockbox supports 3.x)", ver)
	}
	s := &Spec{root: root}
	if servers, ok := mustGet(root, "servers").([]any); ok && len(servers) > 0 {
		if so, ok := servers[0].(*jsonx.Object); ok {
			if u, err := url.Parse(getString(so, "url")); err == nil && !strings.Contains(u.Path, "{") {
				s.BasePath = strings.TrimRight(u.Path, "/")
			}
		}
	}
	paths, _ := mustGet(root, "paths").(*jsonx.Object)
	if paths == nil {
		return s, nil
	}
	for _, p := range paths.Keys() {
		item, _ := paths.Get(p)
		itemObj, ok := item.(*jsonx.Object)
		if !ok {
			continue
		}
		if ref := getString(itemObj, "$ref"); ref != "" {
			target, err := Resolve(root, ref)
			if err != nil {
				return nil, fmt.Errorf("path %s: %v", p, err)
			}
			if t, ok := target.(*jsonx.Object); ok {
				itemObj = t
			}
		}
		segs, lits, err := compilePath(p)
		if err != nil {
			return nil, fmt.Errorf("path %s: %v", p, err)
		}
		for _, m := range methods {
			op, ok := mustGet(itemObj, m).(*jsonx.Object)
			if !ok {
				continue
			}
			s.ops = append(s.ops, &Operation{Method: strings.ToUpper(m), Path: p, op: op, segments: segs, literals: lits})
		}
	}
	return s, nil
}

var paramRe = regexp.MustCompile(`\{([^{}/]+)\}`)

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func compilePath(p string) ([]segment, int, error) {
	var segs []segment
	lits := 0
	for _, part := range splitPath(p) {
		if !strings.Contains(part, "{") {
			segs = append(segs, segment{literal: part})
			lits++
			continue
		}
		var b strings.Builder
		b.WriteString("^")
		var names []string
		last := 0
		for _, m := range paramRe.FindAllStringSubmatchIndex(part, -1) {
			b.WriteString(regexp.QuoteMeta(part[last:m[0]]))
			b.WriteString("([^/]+?)")
			names = append(names, part[m[2]:m[3]])
			last = m[1]
		}
		b.WriteString(regexp.QuoteMeta(part[last:]))
		b.WriteString("$")
		re, err := regexp.Compile(b.String())
		if err != nil {
			return nil, 0, err
		}
		segs = append(segs, segment{re: re, names: names})
	}
	return segs, lits, nil
}

func (op *Operation) match(parts []string) bool {
	if len(parts) != len(op.segments) {
		return false
	}
	for i, seg := range op.segments {
		if seg.re == nil {
			if seg.literal != parts[i] {
				return false
			}
		} else if !seg.re.MatchString(parts[i]) {
			return false
		}
	}
	return true
}

// Routes lists every operation in spec order.
func (s *Spec) Routes() []Route {
	out := make([]Route, 0, len(s.ops))
	for _, op := range s.ops {
		out = append(out, Route{op.Method, op.Path})
	}
	return out
}

// HasRoot reports whether the spec defines GET /.
func (s *Spec) HasRoot() bool {
	for _, op := range s.ops {
		if len(op.segments) == 0 && op.Method == http.MethodGet {
			return true
		}
	}
	return false
}

// Find returns the operation for method and path. When the path exists but
// the method does not, it returns nil and the allowed methods.
func (s *Spec) Find(method, path string) (*Operation, []string) {
	parts := splitPath(path)
	var candidates []*Operation
	for _, op := range s.ops {
		if op.match(parts) {
			candidates = append(candidates, op)
		}
	}
	if len(candidates) == 0 && s.BasePath != "" && (path == s.BasePath || strings.HasPrefix(path, s.BasePath+"/")) {
		return s.Find(method, strings.TrimPrefix(path, s.BasePath))
	}
	// Most literal segments wins: /users/me beats /users/{id}.
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].literals > candidates[j].literals })
	var allow []string
	seen := map[string]bool{}
	for _, want := range []string{method, http.MethodGet} {
		for _, op := range candidates {
			if op.Method == want && (want == method || method == http.MethodHead) {
				return op, nil
			}
		}
	}
	for _, op := range candidates {
		if !seen[op.Method] {
			seen[op.Method] = true
			allow = append(allow, op.Method)
		}
	}
	return nil, allow
}

// ServeHTTP answers with the operation's example or a synthesized value.
func (s *Spec) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	op, allow := s.Find(r.Method, r.URL.Path)
	if op == nil {
		if len(allow) > 0 {
			httpx.MethodNotAllowed(w, r, allow)
			return
		}
		httpx.NotFound(w, r)
		return
	}
	// Mock responses do not depend on the body, but a malformed JSON body is
	// still reported, as a real API would.
	if r.ContentLength != 0 && strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") {
		if herr := httpx.CheckJSON(w, r); herr != nil {
			httpx.WriteErr(w, herr)
			return
		}
	}
	status, ctype, body, err := s.Respond(op, r.Header.Get("Prefer"))
	if err != nil {
		httpx.WriteErr(w, err)
		return
	}
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	if body != nil && status != http.StatusNoContent && status != http.StatusNotModified {
		_, _ = w.Write(body)
	}
}

// parsePrefer reads `Prefer: code=404, example=notFound`.
func parsePrefer(h string) (code, example string) {
	for _, part := range strings.FieldsFunc(h, func(r rune) bool { return r == ',' || r == ';' }) {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "code":
			code = v
		case "example":
			example = v
		}
	}
	return code, example
}

func (s *Spec) deref(v any) (*jsonx.Object, error) {
	for i := 0; i < 10; i++ {
		o, ok := v.(*jsonx.Object)
		if !ok {
			return nil, nil
		}
		ref := getString(o, "$ref")
		if ref == "" {
			return o, nil
		}
		t, err := Resolve(s.root, ref)
		if err != nil {
			return nil, err
		}
		v = t
	}
	return nil, errors.New("$ref chain too long")
}

// Respond picks a response for op and renders its body.
func (s *Spec) Respond(op *Operation, prefer string) (int, string, []byte, error) {
	responses, _ := mustGet(op.op, "responses").(*jsonx.Object)
	if responses == nil || responses.Len() == 0 {
		return http.StatusNoContent, "", nil, nil
	}
	wantCode, wantExample := parsePrefer(prefer)
	key := ""
	if wantCode != "" {
		if _, ok := responses.Get(wantCode); ok {
			key = wantCode
		} else {
			return 0, "", nil, httpx.Errorf(http.StatusBadRequest, "Prefer: code=%s, but %s %s only defines responses %s", wantCode, op.Method, op.Path, strings.Join(responses.Keys(), ", "))
		}
	}
	if key == "" {
		key = pickResponse(responses.Keys())
	}
	status := statusFor(key)
	resp, err := s.deref(mustGet(responses, key))
	if err != nil {
		return 0, "", nil, err
	}
	if resp == nil {
		return status, "", nil, nil
	}
	content, _ := mustGet(resp, "content").(*jsonx.Object)
	if content == nil || content.Len() == 0 {
		return status, "", nil, nil
	}
	ctype := pickMediaType(content.Keys())
	media, _ := mustGet(content, ctype).(*jsonx.Object)
	outType := ctype
	if strings.Contains(outType, "*") {
		outType = "application/json"
	}
	isJSON := strings.Contains(outType, "json")
	if media == nil {
		return status, outType, nil, nil
	}

	value, found, err := s.exampleFor(media, wantExample)
	if err != nil {
		return 0, "", nil, err
	}
	if !found {
		schema, hasSchema := media.Get("schema")
		if !hasSchema {
			return status, outType, nil, nil
		}
		value, err = NewSynthesizer(s.root).Value(schema)
		if err != nil {
			return 0, "", nil, httpx.Errorf(http.StatusInternalServerError, "%s %s: %v", op.Method, op.Path, err)
		}
	}
	if str, ok := value.(string); ok && !isJSON {
		return status, outType, []byte(str), nil
	}
	if isJSON && !strings.Contains(outType, "charset") {
		outType += "; charset=utf-8"
	}
	return status, outType, append(jsonx.Marshal(value, "  "), '\n'), nil
}

func (s *Spec) exampleFor(media *jsonx.Object, name string) (any, bool, error) {
	if exs, ok := mustGet(media, "examples").(*jsonx.Object); ok && exs.Len() > 0 {
		keys := exs.Keys()
		if name != "" {
			if _, ok := exs.Get(name); !ok {
				return nil, false, httpx.Errorf(http.StatusBadRequest, "Prefer: example=%s, but the available examples are %s", name, strings.Join(keys, ", "))
			}
			keys = []string{name}
		}
		for _, k := range keys {
			ex, err := s.deref(mustGet(exs, k))
			if err != nil {
				return nil, false, err
			}
			if ex == nil {
				continue
			}
			if v, ok := ex.Get("value"); ok {
				return v, true, nil
			}
		}
	}
	if v, ok := media.Get("example"); ok {
		return v, true, nil
	}
	return nil, false, nil
}

// pickResponse prefers the lowest 2xx code, then 2XX, then default, then the first key.
func pickResponse(keys []string) string {
	best := ""
	for _, k := range keys {
		if n, err := strconv.Atoi(k); err == nil && n >= 200 && n < 300 {
			if best == "" || k < best {
				best = k
			}
		}
	}
	if best != "" {
		return best
	}
	for _, want := range []string{"2XX", "2xx", "default"} {
		for _, k := range keys {
			if k == want {
				return k
			}
		}
	}
	return keys[0]
}

func statusFor(key string) int {
	if n, err := strconv.Atoi(key); err == nil && n >= 100 && n < 600 {
		return n
	}
	if len(key) == 3 && (key[1] == 'X' || key[1] == 'x') && key[0] >= '1' && key[0] <= '5' {
		return int(key[0]-'0') * 100
	}
	return http.StatusOK
}

func pickMediaType(keys []string) string {
	for _, k := range keys {
		if strings.HasPrefix(strings.ToLower(k), "application/json") {
			return k
		}
	}
	for _, k := range keys {
		if strings.Contains(strings.ToLower(k), "json") {
			return k
		}
	}
	return keys[0]
}
