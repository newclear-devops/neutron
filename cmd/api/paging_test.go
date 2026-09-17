package main

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"neutron/internal/model"
)

func newQueryContext(t *testing.T, query string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/jobs?"+query, nil)
	return c
}

func TestParsePageParams(t *testing.T) {
	cases := []struct {
		query        string
		wantPage     int
		wantPageSize int
	}{
		{query: "", wantPage: 1, wantPageSize: 20},
		{query: "page=3", wantPage: 3, wantPageSize: 20},
		{query: "page=3&page_size=50", wantPage: 3, wantPageSize: 50},
		// A single "page" must not be able to ask for the whole table back.
		{query: "page_size=100000", wantPage: 1, wantPageSize: 100},
		// Junk falls back to the defaults rather than failing the request.
		{query: "page=abc&page_size=-5", wantPage: 1, wantPageSize: 20},
		{query: "page=0", wantPage: 1, wantPageSize: 20},
		// A deep page number is clamped too: past this the OFFSET is pure
		// scanning, and (page-1)*pageSize would otherwise overflow.
		{query: "page=100000", wantPage: 1000, wantPageSize: 20},
		{query: "page=2147483647&page_size=100", wantPage: 1000, wantPageSize: 100},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			page, pageSize := parsePageParams(newQueryContext(t, tc.query))
			if page != tc.wantPage || pageSize != tc.wantPageSize {
				t.Errorf("parsePageParams(%q) = (%d, %d), want (%d, %d)",
					tc.query, page, pageSize, tc.wantPage, tc.wantPageSize)
			}
		})
	}
}

// The project page's "Show all history" toggle is the only caller allowed to
// lift the recency window; every other listing must keep the default.
func TestJobWindowDays(t *testing.T) {
	cases := []struct {
		query string
		want  int
	}{
		{query: "", want: defaultJobWindowDays},
		{query: "page=2&job_name=build", want: defaultJobWindowDays},
		{query: "all=1", want: 0},
		// Only the documented value opts out; anything else keeps the window.
		{query: "all=0", want: defaultJobWindowDays},
		{query: "all=true", want: defaultJobWindowDays},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			if got := jobWindowDays(newQueryContext(t, tc.query)); got != tc.want {
				t.Errorf("jobWindowDays(%q) = %d, want %d", tc.query, got, tc.want)
			}
		})
	}
}

func TestParamsFromSpec(t *testing.T) {
	spec := model.JobSpec{
		JobName:     "build",
		CommitSha:   "abc123",
		Trigger:     "PUSH",
		CodeRef:     "main",
		SourceUrl:   "https://gitlab.example.com/g/p/-/tree/main",
		QueryParams: map[string]string{"DEPLOY_ENV": "prod"},
	}

	p := paramsFromSpec(spec)
	if p.Ref != "main" {
		t.Errorf("ref = %q, want main", p.Ref)
	}
	if p.CommitSha != "abc123" {
		t.Errorf("commit_sha = %q, want abc123", p.CommitSha)
	}
	if p.Trigger != "PUSH" {
		t.Errorf("trigger = %q, want PUSH", p.Trigger)
	}
	if p.Env["DEPLOY_ENV"] != "prod" {
		t.Errorf("env = %v, want DEPLOY_ENV=prod", p.Env)
	}

	// MR runs carry no code ref; the commit is then the most useful thing to show.
	mr := spec
	mr.CodeRef = ""
	if got := paramsFromSpec(mr).Ref; got != "abc123" {
		t.Errorf("ref = %q, want the commit sha when no code ref is set", got)
	}
}

func TestMarshalParseParams(t *testing.T) {
	in := model.JobParams{Ref: "v1.2.3", Trigger: "API", Env: map[string]string{"IMAGE_TAG": "v1.2.3"}}

	got := parseParams(marshalParams(in))
	if got == nil {
		t.Fatal("parseParams returned nil for a marshalled value")
	}
	if got.Ref != in.Ref || got.Trigger != in.Trigger || got.Env["IMAGE_TAG"] != "v1.2.3" {
		t.Errorf("round trip = %+v, want %+v", got, in)
	}

	if parseParams("") != nil {
		t.Error("empty params should parse to nil, not an empty struct")
	}
	if parseParams("not json") != nil {
		t.Error("invalid params should parse to nil")
	}
}
