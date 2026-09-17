package db

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/Mr-hunt-007/mockbox/internal/jsonx"
)

const dbFixture = `{
  "users": [
    {
      "id": 1,
      "name": "Ada",
      "role": "admin"
    },
    {
      "id": 2,
      "name": "Alan",
      "role": "user"
    }
  ],
  "posts": [
    {
      "id": 1,
      "title": "Engines",
      "userId": 1
    },
    {
      "id": 2,
      "title": "Machines",
      "userId": 2
    },
    {
      "id": 3,
      "title": "Numbers",
      "userId": 1
    }
  ],
  "tags": [
    {
      "id": "a1",
      "label": "go"
    }
  ],
  "empty": [],
  "profile": {
    "name": "demo",
    "theme": "dark"
  },
  "version": 3
}
`

func newTestDB(t *testing.T, persist bool) (*DB, string, *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db.json")
	if err := os.WriteFile(path, []byte(dbFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := jsonx.Parse([]byte(dbFixture))
	if err != nil {
		t.Fatal(err)
	}
	obj, warnings, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"version"`) {
		t.Fatalf("expected one warning about version, got %v", warnings)
	}
	d := New(path, obj, persist)
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return d, path, srv
}

type result struct {
	status int
	header http.Header
	body   string
}

func do(t *testing.T, srv *httptest.Server, method, path, body string) result {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

func compact(t *testing.T, s string) string {
	t.Helper()
	v, err := jsonx.Parse([]byte(s))
	if err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, s)
	}
	return string(jsonx.Marshal(v, ""))
}

func TestCollectionRead(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	tests := []struct {
		path   string
		status int
		body   string
	}{
		{"/users", 200, `[{"id":1,"name":"Ada","role":"admin"},{"id":2,"name":"Alan","role":"user"}]`},
		{"/users/", 200, `[{"id":1,"name":"Ada","role":"admin"},{"id":2,"name":"Alan","role":"user"}]`},
		{"/users?role=user", 200, `[{"id":2,"name":"Alan","role":"user"}]`},
		{"/users/2", 200, `{"id":2,"name":"Alan","role":"user"}`},
		{"/tags/a1", 200, `{"id":"a1","label":"go"}`},
		{"/users/1/posts", 200, `[{"id":1,"title":"Engines","userId":1},{"id":3,"title":"Numbers","userId":1}]`},
		{"/users/1/posts?_sort=-id&_limit=1", 200, `[{"id":3,"title":"Numbers","userId":1}]`},
		{"/users/1?_embed=posts", 200, `{"id":1,"name":"Ada","role":"admin","posts":[{"id":1,"title":"Engines","userId":1},{"id":3,"title":"Numbers","userId":1}]}`},
		{"/posts?_expand=user&userId=2", 200, `[{"id":2,"title":"Machines","userId":2,"user":{"id":2,"name":"Alan","role":"user"}}]`},
		{"/profile", 200, `{"name":"demo","theme":"dark"}`},
		{"/empty", 200, `[]`},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			r := do(t, srv, "GET", tt.path, "")
			if r.status != tt.status {
				t.Fatalf("status %d want %d: %s", r.status, tt.status, r.body)
			}
			if got := compact(t, r.body); got != tt.body {
				t.Fatalf("body\n got %s\nwant %s", got, tt.body)
			}
			if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("content type %q", ct)
			}
		})
	}
}

func TestEmbedDoesNotModifyStoredData(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	do(t, srv, "GET", "/users?_embed=posts", "")
	r := do(t, srv, "GET", "/users/1", "")
	if strings.Contains(r.body, "posts") {
		t.Fatalf("embedded data leaked into the store: %s", r.body)
	}
}

func TestErrors(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	tests := []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"GET", "/nope", "", 404, "no route for GET /nope"},
		{"GET", "/version", "", 404, "no route"},
		{"GET", "/users/99", "", 404, `users with id \"99\" not found`},
		{"GET", "/users/99/posts", "", 404, "not found"},
		{"GET", "/profile/1", "", 404, "no route"},
		{"GET", "/a/b/c/d", "", 404, "no route"},
		{"GET", "/users?_page=0", "", 400, "_page must be a positive integer"},
		{"GET", "/users?name_like=(", "", 400, "invalid regular expression"},
		{"GET", "/users?_embed=nothing", "", 400, "_embed=nothing"},
		{"GET", "/posts?_expand=nothing", "", 400, "_expand=nothing"},
		{"POST", "/users", `{"name": "x",}`, 400, "invalid JSON body: invalid character '}'"},
		{"POST", "/users", "", 400, "request body is empty"},
		{"POST", "/users", `[1,2]`, 400, "must be a JSON object, got an array"},
		{"POST", "/users", `{"id": {"x": 1}}`, 400, "id must be a string or number"},
		{"POST", "/users", `{"id": 2, "name": "dup"}`, 409, "users with id 2 already exists"},
		{"POST", "/users", `{"id": "2"}`, 409, "already exists"},
		{"POST", "/users/1", `{}`, 405, "not allowed"},
		{"DELETE", "/users", "", 405, "not allowed"},
		{"POST", "/profile", `{}`, 405, "not allowed"},
		{"DELETE", "/profile", "", 405, "not allowed"},
		{"PUT", "/users/99", `{}`, 404, "not found"},
		{"PATCH", "/users/1", `nope`, 400, "invalid JSON body"},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path+" "+tt.body, func(t *testing.T) {
			r := do(t, srv, tt.method, tt.path, tt.body)
			if r.status != tt.status {
				t.Fatalf("status %d want %d: %s", r.status, tt.status, r.body)
			}
			if !strings.Contains(r.body, tt.contains) {
				t.Fatalf("body %s does not contain %q", r.body, tt.contains)
			}
			var shape struct {
				Error   string `json:"error"`
				Status  int    `json:"status"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(r.body), &shape); err != nil || shape.Status != tt.status || shape.Error == "" {
				t.Fatalf("error shape wrong: %s", r.body)
			}
			if tt.status == 405 && r.header.Get("Allow") == "" {
				t.Fatal("405 without Allow header")
			}
		})
	}
}

func TestCreateIDs(t *testing.T) {
	d, _, srv := newTestDB(t, false)
	r := do(t, srv, "POST", "/users", `{"name": "Grace"}`)
	if r.status != 201 || compact(t, r.body) != `{"id":3,"name":"Grace"}` {
		t.Fatalf("numeric next id: %d %s", r.status, r.body)
	}
	if loc := r.header.Get("Location"); loc != "/users/3" {
		t.Fatalf("Location %q", loc)
	}
	r = do(t, srv, "POST", "/empty", `{"x": 1}`)
	if compact(t, r.body) != `{"id":1,"x":1}` {
		t.Fatalf("empty collection id: %s", r.body)
	}
	d.NewID = func() string { return "fixed" }
	r = do(t, srv, "POST", "/tags", `{"label": "rust"}`)
	if r.status != 201 || compact(t, r.body) != `{"id":"fixed","label":"rust"}` {
		t.Fatalf("string id: %s", r.body)
	}
	r = do(t, srv, "POST", "/users", `{"id": 10, "name": "Explicit"}`)
	if r.status != 201 {
		t.Fatalf("explicit id: %d", r.status)
	}
	r = do(t, srv, "POST", "/users", `{"name": "Next"}`)
	if compact(t, r.body) != `{"id":11,"name":"Next"}` {
		t.Fatalf("id after explicit 10: %s", r.body)
	}
	if got := do(t, srv, "GET", "/users", "").header.Get("X-Total-Count"); got != "5" {
		t.Fatalf("X-Total-Count %q", got)
	}
}

func TestRandomIDAvoidsCollisions(t *testing.T) {
	d, _, srv := newTestDB(t, false)
	calls := 0
	d.NewID = func() string {
		calls++
		if calls == 1 {
			return "a1" // already taken
		}
		return "b2"
	}
	r := do(t, srv, "POST", "/tags", `{}`)
	if compact(t, r.body) != `{"id":"b2"}` {
		t.Fatalf("got %s", r.body)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	r := do(t, srv, "PUT", "/users/1", `{"id": 999, "name": "Replaced"}`)
	if r.status != 200 || compact(t, r.body) != `{"id":1,"name":"Replaced"}` {
		t.Fatalf("PUT: %d %s", r.status, r.body)
	}
	r = do(t, srv, "PATCH", "/users/2", `{"role": "admin", "id": 5, "email": "a@b.c"}`)
	if compact(t, r.body) != `{"id":2,"name":"Alan","role":"admin","email":"a@b.c"}` {
		t.Fatalf("PATCH: %s", r.body)
	}
	r = do(t, srv, "PATCH", "/profile", `{"theme": "light", "lang": "en"}`)
	if compact(t, r.body) != `{"name":"demo","theme":"light","lang":"en"}` {
		t.Fatalf("PATCH singular: %s", r.body)
	}
	r = do(t, srv, "PUT", "/profile", `{"only": true}`)
	if compact(t, r.body) != `{"only":true}` {
		t.Fatalf("PUT singular: %s", r.body)
	}
	r = do(t, srv, "DELETE", "/users/1", "")
	if r.status != 200 || compact(t, r.body) != `{"id":1,"name":"Replaced"}` {
		t.Fatalf("DELETE: %d %s", r.status, r.body)
	}
	if r := do(t, srv, "GET", "/users/1", ""); r.status != 404 {
		t.Fatalf("deleted item still there: %d", r.status)
	}
	if r := do(t, srv, "DELETE", "/users/1", ""); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
}

func TestNestedPost(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	r := do(t, srv, "POST", "/users/2/posts", `{"title": "New", "userId": 1}`)
	if r.status != 201 || compact(t, r.body) != `{"id":4,"title":"New","userId":2}` {
		t.Fatalf("nested POST: %d %s", r.status, r.body)
	}
	if loc := r.header.Get("Location"); loc != "/posts/4" {
		t.Fatalf("Location %q", loc)
	}
	if r := do(t, srv, "POST", "/users/99/posts", `{}`); r.status != 404 {
		t.Fatalf("nested POST to missing parent: %d", r.status)
	}
}

func TestPaginationHeaders(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	r := do(t, srv, "GET", "/posts?_page=2&_limit=2", "")
	if r.header.Get("X-Total-Count") != "3" {
		t.Fatalf("X-Total-Count %q", r.header.Get("X-Total-Count"))
	}
	link := r.header.Get("Link")
	host := strings.TrimPrefix(srv.URL, "http://")
	for _, want := range []string{
		fmt.Sprintf(`<http://%s/posts?_limit=2&_page=1>; rel="first"`, host),
		fmt.Sprintf(`<http://%s/posts?_limit=2&_page=1>; rel="prev"`, host),
		fmt.Sprintf(`<http://%s/posts?_limit=2&_page=2>; rel="last"`, host),
	} {
		if !strings.Contains(link, want) {
			t.Errorf("Link %s missing %s", link, want)
		}
	}
	if strings.Contains(link, `rel="next"`) {
		t.Errorf("last page must not link next: %s", link)
	}
	if compact(t, r.body) != `[{"id":3,"title":"Numbers","userId":1}]` {
		t.Fatalf("page body %s", r.body)
	}
}

func TestHeadRequest(t *testing.T) {
	_, _, srv := newTestDB(t, false)
	r := do(t, srv, "HEAD", "/users", "")
	if r.status != 200 || r.body != "" || r.header.Get("X-Total-Count") != "2" {
		t.Fatalf("HEAD: %d %q %v", r.status, r.body, r.header)
	}
}

func TestRoutes(t *testing.T) {
	d, _, _ := newTestDB(t, false)
	var got []string
	for _, r := range d.Routes(false) {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{
		"GET /users", "GET /users/:id", "GET /users/:id/posts", "POST /users", "PUT /users/:id", "PATCH /users/:id", "DELETE /users/:id",
		"GET /posts", "GET /posts/:id", "POST /posts", "PUT /posts/:id", "PATCH /posts/:id", "DELETE /posts/:id",
		"GET /tags", "GET /tags/:id", "POST /tags", "PUT /tags/:id", "PATCH /tags/:id", "DELETE /tags/:id",
		"GET /empty", "GET /empty/:id", "POST /empty", "PUT /empty/:id", "PATCH /empty/:id", "DELETE /empty/:id",
		"GET /profile", "PUT /profile", "PATCH /profile",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("routes:\n%s", strings.Join(got, "\n"))
	}
	for _, r := range d.Routes(true) {
		if r.Method != "GET" {
			t.Fatalf("readonly route table contains %s %s", r.Method, r.Path)
		}
	}
}

func TestParseRejectsNonObject(t *testing.T) {
	for _, in := range []string{`[]`, `"x"`, `3`} {
		doc, _ := jsonx.Parse([]byte(in))
		if _, _, err := Parse(doc); err == nil || !strings.Contains(err.Error(), "top level must be a JSON object") {
			t.Errorf("%s: %v", in, err)
		}
	}
	doc, _ := jsonx.Parse([]byte(`{"openapi": "3.0.0"}`))
	if _, _, err := Parse(doc); err != ErrNotDatabase {
		t.Errorf("openapi doc: %v", err)
	}
}

func TestInMemoryNeverTouchesFile(t *testing.T) {
	_, path, srv := newTestDB(t, false)
	do(t, srv, "POST", "/users", `{"name": "x"}`)
	do(t, srv, "DELETE", "/posts/1", "")
	raw, _ := os.ReadFile(path)
	if string(raw) != dbFixture {
		t.Fatal("file changed without --persist")
	}
}

func listTempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestPersistWritesFileInOrderWithTwoSpaces(t *testing.T) {
	_, path, srv := newTestDB(t, true)
	r := do(t, srv, "PATCH", "/users/2", `{"role": "admin"}`)
	if r.status != 200 {
		t.Fatalf("PATCH: %d %s", r.status, r.body)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(dbFixture, `"name": "Alan",
      "role": "user"`, `"name": "Alan",
      "role": "admin"`, 1)
	if string(raw) != want {
		t.Fatalf("persisted file differs from expected.\n got:\n%s\nwant:\n%s", raw, want)
	}
	if tmps := listTempFiles(t, filepath.Dir(path)); len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}
}

func TestPersistKeepsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits do not apply on Windows")
	}
	_, path, srv := newTestDB(t, true)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	do(t, srv, "POST", "/users", `{"name": "x"}`)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
}

func TestPersistFollowsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(real, []byte(`{"a": []}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink not supported:", err)
	}
	if err := WriteFileAtomic(link, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink was replaced by a regular file")
	}
	if raw, _ := os.ReadFile(real); string(raw) != "{}\n" {
		t.Fatalf("target not updated: %q", raw)
	}
}

func TestPersistFailureRollsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("read-only directories do not block file creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	_, path, srv := newTestDB(t, true)
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	r := do(t, srv, "POST", "/users", `{"name": "lost"}`)
	if r.status != 500 || !strings.Contains(r.body, "change not saved") {
		t.Fatalf("expected 500, got %d %s", r.status, r.body)
	}
	if r := do(t, srv, "GET", "/users", ""); r.header.Get("X-Total-Count") != "2" {
		t.Fatalf("memory not rolled back: %s", r.body)
	}
	if raw, _ := os.ReadFile(path); string(raw) != dbFixture {
		t.Fatal("file changed despite failed write")
	}
}

func TestPersistConcurrentWritesStayValid(t *testing.T) {
	_, path, srv := newTestDB(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				do(t, srv, "POST", "/posts", fmt.Sprintf(`{"title": "t%d", "userId": 1}`, i))
			case 1:
				do(t, srv, "PATCH", "/profile", fmt.Sprintf(`{"n": %d}`, i))
			case 2:
				do(t, srv, "GET", "/posts?_expand=user", "")
			case 3:
				raw, err := os.ReadFile(path)
				if err == nil {
					if _, perr := jsonx.Parse(raw); perr != nil {
						t.Errorf("reader saw a partial file: %v", perr)
					}
				}
			}
		}(i)
	}
	wg.Wait()
	raw, _ := os.ReadFile(path)
	doc, err := jsonx.Parse(raw)
	if err != nil {
		t.Fatalf("final file invalid: %v", err)
	}
	posts, _ := doc.(*jsonx.Object).Get("posts")
	if n := len(posts.([]any)); n != 13 {
		t.Fatalf("expected 13 posts on disk, got %d", n)
	}
	var seen = map[string]bool{}
	for _, p := range posts.([]any) {
		id, _ := p.(*jsonx.Object).Get("id")
		s, _ := ScalarString(id)
		if seen[s] {
			t.Fatalf("duplicate id %s", s)
		}
		seen[s] = true
	}
	if tmps := listTempFiles(t, filepath.Dir(path)); len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}
}

func TestReloadFile(t *testing.T) {
	d, path, srv := newTestDB(t, true)
	do(t, srv, "POST", "/users", `{"name": "persisted"}`)
	changed, _, err := d.ReloadFile()
	if err != nil || changed {
		t.Fatalf("reloading our own write should be a no-op: changed=%v err=%v", changed, err)
	}
	if err := os.WriteFile(path, []byte(`{"users": [{"id": 7}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, _, err = d.ReloadFile()
	if err != nil || !changed {
		t.Fatalf("external edit: changed=%v err=%v", changed, err)
	}
	if r := do(t, srv, "GET", "/users", ""); compact(t, r.body) != `[{"id":7}]` {
		t.Fatalf("after reload: %s", r.body)
	}
	if err := os.WriteFile(path, []byte(`{"users": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.ReloadFile(); err == nil {
		t.Fatal("broken file should fail to reload")
	}
	if r := do(t, srv, "GET", "/users", ""); compact(t, r.body) != `[{"id":7}]` {
		t.Fatalf("broken edit replaced good data: %s", r.body)
	}
}

func TestResourcesAndRelations(t *testing.T) {
	doc, err := jsonx.Parse([]byte(`{
	  "users": [{"id": 1}],
	  "categories": [{"id": "a"}],
	  "posts": [{"id": 1, "userId": 1, "categoryId": "a"}],
	  "comments": [{"id": 1, "postId": 1}, {"id": 2}],
	  "tags": [],
	  "profile": {"name": "x"},
	  "version": 3
	}`))
	if err != nil {
		t.Fatal(err)
	}
	obj, _, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	d := New("", obj, false)
	colls, singular := d.Resources()
	if got := fmt.Sprint(colls, singular); got != "[{users 1} {categories 1} {posts 1} {comments 2} {tags 0}] [profile]" {
		t.Fatalf("resources: %s", got)
	}
	var got []string
	for _, r := range d.Relations() {
		got = append(got, r.Parent+">"+r.Child+":"+r.ForeignKey+":"+r.Expand)
	}
	want := "users>posts:userId:user categories>posts:categoryId:category posts>comments:postId:post"
	if strings.Join(got, " ") != want {
		t.Fatalf("relations:\n got %s\nwant %s", strings.Join(got, " "), want)
	}
	// Every relation the tool reports must actually work on the server.
	srv := httptest.NewServer(d)
	defer srv.Close()
	for _, r := range d.Relations() {
		for _, path := range []string{"/" + r.Parent + "?_embed=" + r.Child, "/" + r.Child + "?_expand=" + r.Expand} {
			res := do(t, srv, "GET", path, "")
			if res.status != 200 {
				t.Errorf("%s: %d %s", path, res.status, res.body)
			}
		}
	}
}
