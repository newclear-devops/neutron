package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"

	"neutron/internal/model"
)

// newRouterServer wires the real routes onto an engine. The manual-trigger
// lookups are the only endpoints under test that run without a database, so
// the Server is built with a nil repository on purpose: it also proves those
// handlers never reach for it.
func newRouterServer(t *testing.T, cfg model.Config) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	srv := &Server{config: cfg}
	srv.registerRoutes(r)
	return r
}

func doGet(t *testing.T, r *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestRefListURLEscapesSSHURLAsOneSegment(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		sshURL string
		kind   string
		want   string
	}{
		{
			name:   "scp-like ssh url",
			base:   "http://gateway.local",
			sshURL: "git@gitlab.example.com:group/proj.git",
			kind:   refKindBranches,
			// The "@" and ":" stay literal; the "/" separates segments and so
			// must be escaped, otherwise the gateway would read the org as its
			// own path segment.
			want: "http://gateway.local/api/v1/projects/git@gitlab.example.com:group%2Fproj.git/repository/branches",
		},
		{
			name:   "ssh scheme url and tags",
			base:   "http://gateway.local/", // trailing slash must not double up
			sshURL: "ssh://git@codeup.example.com:9022/org/SZ/PublicService/repo.git",
			kind:   refKindTags,
			want:   "http://gateway.local/api/v1/projects/ssh:%2F%2Fgit@codeup.example.com:9022%2Forg%2FSZ%2FPublicService%2Frepo.git/repository/tags",
		},
		{
			name:   "missing base is unusable",
			base:   "",
			sshURL: "git@gitlab.example.com:group/proj.git",
			kind:   refKindBranches,
			want:   "",
		},
		{
			name:   "missing repo url is unusable",
			base:   "http://gateway.local",
			sshURL: "",
			kind:   refKindBranches,
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := refListURL(tc.base, tc.sshURL, tc.kind); got != tc.want {
				t.Errorf("refListURL() =\n  %q\nwant:\n  %q", got, tc.want)
			}
		})
	}
}

// The gateway fronts GitLab and Codeup, whose payload shapes differ. All of
// them have to land in the same {name, default, commit} list.
func TestParseRefItems(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []refItem
	}{
		{
			name: "bare array of gitlab-style objects",
			body: `[{"name":"main","default":true,"commit":{"short_id":"a1b2c3d","title":"init"}},
			        {"name":"release/1.0","commit":{"short_id":"e4f5a6b","title":"bump"}}]`,
			want: []refItem{
				{Name: "main", Default: true, Commit: "a1b2c3d"},
				{Name: "release/1.0", Commit: "e4f5a6b"},
			},
		},
		{
			name: "bare array of plain names",
			body: `["main","develop"]`,
			want: []refItem{{Name: "main"}, {Name: "develop"}},
		},
		{
			name: "data envelope",
			body: `{"data":[{"name":"v1.0.0"}]}`,
			want: []refItem{{Name: "v1.0.0"}},
		},
		{
			name: "branches envelope with platform-specific key",
			body: `{"branches":[{"branch":"main","isDefault":true}]}`,
			want: []refItem{{Name: "main", Default: true}},
		},
		{
			name: "tags envelope with tag key",
			body: `{"tags":[{"tag":"v2.0.0"}]}`,
			want: []refItem{{Name: "v2.0.0"}},
		},
		{
			name: "entries without a usable name are dropped",
			body: `[{"commit":{"short_id":"abc"}},{"name":""},"main"]`,
			want: []refItem{{Name: "main"}},
		},
		{
			name: "empty list",
			body: `[]`,
			want: []refItem{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRefItems([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseRefItems returned an error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseRefItems got %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("item %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseRefItemsRejectsNonJSON(t *testing.T) {
	if _, err := parseRefItems([]byte("<html>gateway error</html>")); err == nil {
		t.Error("an HTML error page should not parse as a ref list")
	}
}

func TestParseDependencyBranches(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "objects with names",
			body: `{"data":[{"id":1,"name":"master202609","baseBranch":"master","status":"active"},{"id":2,"name":"master202608"}]}`,
			want: []string{"master202609", "master202608"},
		},
		{
			name: "bare strings",
			body: `["master202609"]`,
			want: []string{"master202609"},
		},
		{
			name: "branch key",
			body: `[{"branch":"master202607"}]`,
			want: []string{"master202607"},
		},
		{
			name: "not json yields nothing rather than an error",
			body: `not json`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseDependencyBranches([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("parseDependencyBranches got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("branch %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseDepBranch(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "key present", body: "depBranch=master202609\nother=value\n", want: "master202609"},
		{name: "surrounded by blank lines", body: "\n\ndepBranch= master202609 \n", want: "master202609"},
		{name: "key absent", body: "other=value\n", want: ""},
		{name: "empty body", body: "", want: ""},
		// A key that merely starts with depBranch must not match.
		{name: "no prefix false positive", body: "depBranchName=master202609\n", want: ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDepBranch([]byte(tc.body)); got != tc.want {
				t.Errorf("parseDepBranch(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// Exercises the dependency endpoints through the real router, with an upstream
// stub standing in for the dependency service.
func TestDependencyEndpoints(t *testing.T) {
	// The stub only answers the exact expected paths, so a wrong upstream URL
	// surfaces as a 502 instead of quietly succeeding.
	var mu sync.Mutex
	var lastPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		lastPath = r.URL.Path
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/branches":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"name":"master202609"},{"name":"master202608"}]}`))
		case "/property":
			_, _ = w.Write([]byte("depBranch=master202609\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	upstreamPath := func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastPath
	}

	cfg := model.Config{}
	cfg.Dependency.Url = upstream.URL + "/" // trailing slash must be tolerated
	r := newRouterServer(t, cfg)

	w := doGet(t, r, "/api/dependency/branches")
	if w.Code != http.StatusOK {
		t.Fatalf("branches status = %d, body %s", w.Code, w.Body.String())
	}
	var branches struct {
		Branches []string `json:"branches"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &branches); err != nil {
		t.Fatalf("branches body is not JSON: %v (%s)", err, w.Body.String())
	}
	if len(branches.Branches) != 2 || branches.Branches[0] != "master202609" {
		t.Errorf("branches = %v, want [master202609 master202608]", branches.Branches)
	}
	if got := upstreamPath(); got != "/api/v1/branches" {
		t.Errorf("upstream path = %q, want /api/v1/branches", got)
	}

	w = doGet(t, r, "/api/dependency/default")
	if w.Code != http.StatusOK {
		t.Fatalf("default status = %d, body %s", w.Code, w.Body.String())
	}
	var def struct {
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &def); err != nil {
		t.Fatalf("default body is not JSON: %v", err)
	}
	if def.Branch != "master202609" {
		t.Errorf("default branch = %q, want master202609", def.Branch)
	}
	if got := upstreamPath(); got != "/property" {
		t.Errorf("upstream path = %q, want /property", got)
	}
}

// An upstream failure must surface as a gateway error, not as an empty list the
// toolbar would silently render as "no branches".
func TestDependencyEndpointUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	cfg := model.Config{}
	cfg.Dependency.Url = upstream.URL
	r := newRouterServer(t, cfg)

	for _, target := range []string{"/api/dependency/branches", "/api/dependency/default"} {
		w := doGet(t, r, target)
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s status = %d, want 502", target, w.Code)
		}
		if !strings.Contains(w.Body.String(), "500") {
			t.Errorf("%s body = %s, want it to mention the upstream status", target, w.Body.String())
		}
	}
}

// With no dependency service configured the toolbar should be told so
// explicitly, rather than being handed an empty branch list.
func TestDependencyEndpointsNotConfigured(t *testing.T) {
	r := newRouterServer(t, model.Config{})

	for _, target := range []string{"/api/dependency/branches", "/api/dependency/default"} {
		w := doGet(t, r, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, w.Code)
		}
		if !strings.Contains(w.Body.String(), "NEUTRON_DEPENDENCY_URL") {
			t.Errorf("%s body = %s, want it to name the env var to set", target, w.Body.String())
		}
	}
}

// The gateway check has to happen before the project lookup: with no gateway
// configured the handler must answer 400 without touching the (here nil)
// repository.
func TestProjectRefsNotConfigured(t *testing.T) {
	r := newRouterServer(t, model.Config{})

	for _, target := range []string{"/api/projects/p1/branches", "/api/projects/p1/tags"} {
		w := doGet(t, r, target)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", target, w.Code)
		}
		if !strings.Contains(w.Body.String(), "NEUTRON_GITREPO_GATEWAY_URL") {
			t.Errorf("%s body = %s, want it to name the env var to set", target, w.Body.String())
		}
	}
}
