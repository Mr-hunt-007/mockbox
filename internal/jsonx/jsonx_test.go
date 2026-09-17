package jsonx

import (
	"errors"
	"strings"
	"testing"
)

func TestRoundTripPreservesOrderAndNumbers(t *testing.T) {
	in := `{
  "zeta": 1,
  "alpha": {
    "b": 12345678901234567890,
    "a": 1.50
  },
  "list": [
    true,
    null,
    "<b>&</b>"
  ],
  "empty": {},
  "none": []
}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	out := string(Marshal(v, "  "))
	if out != in {
		t.Fatalf("round trip changed the document:\n got: %s\nwant: %s", out, in)
	}
}

func TestCompactMarshal(t *testing.T) {
	v, err := Parse([]byte(`{"b": [1, 2], "a": {"x": "y"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(Marshal(v, "")), `{"b":[1,2],"a":{"x":"y"}}`; got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestSyntaxErrorPosition(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		line, col int
		msg       string
	}{
		{"bad char line 3", "{\n  \"a\": 1,\n  }\n", 3, 3, "invalid character '}'"},
		{"trailing comma in array", `[1, 2,]`, 1, 7, "invalid character ']'"},
		{"unterminated", `{"a": 1`, 1, 8, "unexpected end"},
		{"truncated after newline", "{\n  \"profile\": {\"theme\": \n", 3, 1, "unexpected end"},
		{"bad last byte", `[1,]`, 1, 4, "invalid character ']'"},
		{"garbage after value", "{}\nx", 2, 1, "after top-level value"},
		{"empty", "   ", 1, 1, "empty document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.in))
			var se *SyntaxError
			if !errors.As(err, &se) {
				t.Fatalf("want *SyntaxError, got %v", err)
			}
			if se.Line != tt.line || se.Column != tt.col {
				t.Errorf("position = %d:%d, want %d:%d (%v)", se.Line, se.Column, tt.line, tt.col, se)
			}
			if !strings.Contains(se.Msg, tt.msg) {
				t.Errorf("message %q does not contain %q", se.Msg, tt.msg)
			}
		})
	}
}

func TestParseStripsBOM(t *testing.T) {
	v, err := Parse([]byte("\xef\xbb\xbf{\"a\": 1}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.(*Object).Get("a"); !ok {
		t.Fatal("key a missing")
	}
}

func TestObjectOperations(t *testing.T) {
	o := NewObject()
	o.Set("a", 1)
	o.Set("b", 2)
	o.Set("c", 3)
	o.Set("a", 10) // keeps position
	if got := strings.Join(o.Keys(), ","); got != "a,b,c" {
		t.Fatalf("keys %s", got)
	}
	o.SetFirst("c", 30)
	if got := strings.Join(o.Keys(), ","); got != "c,a,b" {
		t.Fatalf("keys after SetFirst %s", got)
	}
	if !o.Delete("a") || o.Delete("missing") {
		t.Fatal("delete result wrong")
	}
	if got := strings.Join(o.Keys(), ","); got != "c,b" {
		t.Fatalf("keys after delete %s", got)
	}
}

func TestCloneIsDeep(t *testing.T) {
	v, _ := Parse([]byte(`{"a": {"b": [1, {"c": 2}]}}`))
	c := Clone(v).(*Object)
	inner, _ := c.Get("a")
	arr, _ := inner.(*Object).Get("b")
	arr.([]any)[1].(*Object).Set("c", "changed")
	if got := string(Marshal(v, "")); got != `{"a":{"b":[1,{"c":2}]}}` {
		t.Fatalf("original modified through clone: %s", got)
	}
}
