package openapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

// maxDepth bounds synthesis of deeply nested (non-cyclic) schemas.
const maxDepth = 12

// cycle marks a value that could not be built because of a $ref cycle or the
// depth limit. Objects omit such properties and arrays become empty.
type cycle struct{}

// Synthesizer builds example values from JSON Schema fragments of a spec.
type Synthesizer struct {
	root  *jsonx.Object
	stack []string
}

// NewSynthesizer returns a synthesizer resolving $refs against root.
func NewSynthesizer(root *jsonx.Object) *Synthesizer {
	return &Synthesizer{root: root}
}

// Value synthesizes a value for schema. Errors are unresolvable or external $refs.
func (s *Synthesizer) Value(schema any) (any, error) {
	s.stack = s.stack[:0]
	v, err := s.value(schema, 0)
	if _, isCycle := v.(cycle); isCycle {
		v = nil
	}
	return v, err
}

// Resolve follows a local JSON pointer ("#/components/schemas/User").
func Resolve(root *jsonx.Object, ref string) (any, error) {
	if !strings.HasPrefix(ref, "#") {
		return nil, fmt.Errorf("external $ref %q is not supported (only refs inside the same file, starting with #/)", ref)
	}
	ptr := strings.TrimPrefix(ref, "#")
	var cur any = root
	if ptr == "" {
		return cur, nil
	}
	for _, raw := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
		switch t := cur.(type) {
		case *jsonx.Object:
			next, ok := t.Get(tok)
			if !ok {
				return nil, fmt.Errorf("cannot resolve $ref %q: %q not found", ref, tok)
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(t) {
				return nil, fmt.Errorf("cannot resolve $ref %q: bad index %q", ref, tok)
			}
			cur = t[i]
		default:
			return nil, fmt.Errorf("cannot resolve $ref %q", ref)
		}
	}
	return cur, nil
}

func getString(o *jsonx.Object, k string) string {
	v, _ := o.Get(k)
	s, _ := v.(string)
	return s
}

func getNumber(o *jsonx.Object, k string) (float64, bool) {
	v, ok := o.Get(k)
	if !ok {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := n.Float64()
	return f, err == nil
}

func (s *Synthesizer) value(schema any, depth int) (any, error) {
	o, ok := schema.(*jsonx.Object)
	if !ok {
		return nil, nil // `true`, missing or malformed schema: anything goes
	}
	if depth > maxDepth {
		return cycle{}, nil
	}
	if ref := getString(o, "$ref"); ref != "" {
		for _, r := range s.stack {
			if r == ref {
				return cycle{}, nil
			}
		}
		target, err := Resolve(s.root, ref)
		if err != nil {
			return nil, err
		}
		s.stack = append(s.stack, ref)
		v, err := s.value(target, depth+1)
		s.stack = s.stack[:len(s.stack)-1]
		return v, err
	}

	// Explicit values win over synthesis.
	if v, ok := o.Get("example"); ok {
		return v, nil
	}
	if v, ok := o.Get("examples"); ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			return arr[0], nil
		}
	}
	if v, ok := o.Get("const"); ok {
		return v, nil
	}
	if v, ok := o.Get("default"); ok {
		return v, nil
	}
	if v, ok := o.Get("enum"); ok {
		if arr, ok := v.([]any); ok && len(arr) > 0 {
			return arr[0], nil
		}
	}

	if v, ok := o.Get("allOf"); ok {
		if parts, ok := v.([]any); ok {
			return s.allOf(o, parts, depth)
		}
	}
	for _, k := range []string{"oneOf", "anyOf"} {
		if v, ok := o.Get(k); ok {
			if parts, ok := v.([]any); ok && len(parts) > 0 {
				for _, p := range parts {
					val, err := s.value(p, depth+1)
					if err != nil {
						return nil, err
					}
					if _, isCycle := val.(cycle); !isCycle {
						return val, nil
					}
				}
				return cycle{}, nil
			}
		}
	}

	switch schemaType(o) {
	case "object":
		return s.object(o, depth)
	case "array":
		return s.array(o, depth)
	case "string":
		return synthString(o), nil
	case "integer":
		return json.Number(strconv.FormatInt(int64(synthNumber(o, true)), 10)), nil
	case "number":
		return json.Number(strconv.FormatFloat(synthNumber(o, false), 'f', -1, 64)), nil
	case "boolean":
		return true, nil
	case "null":
		return nil, nil
	}
	return nil, nil
}

// schemaType reads "type" (a string, or an OpenAPI 3.1 array) or infers it.
func schemaType(o *jsonx.Object) string {
	switch t := mustGet(o, "type").(type) {
	case string:
		return t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
		return "null"
	}
	if _, ok := o.Get("properties"); ok {
		return "object"
	}
	if _, ok := o.Get("additionalProperties"); ok {
		return "object"
	}
	if _, ok := o.Get("items"); ok {
		return "array"
	}
	return ""
}

func mustGet(o *jsonx.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func (s *Synthesizer) allOf(o *jsonx.Object, parts []any, depth int) (any, error) {
	merged := jsonx.NewObject()
	var last any
	isObject := false
	for _, p := range parts {
		v, err := s.value(p, depth+1)
		if err != nil {
			return nil, err
		}
		switch t := v.(type) {
		case cycle:
			continue
		case *jsonx.Object:
			isObject = true
			for _, k := range t.Keys() {
				val, _ := t.Get(k)
				merged.Set(k, val)
			}
		default:
			last = v
		}
	}
	if _, ok := o.Get("properties"); ok {
		v, err := s.object(o, depth)
		if err != nil {
			return nil, err
		}
		if t, ok := v.(*jsonx.Object); ok {
			isObject = true
			for _, k := range t.Keys() {
				val, _ := t.Get(k)
				merged.Set(k, val)
			}
		}
	}
	if isObject {
		return merged, nil
	}
	return last, nil
}

func (s *Synthesizer) object(o *jsonx.Object, depth int) (any, error) {
	out := jsonx.NewObject()
	props, _ := mustGet(o, "properties").(*jsonx.Object)
	if props != nil {
		for _, name := range props.Keys() {
			ps, _ := props.Get(name)
			if po, ok := ps.(*jsonx.Object); ok {
				if wo, _ := po.Get("writeOnly"); wo == true {
					continue // writeOnly fields (passwords) never appear in responses
				}
			}
			v, err := s.value(ps, depth+1)
			if err != nil {
				return nil, err
			}
			if _, isCycle := v.(cycle); isCycle {
				continue
			}
			out.Set(name, v)
		}
	}
	if props == nil || props.Len() == 0 {
		if ap, ok := mustGet(o, "additionalProperties").(*jsonx.Object); ok {
			v, err := s.value(ap, depth+1)
			if err != nil {
				return nil, err
			}
			if _, isCycle := v.(cycle); !isCycle {
				out.Set("additionalProp1", v)
			}
		}
	}
	return out, nil
}

func (s *Synthesizer) array(o *jsonx.Object, depth int) (any, error) {
	n := 1
	if f, ok := getNumber(o, "minItems"); ok && f > 1 {
		n = int(math.Min(f, 20))
	}
	if f, ok := getNumber(o, "maxItems"); ok && float64(n) > f {
		n = int(f)
	}
	out := []any{}
	items, _ := o.Get("items")
	for i := 0; i < n; i++ {
		v, err := s.value(items, depth+1)
		if err != nil {
			return nil, err
		}
		if _, isCycle := v.(cycle); isCycle {
			return []any{}, nil
		}
		out = append(out, v)
	}
	return out, nil
}

var formatExamples = map[string]string{
	"date-time":     "2026-01-15T09:30:00Z",
	"date":          "2026-01-15",
	"time":          "09:30:00",
	"email":         "user@example.com",
	"idn-email":     "user@example.com",
	"uuid":          "3fa85f64-5717-4562-b3fc-2c963f66afa6",
	"uri":           "https://example.com",
	"url":           "https://example.com",
	"iri":           "https://example.com",
	"uri-reference": "/example",
	"hostname":      "example.com",
	"idn-hostname":  "example.com",
	"ipv4":          "192.0.2.1",
	"ipv6":          "2001:db8::1",
	"byte":          "ZXhhbXBsZQ==",
	"binary":        "example",
	"password":      "********",
	"duration":      "PT1H",
	"regex":         "^example$",
}

func synthString(o *jsonx.Object) string {
	if v, ok := formatExamples[getString(o, "format")]; ok {
		return v
	}
	s := "string"
	if f, ok := getNumber(o, "minLength"); ok && int(f) > len(s) {
		s += strings.Repeat("x", int(math.Min(f, 256))-len(s))
	}
	if f, ok := getNumber(o, "maxLength"); ok && int(f) < len(s) && f >= 0 {
		s = s[:int(f)]
	}
	return s
}

// synthNumber picks 0 when allowed, otherwise the closest bound.
func synthNumber(o *jsonx.Object, integer bool) float64 {
	lo, hasLo := getNumber(o, "minimum")
	hi, hasHi := getNumber(o, "maximum")
	// OpenAPI 3.1: exclusiveMinimum is a number; 3.0: a boolean modifier.
	if f, ok := getNumber(o, "exclusiveMinimum"); ok {
		lo, hasLo = f, true
		if integer {
			lo = math.Floor(f) + 1
		} else {
			lo = f + 0.5
		}
	} else if b, _ := o.Get("exclusiveMinimum"); b == true && hasLo {
		if integer {
			lo = math.Floor(lo) + 1
		} else {
			lo += 0.5
		}
	}
	if f, ok := getNumber(o, "exclusiveMaximum"); ok {
		hi, hasHi = f, true
		if integer {
			hi = math.Ceil(f) - 1
		} else {
			hi = f - 0.5
		}
	} else if b, _ := o.Get("exclusiveMaximum"); b == true && hasHi {
		if integer {
			hi = math.Ceil(hi) - 1
		} else {
			hi -= 0.5
		}
	}
	v := 0.0
	if hasLo && v < lo {
		v = lo
	}
	if hasHi && v > hi {
		v = hi
	}
	if integer {
		v = math.Ceil(v)
	}
	return v
}
