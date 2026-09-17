package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mr-hunt-007/mockbox/internal/httpx"
)

// syncBuffer is a goroutine-safe bytes.Buffer.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const sampleDB = `{
  "users": [
    {"id": 1, "name": "Ada"},
    {"id": 2, "name": "Alan"},
    {"id": 3, "name": "Grace"}
  ],
  "profile": {"name": "demo"}
}
`

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func newApp(t *testing.T, cfg *Config) (*App, *syncBuffer, *syncBuffer) {
	t.Helper()
	if cfg.Host == "" {
		cfg.Host = "127.0.0.1"
	}
	out, errOut := &syncBuffer{}, &syncBuffer{}
	a, err := New(cfg, out, errOut, false)
	if err != nil {
		t.Fatal(err)
	}
	return a, out, errOut
}

func get(t *testing.T, url string, header ...string) (*http.Response, string) {
	t.Helper()
	return request(t, "GET", url, "", header...)
}

func request(t *testing.T, method, url, body string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestParseArgs(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
		check   func(*Config) bool
	}{
		{[]string{"db.json"}, "", func(c *Config) bool {
			return c.File == "db.json" && c.Port == 3000 && c.Host == "127.0.0.1" && !c.Persist
		}},
		{[]string{"db.json", "--port", "4000", "--watch"}, "", func(c *Config) bool { return c.Port == 4000 && c.Watch }},
		{[]string{"--cors", "db.json", "--delay=250", "--readonly"}, "", func(c *Config) bool {
			return c.CORS && c.Delay == 250 && c.Readonly && c.File == "db.json"
		}},
		{[]string{"--host", "0.0.0.0", "db.json", "--routes", "r.json", "--quiet", "--json", "--no-color"}, "", func(c *Config) bool {
			return c.Host == "0.0.0.0" && c.Routes == "r.json" && c.Quiet && c.JSON && c.NoColor
		}},
		{[]string{"--version"}, "", func(c *Config) bool { return c.Version }},
		{[]string{}, "missing file argument", nil},
		{[]string{"a.json", "b.json"}, "expected one file argument, got 2", nil},
		{[]string{"db.json", "--port", "70000"}, "--port must be between", nil},
		{[]string{"db.json", "--port", "abc"}, "invalid value", nil},
		{[]string{"db.json", "--delay", "-1"}, "--delay must be 0 or more", nil},
		{[]string{"db.json", "--nope"}, "not defined", nil},
		{[]string{"db.json", "--persist", "--readonly"}, "cannot be used together", nil},
		{[]string{"db.json", "--host", ""}, "--host must not be empty", nil},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			cfg, err := ParseArgs(tt.args, io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("got %v want error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(cfg) {
				t.Fatalf("unexpected config %+v", cfg)
			}
		})
	}
	for _, h := range []string{"-h", "--help"} {
		if _, err := ParseArgs([]string{h}, io.Discard); err != errHelp {
			t.Errorf("%s: %v", h, err)
		}
	}
}

func TestIndexContentNegotiation(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB)})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/")
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, `<a href="/users">/users</a>`) {
		t.Fatalf("HTML index wrong: %s\n%s", resp.Header.Get("Content-Type"), body)
	}
	if strings.Contains(body, `href="/users/:id"`) {
		t.Fatal("parameterised routes must not be links")
	}
	resp, body = get(t, srv.URL+"/", "Accept", "application/json")
	var idx struct {
		Name    string `json:"name"`
		Mode    string `json:"mode"`
		Version string `json:"version"`
		Routes  []struct{ Method, Path string }
	}
	if err := json.Unmarshal([]byte(body), &idx); err != nil {
		t.Fatalf("JSON index: %v\n%s", err, body)
	}
	if idx.Name != "mockbox" || idx.Mode != "database" || idx.Version != Version || len(idx.Routes) != 9 {
		t.Fatalf("JSON index content: %+v", idx)
	}
	resp, _ = request(t, "POST", srv.URL+"/", "{}")
	if resp.StatusCode != 405 {
		t.Fatalf("POST / status %d", resp.StatusCode)
	}
}

func TestPrefersJSON(t *testing.T) {
	tests := map[string]bool{
		"":                                    false,
		"*/*":                                 false,
		"application/json":                    true,
		"text/html,application/json":          false,
		"text/html;q=0.5,application/json":    true,
		"application/json;q=0.9,text/html":    false,
		"application/json, */*;q=0.1":         true,
		"text/html,application/xhtml+xml,*/*": false,
	}
	for accept, want := range tests {
		if got := httpx.PrefersJSON(accept); got != want {
			t.Errorf("%q: got %v want %v", accept, got, want)
		}
	}
}

func TestReadonly(t *testing.T) {
	dir := t.TempDir()
	a, out, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Readonly: true})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		resp, body := request(t, m, srv.URL+"/users/1", `{"name": "x"}`)
		if resp.StatusCode != 405 || !strings.Contains(body, "read-only") {
			t.Errorf("%s: %d %s", m, resp.StatusCode, body)
		}
	}
	if resp, _ := get(t, srv.URL+"/users/1"); resp.StatusCode != 200 {
		t.Fatalf("GET under readonly: %d", resp.StatusCode)
	}
	a.Banner(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3000})
	banner := out.String()[strings.Index(out.String(), "mockbox "):]
	if strings.Contains(banner, "POST") || !strings.Contains(banner, "Options: readonly") {
		t.Fatalf("readonly banner:\n%s", out.String())
	}
}

func TestCORS(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), CORS: true})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, _ := request(t, "OPTIONS", srv.URL+"/users", "",
		"Origin", "http://localhost:5173",
		"Access-Control-Request-Method", "POST",
		"Access-Control-Request-Headers", "content-type, x-token")
	h := resp.Header
	if resp.StatusCode != 204 ||
		h.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(h.Get("Access-Control-Allow-Methods"), "PATCH") ||
		h.Get("Access-Control-Allow-Headers") != "content-type, x-token" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, h)
	}
	resp, _ = get(t, srv.URL+"/users?_page=1&_limit=1", "Origin", "http://localhost:5173")
	if resp.Header.Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "X-Total-Count") {
		t.Fatalf("simple request headers: %v", resp.Header)
	}
	resp, _ = get(t, srv.URL+"/users")
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("no-origin request: %v", resp.Header)
	}

	b, _, _ := newApp(t, &Config{File: filepath.Join(dir, "db.json")})
	plain := httptest.NewServer(b.Handler())
	defer plain.Close()
	resp, _ = get(t, plain.URL+"/users", "Origin", "http://x")
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS headers sent without --cors")
	}
}

func TestDelay(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Delay: 150})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	start := time.Now()
	resp, _ := get(t, srv.URL+"/users")
	if el := time.Since(start); el < 150*time.Millisecond || resp.StatusCode != 200 {
		t.Fatalf("delay not applied: %v, status %d", el, resp.StatusCode)
	}
}

func TestRoutesRewriteAndLogging(t *testing.T) {
	dir := t.TempDir()
	routes := writeFile(t, dir, "routes.json", `{"/api/*": "/$1"}`)
	a, out, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Routes: routes})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()

	resp, body := get(t, srv.URL+"/api/users?_page=1&_limit=2")
	if resp.StatusCode != 200 || !strings.Contains(body, "Alan") || strings.Contains(body, "Grace") {
		t.Fatalf("rewrite: %d %s", resp.StatusCode, body)
	}
	if link := resp.Header.Get("Link"); !strings.Contains(link, "/api/users?_limit=2&_page=2") {
		t.Fatalf("Link header should use the client-visible path: %s", link)
	}
	resp, _ = request(t, "POST", srv.URL+"/api/users", `{"name": "Linus"}`)
	if resp.Header.Get("Location") != "/api/users/4" {
		t.Fatalf("Location %q", resp.Header.Get("Location"))
	}
	log := out.String()
	if !strings.Contains(log, "GET    /api/users?_page=1&_limit=2 -> /users?_page=1&_limit=2 200") {
		t.Fatalf("log line missing rewrite:\n%s", log)
	}
}

func TestJSONLog(t *testing.T) {
	dir := t.TempDir()
	a, out, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), JSON: true})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	get(t, srv.URL+"/nope")
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &entry); err != nil {
		t.Fatalf("log is not one JSON line: %v\n%s", err, out.String())
	}
	if entry["event"] != "request" || entry["method"] != "GET" || entry["path"] != "/nope" || entry["status"] != float64(404) {
		t.Fatalf("log entry: %v", entry)
	}
	if _, ok := entry["duration_ms"].(float64); !ok {
		t.Fatalf("duration_ms missing: %v", entry)
	}
	if _, err := time.Parse(time.RFC3339Nano, entry["time"].(string)); err != nil {
		t.Fatalf("time: %v", err)
	}
}

func TestQuietSuppressesOutput(t *testing.T) {
	dir := t.TempDir()
	a, out, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Quiet: true})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	a.Banner(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3000})
	get(t, srv.URL+"/users")
	if out.String() != "" {
		t.Fatalf("quiet printed: %q", out.String())
	}
}

func TestBanner(t *testing.T) {
	dir := t.TempDir()
	a, out, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Watch: true, CORS: true, Delay: 500})
	a.Banner(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3000})
	want := []string{
		"  GET    /users\n  GET    /users/:id\n  POST   /users\n  PUT    /users/:id\n  PATCH  /users/:id\n  DELETE /users/:id\n  GET    /profile\n  PUT    /profile\n  PATCH  /profile\n",
		"Options: watch, cors, delay 500ms",
		"Listening on http://127.0.0.1:3000",
		"in memory, file is never modified",
	}
	for _, w := range want {
		if !strings.Contains(out.String(), w) {
			t.Fatalf("banner missing %q:\n%s", w, out.String())
		}
	}

	j, jout, _ := newApp(t, &Config{File: filepath.Join(dir, "db.json"), JSON: true})
	j.Banner(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3000})
	var start struct {
		Event, Mode, URL string
		Routes           []struct{ Method, Path string }
	}
	if err := json.Unmarshal([]byte(jout.String()), &start); err != nil || start.Event != "start" || start.URL != "http://127.0.0.1:3000" || len(start.Routes) != 9 {
		t.Fatalf("JSON banner: %v %s", err, jout.String())
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, file, content string
		cfg                 Config
		usage               bool
		contains            string
	}{
		{"yaml extension", "api.yaml", "openapi: 3.0.0\n", Config{}, false, "YAML is not supported"},
		{"yaml content in .json", "api2.json", "# spec\nopenapi: 3.0.0\npaths: {}\n", Config{}, false, "YAML is not supported"},
		{"invalid json", "bad.json", "{\n  \"users\": [\n    {\"id\": 1,}\n  ]\n}", Config{}, false, "bad.json is not valid JSON: invalid character '}' looking for beginning of object key string (line 3, column 14)"},
		{"array top level", "arr.json", "[]", Config{}, false, "top level must be a JSON object"},
		{"swagger", "sw.json", `{"swagger": "2.0"}`, Config{}, false, "Swagger 2.0"},
		{"persist openapi", "oa.json", `{"openapi": "3.0.0", "paths": {}}`, Config{Persist: true}, true, "--persist only works with a JSON database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.File = writeFile(t, dir, tt.file, tt.content)
			_, err := New(&cfg, io.Discard, io.Discard, false)
			if err == nil || !strings.Contains(err.Error(), tt.contains) {
				t.Fatalf("got %v want %q", err, tt.contains)
			}
			_, isUsage := err.(*UsageError)
			_, isInput := err.(*InputError)
			if isUsage != tt.usage || isInput == tt.usage {
				t.Fatalf("error type %T", err)
			}
		})
	}
	// The conversion hint must never redirect output onto the YAML file itself.
	_, err := New(&Config{File: writeFile(t, dir, "orders.yml", "openapi: 3.0.0\n")}, io.Discard, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "orders.yml > ") || strings.Contains(err.Error(), "> "+filepath.Join(dir, "orders.yml")) ||
		!strings.Contains(err.Error(), "yq -o=json "+filepath.Join(dir, "orders.yml")+" > "+filepath.Join(dir, "orders.json")) {
		t.Fatalf("YAML hint: %v", err)
	}
	_, err = New(&Config{File: filepath.Join(dir, "missing.json")}, io.Discard, io.Discard, false)
	if err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("missing file: %v", err)
	}
	_, err = New(&Config{File: writeFile(t, dir, "ok.json", sampleDB), Routes: writeFile(t, dir, "r.json", `{"/a": 1}`)}, io.Discard, io.Discard, false)
	if _, ok := err.(*InputError); !ok {
		t.Fatalf("bad routes file: %v", err)
	}
}

func TestWarningsForUnservableKeys(t *testing.T) {
	dir := t.TempDir()
	_, _, errOut := newApp(t, &Config{File: writeFile(t, dir, "db.json", `{"users": [], "count": 3}`)})
	if !strings.Contains(errOut.String(), `skipping key "count"`) {
		t.Fatalf("stderr: %q", errOut.String())
	}
}

func startServer(t *testing.T, a *App) (string, context.CancelFunc, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return "http://" + ln.Addr().String(), cancel, done
}

// bumpWrite writes content and pushes the mtime forward so the poller sees it
// even on filesystems with coarse timestamps.
func bumpWrite(t *testing.T, path, content string, step int) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(time.Duration(step) * time.Minute)
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatchReloadsAndSurvivesBrokenEdits(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "db.json", sampleDB)
	a, out, errOut := newApp(t, &Config{File: path, Watch: true})
	a.PollInterval = 20 * time.Millisecond
	base, _, _ := startServer(t, a)

	bumpWrite(t, path, `{"users": [{"id": 1, "name": "Edited"}]}`, 1)
	eventually(t, "reload", func() bool {
		_, body := get(t, base+"/users/1")
		return strings.Contains(body, "Edited")
	})
	if !strings.Contains(out.String(), "reloaded") {
		t.Fatalf("no reload message:\n%s", out.String())
	}

	bumpWrite(t, path, `{"users": [{"id": 1, "name": "Broken"`, 2)
	eventually(t, "reload failure message", func() bool {
		return strings.Contains(errOut.String(), "reload failed, still serving the last good data")
	})
	if !strings.Contains(errOut.String(), "line 1, column") {
		t.Fatalf("parse error should include position: %s", errOut.String())
	}
	if _, body := get(t, base+"/users/1"); !strings.Contains(body, "Edited") {
		t.Fatalf("broken edit replaced good data: %s", body)
	}

	// Switching the file to an OpenAPI spec swaps the backend.
	bumpWrite(t, path, `{"openapi": "3.0.0", "paths": {"/health": {"get": {"responses": {"200": {"description": "ok", "content": {"application/json": {"example": {"ok": true}}}}}}}}}`, 3)
	eventually(t, "switch to openapi", func() bool {
		resp, _ := get(t, base+"/health")
		return resp.StatusCode == 200
	})
	if resp, _ := get(t, base+"/users"); resp.StatusCode != 404 {
		t.Fatalf("old database routes still served: %d", resp.StatusCode)
	}
}

func TestWatchWithPersistDoesNotLoseWrites(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, dir, "db.json", sampleDB)
	a, _, errOut := newApp(t, &Config{File: path, Watch: true, Persist: true})
	a.PollInterval = 5 * time.Millisecond
	base, _, _ := startServer(t, a)
	for i := 0; i < 30; i++ {
		resp, body := request(t, "POST", base+"/users", `{"name": "n`+strconv.Itoa(i)+`"}`)
		if resp.StatusCode != 201 {
			t.Fatalf("POST %d: %d %s", i, resp.StatusCode, body)
		}
	}
	time.Sleep(50 * time.Millisecond)
	resp, _ := get(t, base+"/users")
	if resp.Header.Get("X-Total-Count") != "33" {
		t.Fatalf("writes lost: X-Total-Count=%s stderr=%s", resp.Header.Get("X-Total-Count"), errOut.String())
	}
	raw, _ := os.ReadFile(path)
	if c := strings.Count(string(raw), `"name"`); c != 34 { // 33 users plus profile.name
		t.Fatalf("file has %d name fields", c)
	}
}

func TestGracefulShutdownFinishesInFlightRequests(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Quiet: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx, ln) }()
	base := "http://" + ln.Addr().String()
	eventually(t, "server up", func() bool {
		resp, err := http.Get(base + "/users")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not shut down")
	}
	if _, err := http.Get(base + "/users"); err == nil {
		t.Fatal("server still accepting connections after shutdown")
	}
}

func TestListenPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	dir := t.TempDir()
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "db.json", sampleDB), Port: port})
	_, err = a.Listen()
	if err == nil {
		t.Fatal("expected port conflict")
	}
	if !strings.Contains(err.Error(), "port "+strconv.Itoa(port)+" is already in use") {
		t.Fatalf("message should name the port: %v", err)
	}
}

func TestRunExitCodes(t *testing.T) {
	dir := t.TempDir()
	good := writeFile(t, dir, "db.json", sampleDB)
	yaml := writeFile(t, dir, "spec.yml", "openapi: 3.0.0")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	tests := []struct {
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{[]string{"--help"}, ExitOK, "Usage:", ""},
		{[]string{"--version"}, ExitOK, "mockbox " + Version, ""},
		{[]string{}, ExitUsage, "", "missing file argument"},
		{[]string{good, "--bogus"}, ExitUsage, "", "flag provided but not defined"},
		{[]string{yaml}, ExitInput, "", "YAML is not supported"},
		{[]string{filepath.Join(dir, "nope.json")}, ExitInput, "", "cannot read"},
		{[]string{good, "--port", busy, "--quiet"}, ExitRuntime, "", "is already in use"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := Run(tt.args, &out, &errOut)
			if code != tt.code {
				t.Fatalf("exit %d want %d (stderr %s)", code, tt.code, errOut.String())
			}
			if !strings.Contains(out.String(), tt.stdout) || !strings.Contains(errOut.String(), tt.stderr) {
				t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
			}
		})
	}
}

func TestOpenAPIModeThroughApp(t *testing.T) {
	dir := t.TempDir()
	spec := `{"openapi": "3.1.0", "paths": {"/users/{id}": {"get": {"responses": {"200": {"description": "ok",
	  "content": {"application/json": {"schema": {"type": "object", "properties": {"id": {"type": "string", "format": "uuid"}, "created": {"type": "string", "format": "date-time"}}}}}}}}}}}`
	a, _, _ := newApp(t, &Config{File: writeFile(t, dir, "openapi.json", spec), CORS: true})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	resp, body := get(t, srv.URL+"/users/42")
	if resp.StatusCode != 200 || !strings.Contains(body, `"created": "2026-01-15T09:30:00Z"`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	_, body = get(t, srv.URL+"/", "Accept", "application/json")
	if !strings.Contains(body, `"mode": "openapi"`) || !strings.Contains(body, `"/users/{id}"`) {
		t.Fatalf("index: %s", body)
	}
}
