// Package jsonx decodes and encodes JSON while keeping object key order.
//
// encoding/json maps lose key order, which would reorder a user's file when
// mockbox writes it back with --persist. Values produced by Parse are:
// nil, bool, string, json.Number, []any and *Object.
package jsonx

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Object is a JSON object that remembers the order of its keys.
type Object struct {
	keys []string
	vals map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object {
	return &Object{vals: map[string]any{}}
}

// Get returns the value stored under k.
func (o *Object) Get(k string) (any, bool) {
	v, ok := o.vals[k]
	return v, ok
}

// Set stores v under k. New keys are appended; existing keys keep their position.
func (o *Object) Set(k string, v any) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

// SetFirst stores v under k and moves k to the front.
func (o *Object) SetFirst(k string, v any) {
	o.Delete(k)
	o.keys = append([]string{k}, o.keys...)
	o.vals[k] = v
}

// Delete removes k and reports whether it was present.
func (o *Object) Delete(k string) bool {
	if _, ok := o.vals[k]; !ok {
		return false
	}
	delete(o.vals, k)
	for i, key := range o.keys {
		if key == k {
			o.keys = append(o.keys[:i:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

// Keys returns a copy of the keys in order.
func (o *Object) Keys() []string {
	return append([]string(nil), o.keys...)
}

// Len returns the number of keys.
func (o *Object) Len() int { return len(o.keys) }

// ShallowCopy returns a new object with the same keys and values.
func (o *Object) ShallowCopy() *Object {
	c := &Object{keys: append([]string(nil), o.keys...), vals: make(map[string]any, len(o.vals))}
	for k, v := range o.vals {
		c.vals[k] = v
	}
	return c
}

// Clone deep-copies a value produced by Parse.
func Clone(v any) any {
	switch t := v.(type) {
	case *Object:
		c := &Object{keys: append([]string(nil), t.keys...), vals: make(map[string]any, len(t.vals))}
		for k, val := range t.vals {
			c.vals[k] = Clone(val)
		}
		return c
	case []any:
		c := make([]any, len(t))
		for i, val := range t {
			c[i] = Clone(val)
		}
		return c
	default:
		return v
	}
}

// SyntaxError describes invalid JSON with a 1-based line and column.
type SyntaxError struct {
	Msg    string
	Line   int
	Column int
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s (line %d, column %d)", e.Msg, e.Line, e.Column)
}

// Parse decodes a single JSON document, preserving key order and number text.
func Parse(data []byte) (any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &SyntaxError{Msg: "empty document", Line: 1, Column: 1}
	}
	// A first pass with json.Unmarshal gives precise syntax error offsets,
	// including trailing garbage after the top-level value.
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			off := se.Offset
			// For a bad character encoding/json reports the offset just past
			// it; for truncated input it reports the end, which is already
			// the right place to point at.
			if !strings.HasPrefix(se.Error(), "unexpected end") && off > 0 {
				off--
			}
			line, col := position(data, off)
			return nil, &SyntaxError{Msg: se.Error(), Line: line, Column: col}
		}
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return decodeValue(dec)
}

// position converts a byte offset into a 1-based line and column.
func position(data []byte, offset int64) (int, int) {
	off := int(offset)
	if off > len(data) {
		off = len(data)
	}
	line, col := 1, 1
	for _, b := range data[:off] {
		if b == '\n' {
			line++
			col = 1
		} else {
			col++
		}
	}
	return line, col
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := NewObject()
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := kt.(string)
				if !ok {
					return nil, fmt.Errorf("unexpected object key %v", kt)
				}
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				val, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	default:
		return tok, nil
	}
}

// Marshal encodes v. An empty indent produces compact output; otherwise each
// nesting level is indented by indent. HTML characters are not escaped.
func Marshal(v any, indent string) []byte {
	var buf bytes.Buffer
	write(&buf, v, indent, 0)
	return buf.Bytes()
}

func newline(buf *bytes.Buffer, indent string, depth int) {
	if indent == "" {
		return
	}
	buf.WriteByte('\n')
	for i := 0; i < depth; i++ {
		buf.WriteString(indent)
	}
}

func write(buf *bytes.Buffer, v any, indent string, depth int) {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case string:
		writeString(buf, t)
	case json.Number:
		buf.WriteString(t.String())
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case float64:
		buf.WriteString(strconv.FormatFloat(t, 'f', -1, 64))
	case *Object:
		if t.Len() == 0 {
			buf.WriteString("{}")
			return
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(buf, indent, depth+1)
			writeString(buf, k)
			buf.WriteByte(':')
			if indent != "" {
				buf.WriteByte(' ')
			}
			write(buf, t.vals[k], indent, depth+1)
		}
		newline(buf, indent, depth)
		buf.WriteByte('}')
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return
		}
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(buf, indent, depth+1)
			write(buf, e, indent, depth+1)
		}
		newline(buf, indent, depth)
		buf.WriteByte(']')
	default:
		b, err := json.Marshal(t)
		if err != nil {
			buf.WriteString("null")
			return
		}
		buf.Write(b)
	}
}

func writeString(buf *bytes.Buffer, s string) {
	var sb bytes.Buffer
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	buf.Write(bytes.TrimRight(sb.Bytes(), "\n"))
}
