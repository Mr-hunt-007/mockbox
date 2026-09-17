// Package db serves a JSON file as a REST API: top-level arrays become
// collections and top-level objects become singular resources.
package db

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Mr-hunt-007/mockbox/internal/httpx"
	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

// Route is one entry of the route table.
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// DB is an in-memory JSON database guarded by a mutex.
type DB struct {
	mu          sync.RWMutex
	path        string
	data        *jsonx.Object
	persist     bool
	lastWritten []byte
	// NewID generates ids for collections whose ids are not all integers.
	NewID func() string
}

// ErrNotDatabase is returned by Parse when the document is an OpenAPI spec.
var ErrNotDatabase = errors.New("document is an OpenAPI spec, not a database")

// Parse validates a decoded document as a database and returns warnings
// about top-level keys that cannot be served.
func Parse(doc any) (*jsonx.Object, []string, error) {
	obj, ok := doc.(*jsonx.Object)
	if !ok {
		return nil, nil, fmt.Errorf("top level must be a JSON object whose keys are resources, got %s", httpx.TypeName(doc))
	}
	if _, ok := obj.Get("openapi"); ok {
		return nil, nil, ErrNotDatabase
	}
	var warnings []string
	for _, k := range obj.Keys() {
		v, _ := obj.Get(k)
		switch {
		case k == "" || strings.ContainsAny(k, "/?#"):
			warnings = append(warnings, fmt.Sprintf("skipping key %q: not usable as a URL path segment", k))
		default:
			switch v.(type) {
			case []any, *jsonx.Object:
			default:
				warnings = append(warnings, fmt.Sprintf("skipping key %q: %s is neither an array (collection) nor an object (singular resource)", k, httpx.TypeName(v)))
			}
		}
	}
	return obj, warnings, nil
}

// New builds a DB from a parsed document. path is used for --persist and reloads.
func New(path string, data *jsonx.Object, persist bool) *DB {
	return &DB{path: path, data: data, persist: persist, NewID: randomID}
}

func randomID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func validName(k string) bool { return k != "" && !strings.ContainsAny(k, "/?#") }

// ReloadFile re-reads the file and replaces the data. It holds the write lock
// for the whole read so a concurrent write cannot be lost. It returns
// changed=false when the file holds exactly what mockbox last wrote.
func (d *DB) ReloadFile() (changed bool, warnings []string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	raw, err := os.ReadFile(d.path)
	if err != nil {
		return false, nil, err
	}
	if d.lastWritten != nil && bytes.Equal(raw, d.lastWritten) {
		return false, nil, nil
	}
	doc, err := jsonx.Parse(raw)
	if err != nil {
		return false, nil, err
	}
	obj, warnings, err := Parse(doc)
	if err != nil {
		return false, nil, err
	}
	d.data = obj
	d.lastWritten = nil
	return true, warnings, nil
}

// Routes returns the route table. readonly limits it to GET routes.
func (d *DB) Routes(readonly bool) []Route {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []Route
	add := func(m, p string) {
		if readonly && m != http.MethodGet {
			return
		}
		out = append(out, Route{m, p})
	}
	for _, k := range d.data.Keys() {
		if !validName(k) {
			continue
		}
		v, _ := d.data.Get(k)
		switch v.(type) {
		case []any:
			add("GET", "/"+k)
			add("GET", "/"+k+"/:id")
			for _, child := range d.childrenOf(k) {
				add("GET", "/"+k+"/:id/"+child)
			}
			add("POST", "/"+k)
			add("PUT", "/"+k+"/:id")
			add("PATCH", "/"+k+"/:id")
			add("DELETE", "/"+k+"/:id")
		case *jsonx.Object:
			add("GET", "/"+k)
			add("PUT", "/"+k)
			add("PATCH", "/"+k)
		}
	}
	return out
}

// Collection names a collection and how many items it holds.
type Collection struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// Resources lists the servable collections and singular resources in file order.
func (d *DB) Resources() (collections []Collection, singular []string) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	collections, singular = []Collection{}, []string{}
	for _, k := range d.data.Keys() {
		if !validName(k) {
			continue
		}
		v, _ := d.data.Get(k)
		switch t := v.(type) {
		case []any:
			collections = append(collections, Collection{k, len(t)})
		case *jsonx.Object:
			singular = append(singular, k)
		}
	}
	return collections, singular
}

// Relation is a link between two collections, detected the same way the
// server resolves /parent/:id/child, _embed and _expand.
type Relation struct {
	Parent     string `json:"parent"`
	Child      string `json:"child"`
	ForeignKey string `json:"foreign_key"`
	// Expand is the name to pass as ?_expand= on the child to include the
	// parent, or "" when _expand cannot reach this parent.
	Expand string `json:"expand,omitempty"`
}

// Relations lists every detected parent/child pair in file order.
func (d *DB) Relations() []Relation {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := []Relation{}
	for _, parent := range d.data.Keys() {
		if _, ok := d.collection(parent); !ok {
			continue
		}
		for _, child := range d.childrenOf(parent) {
			items, _ := d.collection(child)
			rel := Relation{Parent: parent, Child: child, ForeignKey: d.foreignKeyIn(parent, items)}
			if name, ok := strings.CutSuffix(rel.ForeignKey, "Id"); ok && name != "" {
				// decorate uses the first existing candidate collection.
				for _, cand := range pluralCandidates(name) {
					if _, exists := d.collection(cand); exists {
						if cand == parent {
							rel.Expand = name
						}
						break
					}
				}
			}
			out = append(out, rel)
		}
	}
	return out
}

// childrenOf lists collections whose items carry a foreign key to parent.
func (d *DB) childrenOf(parent string) []string {
	var out []string
	for _, k := range d.data.Keys() {
		if k == parent || !validName(k) {
			continue
		}
		items, ok := d.collection(k)
		if !ok {
			continue
		}
		if d.foreignKeyIn(parent, items) != "" {
			out = append(out, k)
		}
	}
	return out
}

// foreignKeyIn returns the foreign key to parent used by any of items, or "".
func (d *DB) foreignKeyIn(parent string, items []any) string {
	for _, fk := range ForeignKeys(parent) {
		for _, it := range items {
			if o, ok := it.(*jsonx.Object); ok {
				if _, has := o.Get(fk); has {
					return fk
				}
			}
		}
	}
	return ""
}

func (d *DB) collection(name string) ([]any, bool) {
	if !validName(name) {
		return nil, false
	}
	v, ok := d.data.Get(name)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	return arr, ok
}

func idOf(item any) (any, bool) {
	o, ok := item.(*jsonx.Object)
	if !ok {
		return nil, false
	}
	return o.Get("id")
}

func idString(v any) string {
	s, _ := ScalarString(v)
	return s
}

func findIndex(items []any, id string) int {
	for i, it := range items {
		if v, ok := idOf(it); ok && v != nil && idString(v) == id {
			return i
		}
	}
	return -1
}

// response is computed under the lock and written after it is released.
type response struct {
	status int
	body   []byte
	header http.Header
}

func jsonResponse(status int, v any) *response {
	return &response{status: status, body: append(jsonx.Marshal(v, "  "), '\n'), header: http.Header{}}
}

// ServeHTTP routes /name, /name/:id and /name/:id/child.
func (d *DB) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.Trim(r.URL.Path, "/")
	if p == "" {
		httpx.NotFound(w, r)
		return
	}
	segs := strings.Split(p, "/")
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}

	var resp *response
	var err error
	switch len(segs) {
	case 1:
		resp, err = d.serveResource(w, r, method, segs[0])
	case 2:
		resp, err = d.serveItem(w, r, method, segs[0], segs[1])
	case 3:
		resp, err = d.serveNested(w, r, method, segs[0], segs[1], segs[2])
	default:
		err = errNotFound
	}
	if errors.Is(err, errNotFound) {
		httpx.NotFound(w, r)
		return
	}
	var mna *methodNotAllowed
	if errors.As(err, &mna) {
		httpx.MethodNotAllowed(w, r, mna.allow)
		return
	}
	var qe *QueryError
	if errors.As(err, &qe) {
		httpx.WriteError(w, http.StatusBadRequest, qe.Msg)
		return
	}
	if err != nil {
		httpx.WriteErr(w, err)
		return
	}
	for k, vs := range resp.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.status)
	_, _ = w.Write(resp.body)
}

var errNotFound = errors.New("not found")

type methodNotAllowed struct{ allow []string }

func (m *methodNotAllowed) Error() string { return "method not allowed" }

func (d *DB) serveResource(w http.ResponseWriter, r *http.Request, method, name string) (*response, error) {
	d.mu.RLock()
	v, ok := d.data.Get(name)
	d.mu.RUnlock()
	if !ok || !validName(name) {
		return nil, errNotFound
	}
	switch v.(type) {
	case []any:
		switch method {
		case http.MethodGet:
			d.mu.RLock()
			defer d.mu.RUnlock()
			items, ok := d.collection(name)
			if !ok {
				return nil, errNotFound
			}
			return d.list(r, name, items)
		case http.MethodPost:
			body, herr := httpx.ReadJSONObject(w, r)
			if herr != nil {
				return nil, herr
			}
			return d.create(r, name, body, "", nil)
		}
		return nil, &methodNotAllowed{[]string{"GET", "HEAD", "POST"}}
	case *jsonx.Object:
		switch method {
		case http.MethodGet:
			d.mu.RLock()
			defer d.mu.RUnlock()
			cur, ok := d.data.Get(name)
			if !ok {
				return nil, errNotFound
			}
			return jsonResponse(http.StatusOK, cur), nil
		case http.MethodPut, http.MethodPatch:
			body, herr := httpx.ReadJSONObject(w, r)
			if herr != nil {
				return nil, herr
			}
			return d.mutate(name, func() (*response, error) {
				cur, ok := d.data.Get(name)
				curObj, isObj := cur.(*jsonx.Object)
				if !ok || !isObj {
					return nil, errNotFound
				}
				if method == http.MethodPut {
					d.data.Set(name, body)
					return jsonResponse(http.StatusOK, body), nil
				}
				for _, k := range body.Keys() {
					val, _ := body.Get(k)
					curObj.Set(k, val)
				}
				return jsonResponse(http.StatusOK, curObj), nil
			})
		}
		return nil, &methodNotAllowed{[]string{"GET", "HEAD", "PUT", "PATCH"}}
	}
	return nil, errNotFound
}

func (d *DB) serveItem(w http.ResponseWriter, r *http.Request, method, name, id string) (*response, error) {
	d.mu.RLock()
	_, isColl := d.collection(name)
	d.mu.RUnlock()
	if !isColl {
		return nil, errNotFound
	}
	switch method {
	case http.MethodGet:
		d.mu.RLock()
		defer d.mu.RUnlock()
		items, ok := d.collection(name)
		if !ok {
			return nil, errNotFound
		}
		i := findIndex(items, id)
		if i < 0 {
			return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", name, id)
		}
		out, err := d.decorate(r.URL.Query(), name, []any{items[i]})
		if err != nil {
			return nil, err
		}
		return jsonResponse(http.StatusOK, out[0]), nil
	case http.MethodPut, http.MethodPatch:
		body, herr := httpx.ReadJSONObject(w, r)
		if herr != nil {
			return nil, herr
		}
		return d.mutate(name, func() (*response, error) {
			items, _ := d.collection(name)
			i := findIndex(items, id)
			if i < 0 {
				return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", name, id)
			}
			cur, isObj := items[i].(*jsonx.Object)
			if !isObj {
				return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", name, id)
			}
			idVal, _ := cur.Get("id")
			if method == http.MethodPut {
				body.SetFirst("id", idVal)
				items[i] = body
				return jsonResponse(http.StatusOK, body), nil
			}
			for _, k := range body.Keys() {
				if k == "id" {
					continue
				}
				val, _ := body.Get(k)
				cur.Set(k, val)
			}
			return jsonResponse(http.StatusOK, cur), nil
		})
	case http.MethodDelete:
		return d.mutate(name, func() (*response, error) {
			items, _ := d.collection(name)
			i := findIndex(items, id)
			if i < 0 {
				return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", name, id)
			}
			removed := items[i]
			next := make([]any, 0, len(items)-1)
			next = append(next, items[:i]...)
			next = append(next, items[i+1:]...)
			d.data.Set(name, next)
			return jsonResponse(http.StatusOK, removed), nil
		})
	}
	return nil, &methodNotAllowed{[]string{"GET", "HEAD", "PUT", "PATCH", "DELETE"}}
}

func (d *DB) serveNested(w http.ResponseWriter, r *http.Request, method, parent, id, child string) (*response, error) {
	d.mu.RLock()
	_, pok := d.collection(parent)
	_, cok := d.collection(child)
	d.mu.RUnlock()
	if !pok || !cok || parent == child {
		return nil, errNotFound
	}
	switch method {
	case http.MethodGet:
		d.mu.RLock()
		defer d.mu.RUnlock()
		parents, _ := d.collection(parent)
		children, _ := d.collection(child)
		pi := findIndex(parents, id)
		if pi < 0 {
			return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", parent, id)
		}
		fk := d.foreignKeyIn(parent, children)
		if fk == "" {
			fk = ForeignKeys(parent)[0]
		}
		var sub []any
		for _, c := range children {
			if v, ok := Lookup(c, fk); ok && v != nil && idString(v) == id {
				sub = append(sub, c)
			}
		}
		return d.list(r, child, sub)
	case http.MethodPost:
		body, herr := httpx.ReadJSONObject(w, r)
		if herr != nil {
			return nil, herr
		}
		d.mu.RLock()
		parents, _ := d.collection(parent)
		children, _ := d.collection(child)
		pi := findIndex(parents, id)
		var parentID any
		if pi >= 0 {
			parentID, _ = idOf(parents[pi])
		}
		fk := d.foreignKeyIn(parent, children)
		d.mu.RUnlock()
		if pi < 0 {
			return nil, httpx.Errorf(http.StatusNotFound, "%s with id %q not found", parent, id)
		}
		if fk == "" {
			fk = ForeignKeys(parent)[0]
		}
		return d.create(r, child, body, fk, parentID)
	}
	return nil, &methodNotAllowed{[]string{"GET", "HEAD", "POST"}}
}

// list applies query parameters and embeds. Caller holds at least the read lock.
func (d *DB) list(r *http.Request, name string, items []any) (*response, error) {
	q := r.URL.Query()
	res, err := ApplyList(items, q)
	if err != nil {
		return nil, err
	}
	out, err := d.decorate(q, name, res.Items)
	if err != nil {
		return nil, err
	}
	resp := jsonResponse(http.StatusOK, out)
	resp.header.Set("X-Total-Count", strconv.Itoa(res.Total))
	if link := LinkHeader(httpx.OriginalURL(r), r.Host, res); link != "" {
		resp.header.Set("Link", link)
	}
	return resp, nil
}

func splitList(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// decorate applies _embed and _expand to copies of items. Caller holds the read lock.
func (d *DB) decorate(q url.Values, name string, items []any) ([]any, error) {
	embeds, expands := splitList(q["_embed"]), splitList(q["_expand"])
	if len(embeds) == 0 && len(expands) == 0 {
		return items, nil
	}
	type expandTarget struct {
		key, fk string
		items   []any
	}
	var targets []expandTarget
	for _, e := range expands {
		found := false
		for _, cand := range pluralCandidates(e) {
			if arr, ok := d.collection(cand); ok {
				targets = append(targets, expandTarget{key: e, fk: e + "Id", items: arr})
				found = true
				break
			}
		}
		if !found {
			return nil, qerr("_expand=%s: no collection named %ss (or similar) to expand from", e, e)
		}
	}
	type embedTarget struct {
		key, fk string
		items   []any
	}
	var embedTargets []embedTarget
	for _, e := range embeds {
		arr, ok := d.collection(e)
		if !ok {
			return nil, qerr("_embed=%s: no collection named %q", e, e)
		}
		fk := d.foreignKeyIn(name, arr)
		if fk == "" {
			fk = ForeignKeys(name)[0]
		}
		embedTargets = append(embedTargets, embedTarget{key: e, fk: fk, items: arr})
	}

	out := make([]any, len(items))
	for i, it := range items {
		o, ok := it.(*jsonx.Object)
		if !ok {
			out[i] = it
			continue
		}
		c := o.ShallowCopy()
		idVal, hasID := o.Get("id")
		for _, et := range embedTargets {
			sub := []any{}
			if hasID && idVal != nil {
				for _, child := range et.items {
					if v, ok := Lookup(child, et.fk); ok && v != nil && idString(v) == idString(idVal) {
						sub = append(sub, child)
					}
				}
			}
			c.Set(et.key, sub)
		}
		for _, xt := range targets {
			ref, ok := o.Get(xt.fk)
			if !ok || ref == nil {
				continue
			}
			if j := findIndex(xt.items, idString(ref)); j >= 0 {
				c.Set(xt.key, xt.items[j])
			}
		}
		out[i] = c
	}
	return out, nil
}

// create inserts body into the collection. When fk is set, body[fk] is forced
// to parentID (nested POST).
func (d *DB) create(r *http.Request, name string, body *jsonx.Object, fk string, parentID any) (*response, error) {
	return d.mutate(name, func() (*response, error) {
		items, ok := d.collection(name)
		if !ok {
			return nil, errNotFound
		}
		if fk != "" {
			body.Set(fk, parentID)
		}
		if idVal, has := body.Get("id"); has && idVal != nil {
			if _, scalar := ScalarString(idVal); !scalar {
				return nil, httpx.Errorf(http.StatusBadRequest, "id must be a string or number, got %s", httpx.TypeName(idVal))
			}
			if findIndex(items, idString(idVal)) >= 0 {
				return nil, httpx.Errorf(http.StatusConflict, "%s with id %s already exists", name, idString(idVal))
			}
		} else {
			body.SetFirst("id", d.nextID(items))
		}
		d.data.Set(name, append(items, body))
		resp := jsonResponse(http.StatusCreated, body)
		idv, _ := body.Get("id")
		loc := httpx.OriginalURL(r).Path
		if fk != "" {
			loc = "/" + name
		}
		resp.header.Set("Location", strings.TrimRight(loc, "/")+"/"+url.PathEscape(idString(idv)))
		return resp, nil
	})
}

// nextID returns max+1 when every existing id is an integer (1 for an empty
// collection), otherwise a random string id.
func (d *DB) nextID(items []any) any {
	var max int64
	for _, it := range items {
		v, ok := idOf(it)
		if !ok {
			continue
		}
		n, isNum := v.(json.Number)
		if !isNum {
			return d.unusedRandomID(items)
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return d.unusedRandomID(items)
		}
		if i > max {
			max = i
		}
	}
	return json.Number(strconv.FormatInt(max+1, 10))
}

func (d *DB) unusedRandomID(items []any) any {
	for {
		id := d.NewID()
		if findIndex(items, id) < 0 {
			return id
		}
	}
}

// mutate runs fn under the write lock. With --persist the file is rewritten
// atomically; if that fails the resource is rolled back and a 500 returned.
func (d *DB) mutate(name string, fn func() (*response, error)) (*response, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var snapshot any
	if d.persist {
		cur, _ := d.data.Get(name)
		snapshot = jsonx.Clone(cur)
	}
	resp, err := fn()
	if err != nil || !d.persist {
		return resp, err
	}
	out := append(jsonx.Marshal(d.data, "  "), '\n')
	if werr := WriteFileAtomic(d.path, out); werr != nil {
		d.data.Set(name, snapshot)
		return nil, httpx.Errorf(http.StatusInternalServerError, "change not saved: cannot write %s: %v", d.path, werr)
	}
	d.lastWritten = out
	return resp, nil
}

// Snapshot returns the current data encoded as indented JSON.
func (d *DB) Snapshot() []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return jsonx.Marshal(d.data, "  ")
}

// WriteFileAtomic writes data to a temp file in the same directory, syncs it
// and renames it over path, so readers see either the old or the new file.
// Symlinks are followed and the original permissions are kept.
func WriteFileAtomic(path string, data []byte) error {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	info, statErr := os.Stat(path)
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".mockbox-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func(e error) error {
		_ = f.Close()
		_ = os.Remove(tmp)
		return e
	}
	if _, err := f.Write(data); err != nil {
		return cleanup(err)
	}
	if err := f.Sync(); err != nil {
		return cleanup(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if statErr == nil {
		_ = os.Chmod(tmp, info.Mode().Perm())
	}
	// On Windows a rename over a file that another process has open briefly
	// fails with "Access is denied", so retry for a short while.
	var renameErr error
	for attempt := 0; attempt < 10; attempt++ {
		if renameErr = os.Rename(tmp, path); renameErr == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return renameErr
}
