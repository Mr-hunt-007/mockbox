package rewrite

import (
	"strings"
	"testing"
)

func TestRewrite(t *testing.T) {
	rules, err := Parse([]byte(`{
  "/api/v1/*": "/$1",
  "/blog/:resource/:id/show": "/:resource/:id",
  "/me": "/users/1",
  "/by-author/:name": "/posts?author=:name",
  "/files/*.json": "/files/$1"
}`))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		path, query     string
		wantPath, wantQ string
		wantMatched     bool
	}{
		{"/api/v1/users", "role=admin", "/users", "role=admin", true},
		{"/api/v1/users/1/posts", "", "/users/1/posts", "", true},
		{"/api/v1/", "", "/", "", true},
		{"/blog/posts/3/show", "", "/posts/3", "", true},
		{"/blog/posts/3", "", "/blog/posts/3", "", false},
		{"/me", "_embed=posts", "/users/1", "_embed=posts", true},
		{"/me/", "", "/me/", "", false},
		{"/by-author/ada", "_limit=1", "/posts", "_limit=1&author=ada", true},
		{"/files/a/b.json", "", "/files/a/b", "", true},
		{"/other", "x=1", "/other", "x=1", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			p, q, ok := Rewrite(rules, tt.path, tt.query)
			if p != tt.wantPath || q != tt.wantQ || ok != tt.wantMatched {
				t.Errorf("got (%q, %q, %v) want (%q, %q, %v)", p, q, ok, tt.wantPath, tt.wantQ, tt.wantMatched)
			}
		})
	}
}

func TestFirstRuleWins(t *testing.T) {
	rules, err := Parse([]byte(`{"/a/*": "/first/$1", "/a/b": "/second"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p, _, _ := Rewrite(rules, "/a/b", ""); p != "/first/b" {
		t.Fatalf("got %s", p)
	}
}

func TestParseErrors(t *testing.T) {
	tests := map[string]string{
		`[]`:            "must be a JSON object",
		`{"/a": 1}`:     "target must be a string",
		`{"a": "/b"}`:   "must start with /",
		`{"/a": "b"}`:   "must start with /",
		`{"/a": "/b",}`: "invalid character",
	}
	for in, want := range tests {
		_, err := Parse([]byte(in))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v want %q", in, err, want)
		}
	}
}
