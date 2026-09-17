package mcptools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Mr-hunt-007/mockbox/internal/app"
	"github.com/Mr-hunt-007/mockbox/internal/mcp"
)

const dbJSON = `{
  "users": [
    {"id": 1, "name": "Ada", "role": "admin", "age": 36},
    {"id": 2, "name": "Alan", "role": "user", "age": 41},
    {"id": 3, "name": "Grace", "role": "admin", "age": 85}
  ],
  "posts": [
    {"id": 1, "title": "Notes & sketches", "userId": 1},
    {"id": 2, "title": "Machinery", "userId": 2},
    {"id": 3, "title": "Sketch", "userId": 1}
  ],
  "profile": {"theme": "dark"},
  "version": 3
}
`

const specJSON = `{
  "openapi": "3.0.3",
  "servers": [{"url": "https://api.example.com/v1"}],
  "paths": {
    "/orders": {
      "get": {"responses": {"200": {"description": "", "content": {"application/json": {
        "schema": {"type": "array", "items": {"$ref": "#/components/schemas/Order"}}}}}}},
      "post": {"responses": {"201": {"description": "", "content": {"application/json": {"example": {"id": "ord_1"}}}}}}
    },
    "/orders/{orderId}": {
      "get": {"responses": {
        "200": {"description": "", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Order"}}}},
        "404": {"description": "", "content": {"application/json": {"examples": {
          "missing": {"value": {"error": "order not found"}},
          "gone": {"value": {"error": "order deleted"}}
        }}}}
      }}
    }
  },
  "components": {"schemas": {"Order": {"type": "object", "properties": {
    "id": {"type": "string", "format": "uuid"},
    "total": {"type": "number", "minimum": 1}
  }}}}
}
`

type fixture struct {
	dir, db, spec, routes string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{dir: dir, db: filepath.Join(dir, "db.json"), spec: filepath.Join(dir, "openapi.json"), routes: filepath.Join(dir, "routes.json")}
	for path, content := range map[string]string{f.db: dbJSON, f.spec: specJSON, f.routes: `{"/api/*": "/$1", "/me": "/users/1"}`} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func tool(t *testing.T, s *mcp.Server, name string) mcp.Tool {
	t.Helper()
	for _, tl := range s.Tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return mcp.Tool{}
}

// call runs a tool handler and decodes its JSON text into out.
func call(t *testing.T, s *mcp.Server, name string, args any, out any) error {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool(t, s, name).Handler(context.Background(), raw)
	if err != nil {
		return err
	}
	if res.Structured == nil {
		t.Fatalf("%s: no structured content: %s", name, res.Text)
	}
	if err := json.Unmarshal([]byte(res.Text), out); err != nil {
		t.Fatalf("%s: bad JSON %s: %v", name, res.Text, err)
	}
	return nil
}

func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatalf("body %q: %v", raw, err)
	}
	return b.String()
}

func TestRoutesDatabase(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{})
	var r RoutesResult
	if err := call(t, s, "mockbox_routes", map[string]any{"file": f.db, "routes": f.routes}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mode != "database" || r.TotalRoutes != 16 || len(r.Routes) != 16 || r.Truncated {
		t.Fatalf("routes: %+v", r)
	}
	if r.Routes[2] != (RouteJSON{"GET", "/users/:id/posts"}) {
		t.Errorf("nested route: %+v", r.Routes[2])
	}
	if fmt.Sprint(r.Collections, r.Singular) != "[{users 3} {posts 3}] [profile]" {
		t.Errorf("resources: %v %v", r.Collections, r.Singular)
	}
	if len(r.Relations) != 1 || fmt.Sprintf("%+v", r.Relations[0]) != "{Parent:users Child:posts ForeignKey:userId Expand:user}" {
		t.Errorf("relations: %+v", r.Relations)
	}
	if len(r.Rewrites) != 2 || r.Rewrites[0] != (RewriteJSON{"/api/*", "/$1"}) {
		t.Errorf("rewrites: %+v", r.Rewrites)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0], `"version"`) {
		t.Errorf("skipped: %v", r.Skipped)
	}
}

func TestRoutesOpenAPIAndCap(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.spec})
	var r RoutesResult
	if err := call(t, s, "mockbox_routes", map[string]any{"max_routes": 2}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mode != "openapi" || r.BasePath != "/v1" || r.TotalRoutes != 3 || len(r.Routes) != 2 || !r.Truncated || !strings.Contains(r.Note, "max_routes") {
		t.Fatalf("openapi routes: %+v", r)
	}
	if r.Collections != nil || r.Relations != nil {
		t.Errorf("database fields in openapi mode: %+v", r)
	}
}

func TestArgumentErrors(t *testing.T) {
	f := newFixture(t)
	noDefault := NewServer(Options{})
	withDefault := NewServer(Options{File: f.db})
	tests := []struct {
		s    *mcp.Server
		tool string
		args map[string]any
		want string
	}{
		{noDefault, "mockbox_routes", map[string]any{}, "file is required"},
		{noDefault, "mockbox_routes", map[string]any{"file": filepath.Join(f.dir, "missing.json")}, "cannot read"},
		{noDefault, "mockbox_routes", map[string]any{"file": f.db, "routes": filepath.Join(f.dir, "nope.json")}, "cannot read routes file"},
		{noDefault, "mockbox_routes", map[string]any{"file": f.db, "max_routes": -1}, "max_routes must be 0 or more"},
		{noDefault, "mockbox_routes", map[string]any{"file": f.db, "depth": 1}, `unknown field "depth"`},
		{withDefault, "mockbox_request", map[string]any{"path": "/users"}, "method is required"},
		{withDefault, "mockbox_request", map[string]any{"method": "BREW", "path": "/users"}, "unsupported method"},
		{withDefault, "mockbox_request", map[string]any{"method": "GET"}, "path is required"},
		{withDefault, "mockbox_request", map[string]any{"method": "GET", "path": "http://localhost:3000/users"}, "must start with /"},
		{withDefault, "mockbox_request", map[string]any{"method": "GET", "path": "/users", "max_body_bytes": 1 << 30}, "max_body_bytes must be at most"},
		{withDefault, "mockbox_request", map[string]any{"method": "GET", "path": "/users", "headers": map[string]any{"X": 1}}, "invalid arguments"},
		{withDefault, "mockbox_example", map[string]any{"method": "GET", "path": "/users"}, "is a JSON database, not an OpenAPI spec"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "GET", "path": "/nope"}, "no operation"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "DELETE", "path": "/orders"}, "defined methods: GET, POST"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "GET", "path": "/orders", "status": "500"}, "status 500, but GET /orders only defines responses 200"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "GET", "path": "/orders/1", "status": "404", "example": "x"}, "example x, but the available examples are missing, gone"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "GET", "path": "/orders/1", "status": "404,code=200"}, "single responses key"},
		{withDefault, "mockbox_example", map[string]any{"file": f.spec, "method": "GET", "path": "/orders/1", "status": true}, "status must be a responses key"},
	}
	for _, tt := range tests {
		t.Run(tt.tool+" "+tt.want, func(t *testing.T) {
			var out map[string]any
			err := call(t, tt.s, tt.tool, tt.args, &out)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v (result %v), want error containing %q", err, out, tt.want)
			}
		})
	}
}

func TestRequestQueryHeadersAndRewrites(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.db, Routes: f.routes})
	var r RequestResult
	if err := call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/api/posts?userId=1&_page=1&_limit=1&_expand=user"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != 200 || r.RewrittenTo != "/posts?userId=1&_page=1&_limit=1&_expand=user" || r.Headers["X-Total-Count"] != "2" {
		t.Fatalf("result: %+v", r)
	}
	if !strings.Contains(r.Headers["Link"], `<http://127.0.0.1:3000/api/posts?_expand=user&_limit=1&_page=2&userId=1>; rel="next"`) {
		t.Errorf("link: %s", r.Headers["Link"])
	}
	if got := compact(t, r.Body); got != `[{"id":1,"title":"Notes & sketches","userId":1,"user":{"id":1,"name":"Ada","role":"admin","age":36}}]` {
		t.Errorf("body: %s", got)
	}

	// A mock API error is a normal result, not a tool error.
	r = RequestResult{}
	if err := call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/users/9"}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != 404 || compact(t, r.Body) != `{"error":"Not Found","status":404,"message":"users with id \"9\" not found"}` {
		t.Errorf("404: %+v %s", r, r.Body)
	}

	// The HTML index is returned as text; Accept switches it to JSON.
	r = RequestResult{}
	_ = call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/"}, &r)
	if r.BodyText == nil || !strings.HasPrefix(*r.BodyText, "<!doctype html>") || r.Body != nil {
		t.Errorf("html index: %+v", r)
	}
	r = RequestResult{}
	_ = call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/", "headers": map[string]string{"Accept": "application/json"}}, &r)
	if r.Body == nil || !strings.Contains(string(r.Body), `"mode": "database"`) {
		t.Errorf("json index: %+v", r)
	}
}

func TestRequestWritesAreNotKeptAndFileUntouched(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.db})
	var r RequestResult
	if err := call(t, s, "mockbox_request", map[string]any{"method": "POST", "path": "/users", "body": map[string]any{"name": "Barbara"}}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Status != 201 || r.Headers["Location"] != "/users/4" || compact(t, r.Body) != `{"id":4,"name":"Barbara"}` {
		t.Fatalf("post: %+v %s", r, r.Body)
	}
	for _, args := range []map[string]any{
		{"method": "DELETE", "path": "/users/1"},
		{"method": "PATCH", "path": "/profile", "body": map[string]any{"theme": "light"}},
	} {
		r = RequestResult{}
		if err := call(t, s, "mockbox_request", args, &r); err != nil || r.Status != 200 {
			t.Fatalf("%v: %v %+v", args, err, r)
		}
	}
	r = RequestResult{}
	_ = call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/users"}, &r)
	if r.Headers["X-Total-Count"] != "3" {
		t.Errorf("a previous call leaked into this one: %s", r.Body)
	}
	b, _ := os.ReadFile(f.db)
	if string(b) != dbJSON {
		t.Errorf("file was modified:\n%s", b)
	}

	// Invalid bodies come back as the API's 400.
	r = RequestResult{}
	_ = call(t, s, "mockbox_request", map[string]any{"method": "POST", "path": "/users", "body": []int{1}}, &r)
	if r.Status != 400 || !strings.Contains(string(r.Body), "must be a JSON object, got an array") {
		t.Errorf("array body: %+v", r)
	}
	r = RequestResult{}
	_ = call(t, s, "mockbox_request", map[string]any{"method": "POST", "path": "/users", "body": nil}, &r)
	if r.Status != 400 || !strings.Contains(string(r.Body), "request body is empty") {
		t.Errorf("null body: %+v", r)
	}
}

func TestRequestTruncation(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.db})
	var r RequestResult
	if err := call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/users", "max_body_bytes": 50}, &r); err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.Body != nil || r.BodyText == nil || len(*r.BodyText) != 50 || r.BodyBytes <= 50 || !strings.Contains(r.Note, "max_body_bytes") {
		t.Fatalf("truncation: %+v", r)
	}
}

func TestRequestOpenAPI(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.spec})
	var r RequestResult
	if err := call(t, s, "mockbox_request", map[string]any{"method": "GET", "path": "/v1/orders/7", "headers": map[string]string{"Prefer": "code=404, example=gone"}}, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mode != "openapi" || r.Status != 404 || compact(t, r.Body) != `{"error":"order deleted"}` {
		t.Fatalf("openapi request: %+v %s", r, r.Body)
	}
}

func TestExample(t *testing.T) {
	f := newFixture(t)
	s := NewServer(Options{File: f.spec})
	tests := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"method": "GET", "path": "/orders"}, `/orders 200 200 [200] schema [] [{"id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","total":1}]`},
		{map[string]any{"method": "get", "path": "/orders/{orderId}", "status": "404"}, `/orders/{orderId} 404 404 [200 404] examples/missing [missing gone] {"error":"order not found"}`},
		{map[string]any{"method": "GET", "path": "/v1/orders/abc", "status": "404", "example": "gone"}, `/orders/{orderId} 404 404 [200 404] examples/gone [missing gone] {"error":"order deleted"}`},
		{map[string]any{"method": "GET", "path": "/orders/1", "status": 404}, `/orders/{orderId} 404 404 [200 404] examples/missing [missing gone] {"error":"order not found"}`},
		{map[string]any{"method": "POST", "path": "/orders"}, `/orders 201 201 [201] example [] {"id":"ord_1"}`},
	}
	for _, tt := range tests {
		var r ExampleResult
		if err := call(t, s, "mockbox_example", tt.args, &r); err != nil {
			t.Fatalf("%v: %v", tt.args, err)
		}
		got := fmt.Sprintf("%s %d %s %v %s %v %s", r.Operation, r.Status, r.Response, r.Responses, r.Source, r.Examples, compact(t, r.Body))
		if got != tt.want {
			t.Errorf("%v:\n got %s\nwant %s", tt.args, got, tt.want)
		}
	}
}

// TestServerOverPipes runs `mockbox --mcp` in-process, exactly as main does.
func TestServerOverPipes(t *testing.T) {
	f := newFixture(t)
	for _, extra := range [][]string{nil, {"--allow-destructive"}} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			done := make(chan int, 1)
			args := append([]string{"--mcp", f.db, "--json"}, extra...)
			var stderr strings.Builder
			go func() {
				done <- app.Run(args, inR, outW, &stderr, Serve)
				outW.Close()
			}()
			sc := bufio.NewScanner(outR)
			sc.Buffer(make([]byte, 1<<20), 1<<20)
			send := func(s string) {
				t.Helper()
				if _, err := io.WriteString(inW, s+"\n"); err != nil {
					t.Fatal(err)
				}
			}
			recv := func() map[string]any {
				t.Helper()
				if !sc.Scan() {
					t.Fatalf("no response: %v (stderr %s)", sc.Err(), stderr.String())
				}
				var m map[string]any
				if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
					t.Fatalf("bad line %q: %v", sc.Text(), err)
				}
				return m
			}

			send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
			init := recv()["result"].(map[string]any)
			if init["protocolVersion"] != "2025-06-18" || init["serverInfo"].(map[string]any)["version"] != app.Version || !strings.Contains(init["instructions"].(string), "mockbox <file>") {
				t.Fatalf("initialize: %v", init)
			}
			send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			var names []string
			for _, tl := range recv()["result"].(map[string]any)["tools"].([]any) {
				m := tl.(map[string]any)
				names = append(names, m["name"].(string))
				ann := m["annotations"].(map[string]any)
				if ann["readOnlyHint"] != true || ann["destructiveHint"] != false {
					t.Errorf("%s is not annotated read-only: %v", m["name"], ann)
				}
				props := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
				for p, schema := range props {
					if d, _ := schema.(map[string]any)["description"].(string); d == "" {
						t.Errorf("%s.%s has no description", m["name"], p)
					}
				}
			}
			// mockbox has no destructive tools, so the list is the same with and without --allow-destructive.
			if strings.Join(names, ",") != "mockbox_example,mockbox_request,mockbox_routes" {
				t.Fatalf("tools: %v", names)
			}

			calls := []struct{ name, args, want string }{
				{"mockbox_routes", `{}`, `"relations"`},
				{"mockbox_request", `{"method":"GET","path":"/users?role=admin&_sort=-age"}`, `"name": "Grace"`},
				{"mockbox_example", `{"file":` + jsonString(f.spec) + `,"method":"GET","path":"/orders/1","status":"404"}`, `"order not found"`},
			}
			for i, c := range calls {
				send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, 10+i, c.name, c.args))
				res := recv()["result"].(map[string]any)
				text := res["content"].([]any)[0].(map[string]any)["text"].(string)
				if res["isError"] == true || !strings.Contains(text, c.want) || res["structuredContent"] == nil {
					t.Errorf("%s: %v", c.name, res)
				}
			}
			send(`{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"mockbox_request","arguments":{"method":"GET","path":"users"}}}`)
			res := recv()["result"].(map[string]any)
			if res["isError"] != true {
				t.Errorf("bad path should be a tool error: %v", res)
			}

			inW.Close()
			if code := <-done; code != app.ExitOK {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
		})
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
