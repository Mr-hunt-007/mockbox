// Package mcptools exposes mockbox to AI agents as MCP tools. Every tool
// loads the file fresh into memory and answers through the same code the
// HTTP server uses. No tool opens a network listener or writes to a file.
package mcptools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Mr-hunt-007/mockbox/internal/app"
	"github.com/Mr-hunt-007/mockbox/internal/db"
	"github.com/Mr-hunt-007/mockbox/internal/httpx"
	"github.com/Mr-hunt-007/mockbox/internal/mcp"
	"github.com/Mr-hunt-007/mockbox/internal/rewrite"
)

// Defaults for output caps.
const (
	DefaultMaxBodyBytes = 20000
	DefaultMaxRoutes    = 200
)

// virtualHost is the Host header requests are made with, so Link headers
// look like the ones the default server (127.0.0.1:3000) sends.
const virtualHost = "127.0.0.1:3000"

// Options are the server-wide defaults taken from the command line.
type Options struct {
	File             string // default for the file argument ("" means required)
	Routes           string // default for the routes argument
	AllowDestructive bool   // mockbox has no destructive tools; kept for the flag
}

// Serve runs the MCP server for `mockbox --mcp`.
func Serve(ctx context.Context, cfg *app.Config, stdin io.Reader, stdout io.Writer) error {
	s := NewServer(Options{File: cfg.File, Routes: cfg.Routes, AllowDestructive: cfg.AllowDestructive})
	return s.Serve(ctx, stdin, stdout)
}

// NewServer builds the mockbox MCP server.
func NewServer(o Options) *mcp.Server {
	fileDesc := "Path to a JSON database file or an OpenAPI 3 JSON spec, absolute or relative to the server's working directory."
	if o.File != "" {
		fileDesc += fmt.Sprintf(" Defaults to %s (given on the command line).", o.File)
	} else {
		fileDesc += " Required: the server was started without a default file."
	}
	routesDesc := "Optional --routes rewrite file (JSON object such as {\"/api/*\": \"/$1\"}), absolute or relative to the working directory."
	if o.Routes != "" {
		routesDesc += fmt.Sprintf(" Defaults to %s.", o.Routes)
	}
	var required []string
	if o.File == "" {
		required = []string{"file"}
	}

	return &mcp.Server{
		Name:    "mockbox",
		Title:   "mockbox",
		Version: app.Version,
		Instructions: "mockbox turns a JSON database file or an OpenAPI 3 JSON spec into a REST API. " +
			"Use these tools to see which routes a mock file serves and exactly what status, headers and body a frontend request would get, without starting the server. " +
			"Every call reads the file fresh into memory; nothing is written back and no port is opened. " +
			"To actually serve the API to a browser or app, run `mockbox <file>` in a terminal.",
		Tools: []mcp.Tool{
			{
				Name:  "mockbox_routes",
				Title: "List mock routes",
				Description: "Lists the routes `mockbox <file>` would serve. Use it first, to learn what a db.json or OpenAPI file exposes before calling mockbox_request.\n\n" +
					"Output: `mode` is \"database\" or \"openapi\". `routes` is the startup route table (method, path; `:id` and `{param}` are placeholders). " +
					"In database mode, `collections` (name, item count) are top-level arrays with GET/POST /name and GET/PUT/PATCH/DELETE /name/:id; " +
					"`singular` are top-level objects with GET/PUT/PATCH /name; `relations` are detected parent/child links (child items with a foreign key such as userId for users), " +
					"usable as GET /parent/:id/child, ?_embed=child on the parent and ?_expand=<expand> on the child. `skipped` lists top-level keys that cannot be served. " +
					"In OpenAPI mode, `base_path` is the servers[0].url path under which routes are also served. `rewrites` are the --routes rules.\n\n" +
					"Limits: relations are guessed from names (users -> userId), so irregular plurals are missed. The route list is capped by max_routes.",
				InputSchema: mcp.Object(map[string]any{
					"file":       mcp.String(fileDesc),
					"routes":     mcp.String(routesDesc),
					"max_routes": mcp.Integer(fmt.Sprintf("Maximum routes to return (default %d). The result says when the list was truncated.", DefaultMaxRoutes)),
				}, required...),
				Annotations: mcp.ReadOnly("List mock routes"),
				Handler:     o.routes,
			},
			{
				Name:  "mockbox_request",
				Title: "Run a request against the mock",
				Description: "Runs one HTTP request against a fresh in-memory copy of the file and returns the status, headers and body mockbox would send. " +
					"Use it to check exactly what a frontend will get: filters (?role=admin, ?age_gte=18, ?q=text), sorting (?_sort=-age), pagination (?_page=2&_limit=10 with X-Total-Count and Link headers), ?_embed and ?_expand, error bodies, and in OpenAPI mode `Prefer: code=404` or `Prefer: example=name` headers.\n\n" +
					"Every call starts from the file's current contents: a POST, PUT, PATCH or DELETE shows its response but is not remembered by the next call, and the file is never modified.\n\n" +
					"Output: `status`; `headers` set by mockbox (the running server also adds Date and Content-Length); `body` as JSON when the response is JSON, otherwise `body_text`; " +
					"`body_bytes` is the full size; when it exceeds max_body_bytes, `truncated` is true and `body_text` holds the first bytes. `rewritten_to` appears when a routes rule matched. " +
					"Errors from the mock API are normal results with a 4xx status and a body like {\"error\", \"status\", \"message\"}; a tool error means the file or arguments are unusable.",
				InputSchema: mcp.Object(map[string]any{
					"file":   mcp.String(fileDesc),
					"method": mcp.Enum("HTTP method.", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"),
					"path":   mcp.String("Request path starting with /, with an optional query string, for example /posts?userId=1&_sort=-id or /users/1."),
					"body": map[string]any{
						"type":                 "object",
						"additionalProperties": true,
						"description":          "Optional JSON object request body, sent as-is with Content-Type: application/json (mockbox collections and singular resources only accept objects). Omit it for no body.",
					},
					"headers": map[string]any{
						"type":                 "object",
						"additionalProperties": map[string]any{"type": "string"},
						"description":          "Optional request headers, for example {\"Accept\": \"application/json\"} or {\"Prefer\": \"code=404\"}.",
					},
					"routes":         mcp.String(routesDesc),
					"max_body_bytes": mcp.Integer(fmt.Sprintf("Maximum response body bytes to return (default %d, up to %d). Larger bodies are truncated and marked.", DefaultMaxBodyBytes, httpx.MaxBodyBytes)),
				}, append(required, "method", "path")...),
				Annotations: mcp.ReadOnly("Run a request against the mock"),
				Handler:     o.request,
			},
			{
				Name:  "mockbox_example",
				Title: "Get an OpenAPI example response",
				Description: "For an OpenAPI 3 JSON spec, returns the response body mockbox serves for one operation, and where it came from. " +
					"Use it to see the example or synthesized payload for a given status code (for instance the 404 body) and which responses and named examples the operation defines.\n\n" +
					"Output: `operation` is the matched spec path; `status` and `response` (the responses key used); `responses` lists every defined key; `examples` lists named examples; " +
					"`source` is \"example\", \"examples/<name>\", \"schema\" (synthesized: deterministic placeholder values, not real data) or \"none\"; `body` (JSON) or `body_text`, capped by max_body_bytes.\n\n" +
					"Limits: only for OpenAPI files (use mockbox_request for a JSON database). Without `status`, the lowest 2xx response is used, then 2XX, then default.",
				InputSchema: mcp.Object(map[string]any{
					"file":           mcp.String(strings.Replace(fileDesc, "a JSON database file or an OpenAPI 3 JSON spec", "an OpenAPI 3 JSON spec", 1)),
					"method":         mcp.Enum("HTTP method of the operation.", "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "TRACE"),
					"path":           mcp.String("A concrete path such as /orders/42 or the spec path such as /orders/{orderId}. The servers[0].url base path prefix is also accepted."),
					"status":         mcp.String("Optional responses key to render, for example \"404\", \"2XX\" or \"default\"."),
					"example":        mcp.String("Optional name of an entry under the media type's `examples`."),
					"max_body_bytes": mcp.Integer(fmt.Sprintf("Maximum body bytes to return (default %d, up to %d).", DefaultMaxBodyBytes, httpx.MaxBodyBytes)),
				}, append(required, "method", "path")...),
				Annotations: mcp.ReadOnly("Get an OpenAPI example response"),
				Handler:     o.example,
			},
		},
	}
}

func (o Options) open(file, routes string) (*app.App, []string, string, error) {
	if file == "" {
		file = o.File
	}
	if routes == "" {
		routes = o.Routes
	}
	if file == "" {
		return nil, nil, "", errors.New("file is required: pass the path to a JSON database or OpenAPI 3 JSON spec")
	}
	a, warnings, err := app.Open(file, routes)
	if err != nil {
		return nil, nil, "", err
	}
	return a, warnings, file, nil
}

func capArg(v, def, max int, name string) (int, error) {
	switch {
	case v < 0:
		return 0, fmt.Errorf("%s must be 0 or more, got %d", name, v)
	case v == 0:
		return def, nil
	case v > max:
		return 0, fmt.Errorf("%s must be at most %d, got %d", name, max, v)
	}
	return v, nil
}

// RouteJSON matches the route objects in the --json start event.
type RouteJSON struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// RewriteJSON matches the rewrite objects in the --json start event.
type RewriteJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RoutesResult is the output of mockbox_routes. file, mode, routes and
// rewrites use the same names and shapes as the --json start event.
type RoutesResult struct {
	Version     string          `json:"version"`
	File        string          `json:"file"`
	Mode        string          `json:"mode"`
	Routes      []RouteJSON     `json:"routes"`
	TotalRoutes int             `json:"total_routes"`
	Truncated   bool            `json:"truncated,omitempty"`
	Note        string          `json:"note,omitempty"`
	Rewrites    []RewriteJSON   `json:"rewrites"`
	BasePath    string          `json:"base_path,omitempty"`
	Collections []db.Collection `json:"collections,omitempty"`
	Singular    []string        `json:"singular,omitempty"`
	Relations   []db.Relation   `json:"relations,omitempty"`
	Skipped     []string        `json:"skipped,omitempty"`
}

func (o Options) routes(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var in struct {
		File      string `json:"file"`
		Routes    string `json:"routes"`
		MaxRoutes int    `json:"max_routes"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return mcp.Result{}, err
	}
	max, err := capArg(in.MaxRoutes, DefaultMaxRoutes, 100000, "max_routes")
	if err != nil {
		return mcp.Result{}, err
	}
	a, warnings, file, err := o.open(in.File, in.Routes)
	if err != nil {
		return mcp.Result{}, err
	}
	out := RoutesResult{Version: app.Version, File: file, Mode: a.Mode(), Routes: []RouteJSON{}, Rewrites: []RewriteJSON{}, Skipped: warnings}
	all := a.Routes()
	out.TotalRoutes = len(all)
	for i, r := range all {
		if i == max {
			out.Truncated = true
			out.Note = fmt.Sprintf("showing %d of %d routes; raise max_routes to see more", max, len(all))
			break
		}
		out.Routes = append(out.Routes, RouteJSON{r.Method, r.Path})
	}
	for _, r := range a.Rewrites() {
		out.Rewrites = append(out.Rewrites, RewriteJSON{r.From, r.To})
	}
	if d := a.DB(); d != nil {
		out.Collections, out.Singular = d.Resources()
		out.Relations = d.Relations()
		if out.Collections == nil {
			out.Collections = []db.Collection{}
		}
	}
	if s := a.Spec(); s != nil {
		out.BasePath = s.BasePath
	}
	return mcp.JSONResult(out)
}

// RequestResult is the output of mockbox_request.
type RequestResult struct {
	File        string            `json:"file"`
	Mode        string            `json:"mode"`
	Method      string            `json:"method"`
	Path        string            `json:"path"`
	RewrittenTo string            `json:"rewritten_to,omitempty"`
	Status      int               `json:"status"`
	Headers     map[string]string `json:"headers"`
	Body        json.RawMessage   `json:"body,omitempty"`
	BodyText    *string           `json:"body_text,omitempty"`
	BodyBytes   int               `json:"body_bytes"`
	Truncated   bool              `json:"truncated,omitempty"`
	Note        string            `json:"note,omitempty"`
}

func (o Options) request(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var in struct {
		File         string            `json:"file"`
		Method       string            `json:"method"`
		Path         string            `json:"path"`
		Body         json.RawMessage   `json:"body"`
		Headers      map[string]string `json:"headers"`
		Routes       string            `json:"routes"`
		MaxBodyBytes int               `json:"max_body_bytes"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return mcp.Result{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	switch method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	case "":
		return mcp.Result{}, errors.New("method is required, for example GET")
	default:
		return mcp.Result{}, fmt.Errorf("unsupported method %q: use GET, HEAD, POST, PUT, PATCH, DELETE or OPTIONS", in.Method)
	}
	if err := checkPath(in.Path); err != nil {
		return mcp.Result{}, err
	}
	max, err := capArg(in.MaxBodyBytes, DefaultMaxBodyBytes, httpx.MaxBodyBytes, "max_body_bytes")
	if err != nil {
		return mcp.Result{}, err
	}
	a, _, file, err := o.open(in.File, in.Routes)
	if err != nil {
		return mcp.Result{}, err
	}

	var body io.Reader
	hasBody := len(bytes.TrimSpace(in.Body)) > 0 && string(bytes.TrimSpace(in.Body)) != "null"
	if hasBody {
		body = bytes.NewReader(in.Body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+virtualHost+in.Path, body)
	if err != nil {
		return mcp.Result{}, fmt.Errorf("invalid path %q: %v", in.Path, err)
	}
	req.RemoteAddr = "127.0.0.1:0"
	if req.Body == nil {
		req.Body = http.NoBody // what a real server hands to handlers
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range in.Headers {
		if strings.EqualFold(k, "Host") {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}

	rewritten := ""
	if p, q, ok := rewrite.Rewrite(a.Rewrites(), req.URL.Path, req.URL.RawQuery); ok {
		rewritten = p
		if q != "" {
			rewritten += "?" + q
		}
	}
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)

	out := RequestResult{File: file, Mode: a.Mode(), Method: method, Path: in.Path, RewrittenTo: rewritten, Status: res.StatusCode, Headers: map[string]string{}}
	for k, vs := range res.Header {
		out.Headers[k] = strings.Join(vs, ", ")
	}
	setBody(&out.Body, &out.BodyText, &out.BodyBytes, &out.Truncated, &out.Note, raw, res.Header.Get("Content-Type"), max)
	return mcp.JSONResult(out)
}

func checkPath(p string) error {
	if p == "" {
		return errors.New("path is required, for example /users?_page=1")
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must start with /, got %q (pass only the path and query, not a full URL)", p)
	}
	if _, err := url.ParseRequestURI(p); err != nil {
		return fmt.Errorf("invalid path %q: %v", p, err)
	}
	return nil
}

// setBody fills the body fields: JSON bodies inline, anything else or a
// truncated body as text.
func setBody(jsonBody *json.RawMessage, text **string, n *int, truncated *bool, note *string, raw []byte, ctype string, max int) {
	*n = len(raw)
	if len(raw) == 0 {
		return
	}
	if len(raw) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(raw[cut]) {
			cut--
		}
		s := string(raw[:cut])
		*text = &s
		*truncated = true
		*note = fmt.Sprintf("body is %d bytes; showing the first %d as text. Raise max_body_bytes, or narrow the request with filters or _page and _limit", len(raw), cut)
		return
	}
	if strings.Contains(strings.ToLower(ctype), "json") && json.Valid(raw) {
		*jsonBody = json.RawMessage(bytes.TrimSpace(raw))
		return
	}
	s := string(raw)
	*text = &s
}

// statusArg is a responses key. Clients often send 404 as a number, so both
// "404" and 404 are accepted.
type statusArg string

func (s *statusArg) UnmarshalJSON(b []byte) error {
	var str string
	if err := json.Unmarshal(b, &str); err == nil {
		*s = statusArg(str)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("status must be a responses key such as \"404\" or \"default\"")
	}
	*s = statusArg(n.String())
	return nil
}

// ExampleResult is the output of mockbox_example.
type ExampleResult struct {
	File        string          `json:"file"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Operation   string          `json:"operation"`
	Status      int             `json:"status"`
	Response    string          `json:"response,omitempty"`
	Responses   []string        `json:"responses"`
	ContentType string          `json:"content_type,omitempty"`
	Examples    []string        `json:"examples,omitempty"`
	Source      string          `json:"source"`
	Body        json.RawMessage `json:"body,omitempty"`
	BodyText    *string         `json:"body_text,omitempty"`
	BodyBytes   int             `json:"body_bytes"`
	Truncated   bool            `json:"truncated,omitempty"`
	Note        string          `json:"note,omitempty"`
}

func (o Options) example(ctx context.Context, args json.RawMessage) (mcp.Result, error) {
	var in struct {
		File         string    `json:"file"`
		Method       string    `json:"method"`
		Path         string    `json:"path"`
		Status       statusArg `json:"status"`
		Example      string    `json:"example"`
		MaxBodyBytes int       `json:"max_body_bytes"`
	}
	if err := mcp.Decode(args, &in); err != nil {
		return mcp.Result{}, err
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if method == "" {
		return mcp.Result{}, errors.New("method is required, for example GET")
	}
	if err := checkPath(in.Path); err != nil {
		return mcp.Result{}, err
	}
	if strings.ContainsAny(string(in.Status), ",;=") || strings.ContainsAny(in.Example, ",;") {
		return mcp.Result{}, errors.New("status and example must be a single responses key and a single example name")
	}
	max, err := capArg(in.MaxBodyBytes, DefaultMaxBodyBytes, httpx.MaxBodyBytes, "max_body_bytes")
	if err != nil {
		return mcp.Result{}, err
	}
	a, _, file, err := o.open(in.File, "")
	if err != nil {
		return mcp.Result{}, err
	}
	spec := a.Spec()
	if spec == nil {
		return mcp.Result{}, fmt.Errorf("%s is a JSON database, not an OpenAPI spec; use mockbox_request to see its responses", filepath.Base(file))
	}
	u, _ := url.ParseRequestURI(in.Path)
	op, allow := spec.Find(method, u.Path)
	if op == nil {
		if len(allow) > 0 {
			sort.Strings(allow)
			return mcp.Result{}, fmt.Errorf("%s is not defined for %s; defined methods: %s", method, u.Path, strings.Join(allow, ", "))
		}
		return mcp.Result{}, fmt.Errorf("no operation in %s matches %s %s; call mockbox_routes to list them", filepath.Base(file), method, u.Path)
	}
	var prefer []string
	if in.Status != "" {
		prefer = append(prefer, "code="+string(in.Status))
	}
	if in.Example != "" {
		prefer = append(prefer, "example="+in.Example)
	}
	r, err := spec.Render(op, strings.Join(prefer, ", "))
	if err != nil {
		msg := err.Error()
		msg = strings.Replace(msg, "Prefer: code=", "status ", 1)
		msg = strings.Replace(msg, "Prefer: example=", "example ", 1)
		return mcp.Result{}, errors.New(msg)
	}
	out := ExampleResult{File: file, Method: method, Path: in.Path, Operation: op.Path, Status: r.Status, Response: r.Response,
		Responses: r.Responses, ContentType: r.ContentType, Examples: r.Examples, Source: r.Source}
	if out.Responses == nil {
		out.Responses = []string{}
	}
	setBody(&out.Body, &out.BodyText, &out.BodyBytes, &out.Truncated, &out.Note, r.Body, r.ContentType, max)
	return mcp.JSONResult(out)
}
