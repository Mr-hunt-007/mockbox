// Package mcp is a small Model Context Protocol server over stdio, written
// against the standard library only. It implements the tools capability:
// initialize, ping, tools/list, tools/call and request cancellation.
//
// Messages are newline-delimited JSON-RPC 2.0 on the reader and writer.
// Nothing else may be written to the writer, so tools must never print to
// stdout; diagnostics belong on stderr.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// SupportedVersions lists the protocol revisions this server speaks, newest
// first. A client asking for one of these gets it back; any other request is
// answered with the newest.
var SupportedVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Annotations are hints to clients about a tool's behaviour.
type Annotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// ReadOnly returns annotations for a tool that does not modify anything.
func ReadOnly(title string) *Annotations {
	t, f := true, false
	return &Annotations{Title: title, ReadOnlyHint: &t, DestructiveHint: &f, IdempotentHint: &t, OpenWorldHint: &f}
}

// Writes returns annotations for a tool that creates or changes files but
// never deletes or overwrites data it did not create.
func Writes(title string) *Annotations {
	t, f := true, false
	return &Annotations{Title: title, ReadOnlyHint: &f, DestructiveHint: &f, IdempotentHint: &t, OpenWorldHint: &f}
}

// Destructive returns annotations for a tool that deletes, kills, or runs
// arbitrary commands.
func Destructive(title string) *Annotations {
	t, f := true, false
	return &Annotations{Title: title, ReadOnlyHint: &f, DestructiveHint: &t, IdempotentHint: &f, OpenWorldHint: &t}
}

// Result is what a tool returns. Text is shown to the model; Structured, when
// set, is sent as structuredContent and must be a JSON object.
type Result struct {
	Text       string
	Structured any
	IsError    bool
}

// Handler runs a tool. args is the raw "arguments" object ({} when absent).
// Returning an error produces a tool result with isError set, which is the
// right shape for failures the model can react to (bad path, not a repo).
type Handler func(ctx context.Context, args json.RawMessage) (Result, error)

// Tool is one callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any
	Annotations *Annotations
	Handler     Handler
}

// Server holds the tool set and identity.
type Server struct {
	Name         string
	Title        string
	Version      string
	Instructions string
	Tools        []Tool
}

// JSONResult returns v as both pretty JSON text and structured content.
// Characters such as &, < and > are kept as they are rather than escaped to
// \u0026 and friends, so URLs and headers stay readable to the model.
func JSONResult(v any) (Result, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return Result{}, err
	}
	b := bytes.TrimRight(buf.Bytes(), "\n")
	var structured any
	if len(b) > 0 && b[0] == '{' {
		structured = json.RawMessage(b)
	}
	return Result{Text: string(b), Structured: structured}, nil
}

// TextResult returns plain text.
func TextResult(s string) Result { return Result{Text: s} }

// Decode unmarshals tool arguments into v, rejecting unknown fields so a
// misspelled argument is reported instead of silently ignored.
func Decode(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	d := json.NewDecoder(bytes.NewReader(args))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type session struct {
	s       *Server
	w       io.Writer
	mu      sync.Mutex // guards writes
	wg      sync.WaitGroup
	cmu     sync.Mutex
	cancels map[string]context.CancelFunc
}

// Serve reads requests from r and writes responses to w until r reaches EOF
// or ctx is cancelled. Requests are handled concurrently; each response is
// written as one line.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	if err := s.validate(); err != nil {
		return err
	}
	ss := &session{s: s, w: w, cancels: map[string]context.CancelFunc{}}
	br := bufio.NewReader(r)
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	go func() {
		for {
			line, err := br.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			ss.wg.Wait()
			return ctx.Err()
		case err := <-readErr:
			ss.wg.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			ss.dispatch(ctx, line)
		}
	}
}

func (s *Server) validate() error {
	seen := map[string]bool{}
	for _, t := range s.Tools {
		if t.Name == "" || t.Handler == nil || t.InputSchema == nil {
			return fmt.Errorf("mcp: tool %q needs a name, handler and input schema", t.Name)
		}
		if seen[t.Name] {
			return fmt.Errorf("mcp: duplicate tool %q", t.Name)
		}
		seen[t.Name] = true
	}
	return nil
}

func (ss *session) dispatch(ctx context.Context, line []byte) {
	line = bytes.TrimSpace(line)
	if line[0] == '[' { // JSON-RPC batch (protocol 2025-03-26)
		var batch []json.RawMessage
		if err := json.Unmarshal(line, &batch); err != nil || len(batch) == 0 {
			ss.write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{CodeParseError, "parse error"}})
			return
		}
		for _, m := range batch {
			ss.one(ctx, m)
		}
		return
	}
	ss.one(ctx, line)
}

func (ss *session) one(ctx context.Context, raw []byte) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		ss.write(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{CodeParseError, "parse error: " + err.Error()}})
		return
	}
	isNotification := len(req.ID) == 0 || string(req.ID) == "null"
	if req.JSONRPC != "2.0" || req.Method == "" {
		if !isNotification {
			ss.write(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{CodeInvalidRequest, "invalid request"}})
		}
		return
	}
	if isNotification {
		if req.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			if json.Unmarshal(req.Params, &p) == nil {
				ss.cmu.Lock()
				if cancel := ss.cancels[string(p.RequestID)]; cancel != nil {
					cancel()
				}
				ss.cmu.Unlock()
			}
		}
		return // notifications/initialized and anything else: no reply
	}

	switch req.Method {
	case "initialize":
		ss.write(response{JSONRPC: "2.0", ID: req.ID, Result: ss.initialize(req.Params)})
	case "ping":
		ss.write(response{JSONRPC: "2.0", ID: req.ID, Result: struct{}{}})
	case "tools/list":
		ss.write(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": ss.list()}})
	case "tools/call":
		cctx, cancel := context.WithCancel(ctx)
		key := string(req.ID)
		ss.cmu.Lock()
		ss.cancels[key] = cancel
		ss.cmu.Unlock()
		ss.wg.Add(1)
		go func() {
			defer ss.wg.Done()
			defer func() {
				ss.cmu.Lock()
				delete(ss.cancels, key)
				ss.cmu.Unlock()
				cancel()
			}()
			res, rerr := ss.call(cctx, req.Params)
			if cctx.Err() != nil && ctx.Err() == nil {
				return // cancelled by the client: the spec says not to respond
			}
			ss.write(response{JSONRPC: "2.0", ID: req.ID, Result: res, Error: rerr})
		}()
	default:
		ss.write(response{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{CodeMethodNotFound, "method not found: " + req.Method}})
	}
}

func (ss *session) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	version := SupportedVersions[0]
	for _, v := range SupportedVersions {
		if v == p.ProtocolVersion {
			version = v
		}
	}
	info := map[string]any{"name": ss.s.Name, "version": ss.s.Version}
	if ss.s.Title != "" {
		info["title"] = ss.s.Title
	}
	out := map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      info,
	}
	if ss.s.Instructions != "" {
		out["instructions"] = ss.s.Instructions
	}
	return out
}

func (ss *session) list() []map[string]any {
	tools := make([]map[string]any, 0, len(ss.s.Tools))
	for _, t := range ss.s.Tools {
		m := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
		if t.Title != "" {
			m["title"] = t.Title
		}
		if t.Annotations != nil {
			m["annotations"] = t.Annotations
		}
		tools = append(tools, m)
	}
	sort.SliceStable(tools, func(i, j int) bool { return tools[i]["name"].(string) < tools[j]["name"].(string) })
	return tools
}

func (ss *session) call(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{CodeInvalidParams, "tools/call needs a tool name"}
	}
	var tool *Tool
	for i := range ss.s.Tools {
		if ss.s.Tools[i].Name == p.Name {
			tool = &ss.s.Tools[i]
		}
	}
	if tool == nil {
		return nil, &rpcError{CodeInvalidParams, "unknown tool: " + p.Name}
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
	}
	res, err := safeRun(ctx, tool.Handler, p.Arguments)
	if err != nil {
		res = Result{Text: err.Error(), IsError: true}
	}
	out := map[string]any{"content": []map[string]any{{"type": "text", "text": res.Text}}}
	if res.Structured != nil {
		out["structuredContent"] = res.Structured
	}
	if res.IsError {
		out["isError"] = true
	}
	return out, nil
}

// safeRun turns a panic in a handler into a tool error so one bad call does
// not take the whole server down.
func safeRun(ctx context.Context, h Handler, args json.RawMessage) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
	}()
	return h(ctx, args)
}

func (ss *session) write(r response) {
	b, err := json.Marshal(r)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: r.ID, Error: &rpcError{CodeInternalError, err.Error()}})
	}
	// encoding/json never emits raw newlines, so one message is one line.
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.w.Write(append(b, '\n'))
}

// Schema helpers for building input schemas without typos.

// Object returns a JSON Schema object with the given properties.
func Object(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

// String describes a string property.
func String(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

// Integer describes an integer property.
func Integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Boolean describes a boolean property.
func Boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// StringArray describes an array of strings.
func StringArray(desc string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": desc}
}

// Enum describes a string restricted to values.
func Enum(desc string, values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values, "description": desc}
}

// Capture runs fn with fresh stdout and stderr buffers and returns both. It
// is the usual way to reuse a CLI's --json path from a tool handler.
func Capture(fn func(stdout, stderr io.Writer) int) (stdout, stderr string, code int) {
	var o, e strings.Builder
	code = fn(&o, &e)
	return o.String(), e.String(), code
}
