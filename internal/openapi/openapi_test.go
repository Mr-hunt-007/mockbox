package openapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

func mustParse(t *testing.T, s string) any {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return v
}

func synth(t *testing.T, root, schema string) string {
	t.Helper()
	r := mustParse(t, root).(*jsonx.Object)
	v, err := NewSynthesizer(r).Value(mustParse(t, schema))
	if err != nil {
		t.Fatal(err)
	}
	return string(jsonx.Marshal(v, ""))
}

func TestSynthesizer(t *testing.T) {
	root := `{
	  "components": {
	    "schemas": {
	      "User": {"type": "object", "properties": {
	        "id": {"type": "string", "format": "uuid"},
	        "email": {"type": "string", "format": "email"},
	        "password": {"type": "string", "writeOnly": true},
	        "manager": {"$ref": "#/components/schemas/User"},
	        "reports": {"type": "array", "items": {"$ref": "#/components/schemas/User"}}
	      }},
	      "Node": {"type": "object", "properties": {"value": {"type": "integer"}, "next": {"$ref": "#/components/schemas/Node"}}},
	      "A~B/C": {"type": "boolean"},
	      "Named": {"type": "object", "properties": {"name": {"type": "string"}}},
	      "Aged": {"type": "object", "properties": {"age": {"type": "integer", "minimum": 18}}}
	    }
	  }
	}`
	tests := []struct {
		name, schema, want string
	}{
		{"string", `{"type": "string"}`, `"string"`},
		{"date-time", `{"type": "string", "format": "date-time"}`, `"2026-01-15T09:30:00Z"`},
		{"date", `{"type": "string", "format": "date"}`, `"2026-01-15"`},
		{"email", `{"type": "string", "format": "email"}`, `"user@example.com"`},
		{"uuid", `{"type": "string", "format": "uuid"}`, `"3fa85f64-5717-4562-b3fc-2c963f66afa6"`},
		{"uri", `{"type": "string", "format": "uri"}`, `"https://example.com"`},
		{"ipv4", `{"type": "string", "format": "ipv4"}`, `"192.0.2.1"`},
		{"minLength", `{"type": "string", "minLength": 10}`, `"stringxxxx"`},
		{"maxLength", `{"type": "string", "maxLength": 3}`, `"str"`},
		{"enum", `{"type": "string", "enum": ["active", "banned"]}`, `"active"`},
		{"example wins", `{"type": "integer", "example": 42}`, `42`},
		{"default", `{"type": "integer", "default": 7}`, `7`},
		{"const", `{"const": "fixed"}`, `"fixed"`},
		{"integer", `{"type": "integer"}`, `0`},
		{"integer minimum", `{"type": "integer", "minimum": 5}`, `5`},
		{"integer exclusiveMinimum 3.0", `{"type": "integer", "minimum": 5, "exclusiveMinimum": true}`, `6`},
		{"integer exclusiveMinimum 3.1", `{"type": "integer", "exclusiveMinimum": 5}`, `6`},
		{"negative maximum", `{"type": "number", "maximum": -2.5}`, `-2.5`},
		{"boolean", `{"type": "boolean"}`, `true`},
		{"3.1 type array", `{"type": ["null", "string"]}`, `"string"`},
		{"null", `{"type": "null"}`, `null`},
		{"array", `{"type": "array", "items": {"type": "integer"}}`, `[0]`},
		{"array minItems", `{"type": "array", "minItems": 3, "items": {"type": "boolean"}}`, `[true,true,true]`},
		{"array maxItems 0", `{"type": "array", "maxItems": 0, "items": {"type": "boolean"}}`, `[]`},
		{"inferred object", `{"properties": {"a": {"type": "string"}}}`, `{"a":"string"}`},
		{"additionalProperties", `{"type": "object", "additionalProperties": {"type": "integer"}}`, `{"additionalProp1":0}`},
		{"ref", `{"$ref": "#/components/schemas/Named"}`, `{"name":"string"}`},
		{"escaped pointer", `{"$ref": "#/components/schemas/A~0B~1C"}`, `true`},
		{"self cycle omitted, array cycle empty, writeOnly skipped", `{"$ref": "#/components/schemas/User"}`,
			`{"id":"3fa85f64-5717-4562-b3fc-2c963f66afa6","email":"user@example.com","reports":[]}`},
		{"linked list cycle", `{"$ref": "#/components/schemas/Node"}`, `{"value":0}`},
		{"allOf merge", `{"allOf": [{"$ref": "#/components/schemas/Named"}, {"$ref": "#/components/schemas/Aged"}, {"properties": {"x": {"type": "boolean"}}}]}`,
			`{"name":"string","age":18,"x":true}`},
		{"oneOf first", `{"oneOf": [{"type": "integer"}, {"type": "string"}]}`, `0`},
		{"empty schema", `{}`, `null`},
		{"true schema", `true`, `null`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := synth(t, root, tt.schema); got != tt.want {
				t.Errorf("got %s want %s", got, tt.want)
			}
		})
	}
}

func TestSynthesizerDeepCycleTerminates(t *testing.T) {
	root := `{"components": {"schemas": {
	  "A": {"type": "object", "properties": {"b": {"$ref": "#/components/schemas/B"}}},
	  "B": {"type": "object", "properties": {"c": {"$ref": "#/components/schemas/C"}, "list": {"type": "array", "items": {"$ref": "#/components/schemas/A"}}}},
	  "C": {"type": "object", "properties": {"a": {"$ref": "#/components/schemas/A"}, "name": {"type": "string"}}}
	}}}`
	got := synth(t, root, `{"$ref": "#/components/schemas/A"}`)
	if got != `{"b":{"c":{"name":"string"},"list":[]}}` {
		t.Fatalf("got %s", got)
	}
}

func TestSynthesizerRefErrors(t *testing.T) {
	root := mustParse(t, `{"components": {}}`).(*jsonx.Object)
	for schema, want := range map[string]string{
		`{"$ref": "#/components/schemas/Missing"}`: `"schemas" not found`,
		`{"$ref": "other.json#/User"}`:             "external $ref",
	} {
		_, err := NewSynthesizer(root).Value(mustParse(t, schema))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", schema, err, want)
		}
	}
}

const petstore = `{
  "openapi": "3.0.3",
  "info": {"title": "Pets", "version": "1"},
  "servers": [{"url": "https://api.example.com/v1"}],
  "paths": {
    "/": {"get": {"responses": {"200": {"description": "root", "content": {"text/plain": {"example": "hello"}}}}}},
    "/pets": {
      "get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"schema": {"type": "array", "items": {"$ref": "#/components/schemas/Pet"}}}}}}},
      "post": {"responses": {
        "201": {"description": "created", "content": {"application/json": {"example": {"id": 99, "name": "new"}}}},
        "400": {"$ref": "#/components/responses/BadRequest"}
      }}
    },
    "/pets/mine": {"get": {"responses": {"200": {"description": "mine", "content": {"application/json": {"example": {"mine": true}}}}}}},
    "/pets/{petId}": {
      "get": {"responses": {
        "200": {"description": "ok", "content": {"application/json": {"examples": {
          "cat": {"$ref": "#/components/examples/Cat"},
          "dog": {"value": {"id": 2, "name": "Rex", "kind": "dog"}}
        }}}},
        "404": {"description": "missing", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Error"}}}}
      }},
      "delete": {"responses": {"204": {"description": "gone"}}}
    },
    "/files/{name}.{ext}": {"get": {"responses": {"default": {"description": "file", "content": {"application/problem+json": {"schema": {"type": "object", "properties": {"ok": {"type": "boolean"}}}}}}}}},
    "/broken": {"get": {"responses": {"200": {"description": "x", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Nope"}}}}}}}
  },
  "components": {
    "schemas": {
      "Pet": {"type": "object", "required": ["id"], "properties": {
        "id": {"type": "integer", "format": "int64"},
        "name": {"type": "string"},
        "status": {"type": "string", "enum": ["available", "sold"]},
        "born": {"type": "string", "format": "date-time"}
      }},
      "Error": {"type": "object", "properties": {"code": {"type": "integer", "example": 404}, "message": {"type": "string"}}}
    },
    "responses": {"BadRequest": {"description": "bad", "content": {"application/json": {"example": {"error": "bad"}}}}},
    "examples": {"Cat": {"value": {"id": 1, "name": "Tom", "kind": "cat"}}}
  }
}`

func loadPetstore(t *testing.T) (*Spec, *httptest.Server) {
	t.Helper()
	spec, err := Load(mustParse(t, petstore))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(spec)
	t.Cleanup(srv.Close)
	return spec, srv
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]string{
		`{"swagger": "2.0"}`: "Swagger 2.0 documents are not supported",
		`{"openapi": "2.1"}`: "unsupported openapi version",
		`{"openapi": 3}`:     "unsupported openapi version",
		`[]`:                 "must be a JSON object",
	}
	for in, want := range tests {
		_, err := Load(mustParse(t, in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v want %q", in, err, want)
		}
	}
	if _, err := Load(mustParse(t, `{"openapi": "3.1.0"}`)); err != nil {
		t.Errorf("spec without paths should load: %v", err)
	}
}

func TestRoutesAndBasePath(t *testing.T) {
	spec, _ := loadPetstore(t)
	var got []string
	for _, r := range spec.Routes() {
		got = append(got, r.Method+" "+r.Path)
	}
	want := "GET /,GET /pets,POST /pets,GET /pets/mine,GET /pets/{petId},DELETE /pets/{petId},GET /files/{name}.{ext},GET /broken"
	if strings.Join(got, ",") != want {
		t.Fatalf("routes: %s", strings.Join(got, ","))
	}
	if spec.BasePath != "/v1" || !spec.HasRoot() {
		t.Fatalf("basePath=%q hasRoot=%v", spec.BasePath, spec.HasRoot())
	}
}

func TestServe(t *testing.T) {
	_, srv := loadPetstore(t)
	tests := []struct {
		method, path, prefer, body string
		status                     int
		ctype, want                string
	}{
		{"GET", "/pets", "", "", 200, "application/json; charset=utf-8",
			`[{"id":0,"name":"string","status":"available","born":"2026-01-15T09:30:00Z"}]`},
		{"GET", "/v1/pets", "", "", 200, "application/json; charset=utf-8",
			`[{"id":0,"name":"string","status":"available","born":"2026-01-15T09:30:00Z"}]`},
		{"GET", "/pets/mine", "", "", 200, "application/json; charset=utf-8", `{"mine":true}`},
		{"GET", "/pets/123", "", "", 200, "application/json; charset=utf-8", `{"id":1,"name":"Tom","kind":"cat"}`},
		{"GET", "/pets/123", "example=dog", "", 200, "application/json; charset=utf-8", `{"id":2,"name":"Rex","kind":"dog"}`},
		{"GET", "/pets/123", "code=404", "", 404, "application/json; charset=utf-8", `{"code":404,"message":"string"}`},
		{"POST", "/pets", "", `{"name": "x"}`, 201, "application/json; charset=utf-8", `{"id":99,"name":"new"}`},
		{"POST", "/pets", "code=400", "", 400, "application/json; charset=utf-8", `{"error":"bad"}`},
		{"DELETE", "/pets/1", "", "", 204, "", ``},
		{"GET", "/files/report.pdf", "", "", 200, "application/problem+json; charset=utf-8", `{"ok":true}`},
		{"GET", "/", "", "", 200, "text/plain", `hello`},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path+" "+tt.prefer, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			if tt.body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			if tt.prefer != "" {
				req.Header.Set("Prefer", tt.prefer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.status {
				t.Fatalf("status %d want %d: %s", resp.StatusCode, tt.status, b)
			}
			if ct := resp.Header.Get("Content-Type"); tt.ctype != "" && ct != tt.ctype {
				t.Errorf("content type %q want %q", ct, tt.ctype)
			}
			got := string(b)
			if strings.Contains(tt.ctype, "json") {
				v, err := jsonx.Parse(b)
				if err != nil {
					t.Fatalf("not JSON: %s", b)
				}
				got = string(jsonx.Marshal(v, ""))
			}
			if got != tt.want {
				t.Errorf("body %s want %s", got, tt.want)
			}
		})
	}
}

func TestServeErrors(t *testing.T) {
	_, srv := loadPetstore(t)
	tests := []struct {
		method, path, prefer, ctype, body string
		status                            int
		contains                          string
	}{
		{"GET", "/cats", "", "", "", 404, "no route for GET /cats"},
		{"GET", "/pets/1/extra", "", "", "", 404, "no route"},
		{"PUT", "/pets/1", "", "", "", 405, "allowed: GET, DELETE"},
		{"GET", "/pets/1", "code=500", "", "", 400, "only defines responses 200, 404"},
		{"GET", "/pets/1", "example=bird", "", "", 400, "available examples are cat, dog"},
		{"POST", "/pets", "", "application/json", `{"name": }`, 400, "invalid JSON body"},
		{"GET", "/broken", "", "", "", 500, "cannot resolve $ref"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, srv.URL+tt.path, strings.NewReader(tt.body))
			if tt.ctype != "" {
				req.Header.Set("Content-Type", tt.ctype)
			}
			if tt.prefer != "" {
				req.Header.Set("Prefer", tt.prefer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tt.status || !strings.Contains(string(b), tt.contains) {
				t.Fatalf("got %d %s, want %d containing %q", resp.StatusCode, b, tt.status, tt.contains)
			}
		})
	}
}

func TestHeadFallsBackToGet(t *testing.T) {
	_, srv := loadPetstore(t)
	resp, err := http.Head(srv.URL + "/pets")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("HEAD status %d", resp.StatusCode)
	}
}

func TestPickResponse(t *testing.T) {
	tests := []struct {
		keys []string
		want string
	}{
		{[]string{"404", "201", "200"}, "200"},
		{[]string{"default", "2XX"}, "2XX"},
		{[]string{"400", "default"}, "default"},
		{[]string{"400", "500"}, "400"},
	}
	for _, tt := range tests {
		if got := pickResponse(tt.keys); got != tt.want {
			t.Errorf("%v: got %s want %s", tt.keys, got, tt.want)
		}
	}
	if statusFor("2XX") != 200 || statusFor("default") != 200 || statusFor("418") != 418 {
		t.Error("statusFor wrong")
	}
}
