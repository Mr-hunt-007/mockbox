package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func testServer() *Server {
	return &Server{
		Name: "demo", Version: "1.2.3", Instructions: "use demo_echo",
		Tools: []Tool{
			{
				Name: "demo_echo", Description: "echo", Annotations: ReadOnly("Echo"),
				InputSchema: Object(map[string]any{"text": String("text")}, "text"),
				Handler: func(ctx context.Context, args json.RawMessage) (Result, error) {
					var a struct {
						Text string `json:"text"`
					}
					if err := Decode(args, &a); err != nil {
						return Result{}, err
					}
					return JSONResult(map[string]string{"echo": a.Text})
				},
			},
			{
				Name: "demo_fail", Description: "fails", InputSchema: Object(map[string]any{}),
				Handler: func(ctx context.Context, args json.RawMessage) (Result, error) {
					return Result{}, errors.New("not a git repository")
				},
			},
			{
				Name: "demo_panic", Description: "panics", InputSchema: Object(map[string]any{}),
				Handler: func(ctx context.Context, args json.RawMessage) (Result, error) { panic("boom") },
			},
			{
				Name: "demo_slow", Description: "waits for cancel", InputSchema: Object(map[string]any{}),
				Handler: func(ctx context.Context, args json.RawMessage) (Result, error) {
					select {
					case <-ctx.Done():
						return Result{}, ctx.Err()
					case <-time.After(5 * time.Second):
						return TextResult("finished"), nil
					}
				},
			},
		},
	}
}

// client drives a server over pipes, one JSON line at a time.
type client struct {
	t    *testing.T
	in   io.WriteCloser
	out  *bufio.Scanner
	done chan error
}

func start(t *testing.T) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, out: bufio.NewScanner(outR), done: make(chan error, 1)}
	c.out.Buffer(make([]byte, 1<<20), 1<<20)
	go func() {
		c.done <- testServer().Serve(context.Background(), inR, outW)
		outW.Close()
	}()
	t.Cleanup(func() { inW.Close() })
	return c
}

func (c *client) send(s string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, s+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() map[string]any {
	c.t.Helper()
	if !c.out.Scan() {
		c.t.Fatalf("no response: %v", c.out.Err())
	}
	var m map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &m); err != nil {
		c.t.Fatalf("bad JSON %q: %v", c.out.Text(), err)
	}
	return m
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	r := c.recv()["result"].(map[string]any)
	if r["protocolVersion"] != "2025-03-26" {
		t.Errorf("version = %v", r["protocolVersion"])
	}
	if r["serverInfo"].(map[string]any)["version"] != "1.2.3" || r["instructions"] != "use demo_echo" {
		t.Errorf("initialize result = %v", r)
	}
	if _, ok := r["capabilities"].(map[string]any)["tools"]; !ok {
		t.Error("tools capability missing")
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if v := c.recv()["result"].(map[string]any)["protocolVersion"]; v != SupportedVersions[0] {
		t.Errorf("unknown version should get newest, got %v", v)
	}
}

func TestNotificationsGetNoReply(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	c.send(`{"jsonrpc":"2.0","method":"something/else"}`)
	c.send(`{"jsonrpc":"2.0","id":"p","method":"ping"}`)
	r := c.recv()
	if r["id"] != "p" {
		t.Fatalf("expected only the ping reply first, got %v", r)
	}
}

func TestListAndCall(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools := c.recv()["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 4 || tools[0].(map[string]any)["name"] != "demo_echo" {
		t.Fatalf("tools = %v", tools)
	}
	ann := tools[0].(map[string]any)["annotations"].(map[string]any)
	if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
		t.Errorf("annotations = %v", ann)
	}

	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"demo_echo","arguments":{"text":"hi"}}}`)
	res := c.recv()["result"].(map[string]any)
	if res["structuredContent"].(map[string]any)["echo"] != "hi" || res["isError"] != nil {
		t.Errorf("call result = %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"echo": "hi"`) {
		t.Errorf("text = %q", text)
	}
}

func TestToolErrors(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"demo_fail"}}`)
	res := c.recv()["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "not a git repository") {
		t.Errorf("fail = %v", res)
	}
	c.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"demo_echo","arguments":{"txt":"typo"}}}`)
	res = c.recv()["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(res["content"].([]any)[0].(map[string]any)["text"].(string), "unknown field") {
		t.Errorf("unknown argument should be a tool error: %v", res)
	}
	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"demo_panic"}}`)
	if res := c.recv()["result"].(map[string]any); res["isError"] != true {
		t.Errorf("panic should become a tool error: %v", res)
	}
	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nope"}}`)
	if e := c.recv()["error"].(map[string]any); e["code"].(float64) != CodeInvalidParams {
		t.Errorf("unknown tool: %v", e)
	}
}

func TestProtocolErrors(t *testing.T) {
	c := start(t)
	c.send(`{not json`)
	if e := c.recv()["error"].(map[string]any); e["code"].(float64) != CodeParseError {
		t.Errorf("parse error: %v", e)
	}
	c.send(`{"jsonrpc":"2.0","id":7,"method":"resources/list"}`)
	if r := c.recv(); r["error"].(map[string]any)["code"].(float64) != CodeMethodNotFound || r["id"].(float64) != 7 {
		t.Errorf("method not found: %v", r)
	}
	c.send(`{"id":8,"method":"ping"}`)
	if e := c.recv()["error"].(map[string]any); e["code"].(float64) != CodeInvalidRequest {
		t.Errorf("missing jsonrpc: %v", e)
	}
	c.send(`[{"jsonrpc":"2.0","id":9,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"}]`)
	if r := c.recv(); r["id"].(float64) != 9 {
		t.Errorf("batch: %v", r)
	}
}

func TestCancellation(t *testing.T) {
	c := start(t)
	c.send(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"demo_slow"}}`)
	time.Sleep(50 * time.Millisecond)
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow"}}`)
	c.send(`{"jsonrpc":"2.0","id":"after","method":"ping"}`)
	if r := c.recv(); r["id"] != "after" {
		t.Fatalf("cancelled request must not be answered, got %v", r)
	}
}

func TestEOFEndsServe(t *testing.T) {
	c := start(t)
	c.in.Close()
	select {
	case err := <-c.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return at EOF")
	}
}

func TestValidate(t *testing.T) {
	s := &Server{Tools: []Tool{{Name: "x"}}}
	if err := s.Serve(context.Background(), strings.NewReader(""), io.Discard); err == nil {
		t.Error("tool without handler should be rejected")
	}
}

func TestDecodeEmpty(t *testing.T) {
	var v struct{ A int }
	for _, in := range []string{"", "null", "{}"} {
		if err := Decode(json.RawMessage(in), &v); err != nil {
			t.Errorf("Decode(%q): %v", in, err)
		}
	}
}

func TestJSONResultKeepsURLsReadable(t *testing.T) {
	res, err := JSONResult(map[string]any{"path": "/posts?a=1&b=<2>", "raw": json.RawMessage(`{"link":"<http://x/?a=1&b=2>"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, `\u0026`) || !strings.Contains(res.Text, `"/posts?a=1&b=<2>"`) || !strings.Contains(res.Text, `"<http://x/?a=1&b=2>"`) {
		t.Fatalf("escaped output: %s", res.Text)
	}
	if !json.Valid(res.Structured.(json.RawMessage)) {
		t.Fatalf("structured is not valid JSON")
	}
}
