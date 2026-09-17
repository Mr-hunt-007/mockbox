// Package app wires the command line, the backends and the HTTP server.
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mr-hunt-007/mockbox/internal/db"
	"github.com/Mr-hunt-007/mockbox/internal/httpx"
	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
	"github.com/Mr-hunt-007/mockbox/internal/openapi"
	"github.com/Mr-hunt-007/mockbox/internal/rewrite"
	"github.com/Mr-hunt-007/mockbox/internal/watch"
)

// Route is one row of the route table.
type Route struct {
	Method string
	Path   string
}

type backend interface {
	http.Handler
	routes(readonly bool) []Route
	mode() string // "database" or "openapi"
	hasRoot() bool
}

type dbBackend struct{ d *db.DB }

func (b *dbBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) { b.d.ServeHTTP(w, r) }
func (b *dbBackend) mode() string                                     { return "database" }
func (b *dbBackend) hasRoot() bool                                    { return false }
func (b *dbBackend) routes(readonly bool) []Route {
	var out []Route
	for _, r := range b.d.Routes(readonly) {
		out = append(out, Route{r.Method, r.Path})
	}
	return out
}

type specBackend struct{ s *openapi.Spec }

func (b *specBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) { b.s.ServeHTTP(w, r) }
func (b *specBackend) mode() string                                     { return "openapi" }
func (b *specBackend) hasRoot() bool                                    { return b.s.HasRoot() }
func (b *specBackend) routes(readonly bool) []Route {
	var out []Route
	for _, r := range b.s.Routes() {
		if readonly && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			continue
		}
		out = append(out, Route{r.Method, r.Path})
	}
	return out
}

// InputError is a problem with the source, spec or routes file (exit 3).
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

// UsageError is an invalid combination of flags and input (exit 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// App is a configured mockbox server.
type App struct {
	cfg     *Config
	stdout  io.Writer
	stderr  io.Writer
	color   bool
	outMu   sync.Mutex
	mu      sync.RWMutex
	backend backend
	rules   []rewrite.Rule
	watcher *watch.Watcher
	// PollInterval is how often --watch checks the file.
	PollInterval time.Duration
}

// New loads the input files. It does not listen yet.
func New(cfg *Config, stdout, stderr io.Writer, color bool) (*App, error) {
	a := &App{cfg: cfg, stdout: stdout, stderr: stderr, color: color, PollInterval: 300 * time.Millisecond}
	if cfg.Watch {
		// Record the file state before loading so an edit made during
		// startup is still picked up.
		a.watcher = watch.New(cfg.File)
	}
	if cfg.Routes != "" {
		raw, err := os.ReadFile(cfg.Routes)
		if err != nil {
			return nil, &InputError{fmt.Sprintf("cannot read routes file: %v", err)}
		}
		rules, err := rewrite.Parse(raw)
		if err != nil {
			return nil, &InputError{fmt.Sprintf("%s: %v", cfg.Routes, err)}
		}
		a.rules = rules
	}
	b, warnings, err := a.load()
	if err != nil {
		return nil, err
	}
	for _, w := range warnings {
		a.warnf("%s", w)
	}
	a.backend = b
	return a, nil
}

// Open loads file (and an optional routes file) for in-memory use, the way
// the MCP tools need it: nothing is printed, nothing is watched, and changes
// made through Handler never reach the file. Warnings about top-level keys
// that cannot be served are returned instead of printed.
func Open(file, routes string) (*App, []string, error) {
	var warn bytes.Buffer
	a, err := New(&Config{File: file, Routes: routes, Host: "127.0.0.1", Port: 3000, Quiet: true}, io.Discard, &warn, false)
	if err != nil {
		return nil, nil, err
	}
	var warnings []string
	for _, line := range strings.Split(strings.TrimSpace(warn.String()), "\n") {
		if line = strings.TrimPrefix(line, "mockbox: warning: "); line != "" {
			warnings = append(warnings, line)
		}
	}
	return a, warnings, nil
}

// Mode is "database" or "openapi".
func (a *App) Mode() string { return a.current().mode() }

// Routes is the route table as the server prints it at startup.
func (a *App) Routes() []Route { return a.current().routes(a.cfg.Readonly) }

// Rewrites returns the --routes rules in file order.
func (a *App) Rewrites() []rewrite.Rule { return a.rules }

// DB returns the database, or nil in OpenAPI mode.
func (a *App) DB() *db.DB {
	if b, ok := a.current().(*dbBackend); ok {
		return b.d
	}
	return nil
}

// Spec returns the OpenAPI spec, or nil in database mode.
func (a *App) Spec() *openapi.Spec {
	if b, ok := a.current().(*specBackend); ok {
		return b.s
	}
	return nil
}

const yamlHelp = `%[1]s: YAML is not supported (mockbox uses only the Go standard library, which has no YAML parser).
Convert the spec to JSON first, for example:
  yq -o=json %[1]s > %[2]s
  python3 -c 'import sys,yaml,json; json.dump(yaml.safe_load(open(sys.argv[1])), sys.stdout, indent=2)' %[1]s > %[2]s
then run: mockbox %[2]s`

func yamlError(file string) error {
	out := strings.TrimSuffix(file, filepath.Ext(file)) + ".json"
	return &InputError{fmt.Sprintf(yamlHelp, file, out)}
}

func looksLikeYAML(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		return strings.HasPrefix(t, "openapi:") || strings.HasPrefix(t, "swagger:")
	}
	return false
}

// load reads and parses the source file into a backend.
func (a *App) load() (backend, []string, error) {
	file := a.cfg.File
	switch strings.ToLower(filepath.Ext(file)) {
	case ".yaml", ".yml":
		return nil, nil, yamlError(file)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, &InputError{fmt.Sprintf("cannot read %s: %v", file, unwrapPathError(err))}
	}
	doc, err := jsonx.Parse(raw)
	if err != nil {
		if looksLikeYAML(raw) {
			return nil, nil, yamlError(file)
		}
		return nil, nil, &InputError{fmt.Sprintf("%s is not valid JSON: %v", file, err)}
	}
	if openapi.IsSpec(doc) {
		if a.cfg.Persist {
			return nil, nil, &UsageError{fmt.Sprintf("--persist only works with a JSON database; %s is an OpenAPI spec", file)}
		}
		spec, err := openapi.Load(doc)
		if err != nil {
			return nil, nil, &InputError{fmt.Sprintf("%s: %v", file, err)}
		}
		return &specBackend{spec}, nil, nil
	}
	obj, warnings, err := db.Parse(doc)
	if err != nil {
		return nil, nil, &InputError{fmt.Sprintf("%s: %v", file, err)}
	}
	return &dbBackend{db.New(file, obj, a.cfg.Persist)}, warnings, nil
}

func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

func (a *App) current() backend {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.backend
}

// Reload re-reads the source file. On any error the last good data stays live.
func (a *App) Reload() {
	if b, ok := a.current().(*dbBackend); ok {
		changed, warnings, err := b.d.ReloadFile()
		for _, w := range warnings {
			a.warnf("%s", w)
		}
		switch {
		case err == nil:
			if changed {
				a.event("reload", true, fmt.Sprintf("reloaded %s", a.cfg.File))
			}
			return
		case !errors.Is(err, db.ErrNotDatabase):
			a.event("reload", false, fmt.Sprintf("reload failed, still serving the last good data: %s: %v", a.cfg.File, unwrapPathError(err)))
			return
		}
	}
	nb, warnings, err := a.load()
	if err != nil {
		a.event("reload", false, fmt.Sprintf("reload failed, still serving the last good data: %v", err))
		return
	}
	for _, w := range warnings {
		a.warnf("%s", w)
	}
	a.mu.Lock()
	a.backend = nb
	a.mu.Unlock()
	a.event("reload", true, fmt.Sprintf("reloaded %s (%s)", a.cfg.File, nb.mode()))
}

// Handler returns the full middleware chain.
func (a *App) Handler() http.Handler {
	var h http.Handler = http.HandlerFunc(a.route)
	if len(a.rules) > 0 {
		h = a.rewriteMW(h)
	}
	if a.cfg.Readonly {
		h = readonlyMW(h)
	}
	if a.cfg.CORS {
		h = corsMW(h)
	}
	if a.cfg.Delay > 0 {
		h = delayMW(time.Duration(a.cfg.Delay)*time.Millisecond, h)
	}
	return a.logMW(h)
}

func (a *App) route(w http.ResponseWriter, r *http.Request) {
	b := a.current()
	if strings.Trim(r.URL.Path, "/") == "" && !b.hasRoot() {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			httpx.MethodNotAllowed(w, r, []string{"GET", "HEAD"})
			return
		}
		a.index(w, r, b)
		return
	}
	b.ServeHTTP(w, r)
}

var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>mockbox</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;max-width:760px;margin:40px auto;padding:0 16px;color:#1d1d1f;background:#fff}
h1{font-size:20px;margin:0 0 4px}p{color:#555;margin:0 0 20px}
table{border-collapse:collapse;width:100%}td{padding:4px 8px;border-bottom:1px solid #eee;font-family:ui-monospace,monospace;font-size:13px}
td.m{width:70px;font-weight:600}a{color:#0a58ca;text-decoration:none}a:hover{text-decoration:underline}
@media (prefers-color-scheme:dark){body{background:#111;color:#eee}p{color:#aaa}td{border-color:#2a2a2a}a{color:#6ea8fe}}
</style></head><body>
<h1>mockbox {{.Version}}</h1>
<p>Serving <code>{{.File}}</code> ({{.Mode}}{{if .Readonly}}, read-only{{end}}{{if .Persist}}, changes saved to file{{end}})</p>
<table>{{range .Routes}}<tr><td class="m">{{.Method}}</td><td>{{if .Link}}<a href="{{.Path}}">{{.Path}}</a>{{else}}{{.Path}}{{end}}</td></tr>{{end}}</table>
</body></html>
`))

func (a *App) index(w http.ResponseWriter, r *http.Request, b backend) {
	routes := b.routes(a.cfg.Readonly)
	if httpx.PrefersJSON(r.Header.Get("Accept")) {
		o := jsonx.NewObject()
		o.Set("name", "mockbox")
		o.Set("version", Version)
		o.Set("file", filepath.Base(a.cfg.File))
		o.Set("mode", b.mode())
		o.Set("readonly", a.cfg.Readonly)
		o.Set("persist", a.cfg.Persist)
		o.Set("routes", routesJSON(routes))
		httpx.WriteJSON(w, http.StatusOK, o)
		return
	}
	type row struct {
		Method, Path string
		Link         bool
	}
	data := struct {
		Version, File, Mode string
		Readonly, Persist   bool
		Routes              []row
	}{Version, filepath.Base(a.cfg.File), b.mode(), a.cfg.Readonly, a.cfg.Persist, nil}
	for _, rt := range routes {
		link := rt.Method == "GET" && !strings.ContainsAny(rt.Path, ":{")
		data.Routes = append(data.Routes, row{rt.Method, rt.Path, link})
	}
	var buf bytes.Buffer
	_ = indexTmpl.Execute(&buf, data)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept")
	_, _ = w.Write(buf.Bytes())
}

func routesJSON(routes []Route) []any {
	out := []any{}
	for _, rt := range routes {
		o := jsonx.NewObject()
		o.Set("method", rt.Method)
		o.Set("path", rt.Path)
		out = append(out, o)
	}
	return out
}

// reqInfo lets inner middleware report back to the logger.
type reqInfo struct{ rewrittenTo string }

type ctxKey int

const infoKey ctxKey = 1

func (a *App) rewriteMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		orig := *r.URL
		path, query, ok := rewrite.Rewrite(a.rules, r.URL.Path, r.URL.RawQuery)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		r2 := httpx.WithOriginalURL(r, &orig)
		u := *r.URL
		u.Path, u.RawPath, u.RawQuery = path, "", query
		r2.URL = &u
		if info, ok := r.Context().Value(infoKey).(*reqInfo); ok {
			info.rewrittenTo = u.RequestURI()
		}
		next.ServeHTTP(w, r2)
	})
}

func readonlyMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			httpx.WriteError(w, http.StatusMethodNotAllowed, fmt.Sprintf("%s is not allowed: the server is read-only (--readonly)", r.Method))
		}
	})
}

func corsMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if origin := r.Header.Get("Origin"); origin != "" {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
		} else {
			h.Set("Access-Control-Allow-Origin", "*")
		}
		h.Set("Access-Control-Expose-Headers", "X-Total-Count, Link, Location")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				h.Set("Access-Control-Allow-Headers", req)
				h.Add("Vary", "Access-Control-Request-Headers")
			} else {
				h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func delayMW(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (a *App) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &reqInfo{}
		r = r.WithContext(context.WithValue(r.Context(), infoKey, info))
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if a.cfg.Quiet {
			return
		}
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		dur := time.Since(start)
		if a.cfg.JSON {
			o := jsonx.NewObject()
			o.Set("event", "request")
			o.Set("time", start.UTC().Format(time.RFC3339Nano))
			o.Set("method", r.Method)
			o.Set("path", r.URL.RequestURI())
			if info.rewrittenTo != "" {
				o.Set("rewritten_to", info.rewrittenTo)
			}
			o.Set("status", status)
			o.Set("duration_ms", float64(dur.Microseconds())/1000)
			a.println(string(jsonx.Marshal(o, "")))
			return
		}
		line := fmt.Sprintf("%s %-6s %s", a.paint("2", start.Format("15:04:05")), r.Method, r.URL.RequestURI())
		if info.rewrittenTo != "" {
			line += " -> " + info.rewrittenTo
		}
		line += fmt.Sprintf(" %s %s", a.paint(statusColor(status), strconv.Itoa(status)), formatDuration(dur))
		a.println(line)
	})
}

func formatDuration(d time.Duration) string {
	ms := float64(d.Microseconds()) / 1000
	if ms >= 1000 {
		return fmt.Sprintf("%.2fs", ms/1000)
	}
	return fmt.Sprintf("%.1fms", ms)
}

func statusColor(code int) string {
	switch {
	case code >= 500:
		return "31"
	case code >= 400:
		return "33"
	case code >= 300:
		return "36"
	}
	return "32"
}

func (a *App) paint(code, s string) string {
	if !a.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func (a *App) println(s string) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	fmt.Fprintln(a.stdout, s)
}

func (a *App) warnf(format string, args ...any) {
	a.outMu.Lock()
	defer a.outMu.Unlock()
	fmt.Fprintf(a.stderr, "mockbox: warning: "+format+"\n", args...)
}

// event reports a reload. Failures always reach stderr, even with --quiet.
func (a *App) event(kind string, ok bool, msg string) {
	if a.cfg.JSON && !a.cfg.Quiet {
		o := jsonx.NewObject()
		o.Set("event", kind)
		o.Set("time", time.Now().UTC().Format(time.RFC3339Nano))
		o.Set("ok", ok)
		o.Set("message", msg)
		a.println(string(jsonx.Marshal(o, "")))
		if ok {
			return
		}
	}
	if !ok {
		a.outMu.Lock()
		fmt.Fprintf(a.stderr, "mockbox: %s\n", msg)
		a.outMu.Unlock()
		return
	}
	if !a.cfg.Quiet {
		a.println(a.paint("36", msg))
	}
}

// Listen binds the configured address, explaining a port conflict clearly.
func (a *App) Listen() (net.Listener, error) {
	addr := net.JoinHostPort(a.cfg.Host, strconv.Itoa(a.cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "address already in use") || strings.Contains(msg, "only one usage of each socket address") {
			return nil, fmt.Errorf("port %d is already in use on %s; stop the other process or pick another port with --port %d", a.cfg.Port, a.cfg.Host, a.cfg.Port+1)
		}
		return nil, fmt.Errorf("cannot listen on %s: %v", addr, err)
	}
	return ln, nil
}

// Banner writes the startup information for the given listen address.
func (a *App) Banner(addr net.Addr) {
	if a.cfg.Quiet {
		return
	}
	b := a.current()
	url := "http://" + addr.String()
	routes := b.routes(a.cfg.Readonly)
	if a.cfg.JSON {
		o := jsonx.NewObject()
		o.Set("event", "start")
		o.Set("version", Version)
		o.Set("file", a.cfg.File)
		o.Set("mode", b.mode())
		o.Set("url", url)
		o.Set("readonly", a.cfg.Readonly)
		o.Set("persist", a.cfg.Persist)
		o.Set("routes", routesJSON(routes))
		rw := []any{}
		for _, r := range a.rules {
			ro := jsonx.NewObject()
			ro.Set("from", r.From)
			ro.Set("to", r.To)
			rw = append(rw, ro)
		}
		o.Set("rewrites", rw)
		a.println(string(jsonx.Marshal(o, "")))
		return
	}
	var sb strings.Builder
	storage := "in memory, file is never modified"
	if a.cfg.Persist {
		storage = "changes are written to the file"
	}
	if b.mode() == "openapi" {
		storage = "static responses from the spec"
	}
	fmt.Fprintf(&sb, "%s %s serving %s (%s, %s)\n\n", a.paint("1", "mockbox"), Version, a.cfg.File, b.mode(), storage)
	for _, r := range routes {
		fmt.Fprintf(&sb, "  %-7s%s\n", r.Method, r.Path)
	}
	if len(routes) == 0 {
		sb.WriteString("  (no routes)\n")
	}
	if sb2, ok := b.(*specBackend); ok && sb2.s.BasePath != "" {
		fmt.Fprintf(&sb, "\n  Paths are also served under %s (from servers[0].url)\n", sb2.s.BasePath)
	}
	if len(a.rules) > 0 {
		sb.WriteString("\n  Rewrites:\n")
		for _, r := range a.rules {
			fmt.Fprintf(&sb, "  %s -> %s\n", r.From, r.To)
		}
	}
	var opts []string
	if a.cfg.Watch {
		opts = append(opts, "watch")
	}
	if a.cfg.CORS {
		opts = append(opts, "cors")
	}
	if a.cfg.Readonly {
		opts = append(opts, "readonly")
	}
	if a.cfg.Delay > 0 {
		opts = append(opts, fmt.Sprintf("delay %dms", a.cfg.Delay))
	}
	sb.WriteString("\n")
	if len(opts) > 0 {
		fmt.Fprintf(&sb, "Options: %s\n", strings.Join(opts, ", "))
	}
	fmt.Fprintf(&sb, "Listening on %s (Ctrl-C to stop)", a.paint("1", url))
	a.println(sb.String())
}

// Serve runs the server on ln until ctx is cancelled, then shuts down gracefully.
func (a *App) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           a.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	a.Banner(ln.Addr())
	if a.watcher != nil {
		go a.watcher.Run(ctx, a.PollInterval, a.Reload)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if !a.cfg.Quiet && !a.cfg.JSON {
		a.println("\nStopped.")
	}
	return err
}
