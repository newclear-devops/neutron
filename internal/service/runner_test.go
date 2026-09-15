package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"neutron/internal/model"
)

type stubReporter struct{}

func (stubReporter) Report(jobName, stepName string, status model.StepResult, description string) {}
func (stubReporter) ReportJobFinal(status model.StepResult, description string)                   {}

const defaultPipelineYaml = "jobs:\n" +
	"  build:\n" +
	"    image: alpine\n" +
	"    trigger:\n" +
	"      - PUSH\n" +
	"    steps:\n" +
	"      - name: hello\n" +
	"        cmd: echo hello\n"

// serveDefaultPipeline starts an API stub answering GET /api/default-pipeline.
func serveDefaultPipeline(t *testing.T, content string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/default-pipeline" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body, err := json.Marshal(map[string]string{"content": content})
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A repo neutron.yaml with no `jobs` key at all (empty file, or a different
// top-level shape) unmarshals to a nil Jobs map. Resolving the job from the
// default pipeline then writes into that nil map, which panics.
func TestNewRunnerEmptyRepoYamlFallsBackToDefaultJob(t *testing.T) {
	srv := serveDefaultPipeline(t, defaultPipelineYaml)

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "neutron.yaml"), []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRunner(dir, "PUSH", "build", stubReporter{}, srv.URL, true)
	if r == nil {
		t.Fatal("NewRunner returned nil")
	}
	if len(r.Steps) != 1 || r.Steps[0].Command != "echo hello" {
		t.Fatalf("steps = %+v, want one step running 'echo hello'", r.Steps)
	}
}

// Same fallback, but the repo file defines other jobs — the map is non-nil, so
// the resolved job is simply added alongside them.
func TestNewRunnerDefaultJobAddedToExistingRepoJobs(t *testing.T) {
	srv := serveDefaultPipeline(t, defaultPipelineYaml)

	dir := t.TempDir()
	repoYaml := "jobs:\n  lint:\n    image: alpine\n    steps:\n      - name: lint\n        cmd: echo lint\n"
	if err := os.WriteFile(filepath.Join(dir, "neutron.yaml"), []byte(repoYaml), 0o600); err != nil {
		t.Fatal(err)
	}

	r := NewRunner(dir, "PUSH", "build", stubReporter{}, srv.URL, true)
	if r == nil {
		t.Fatal("NewRunner returned nil")
	}
	if len(r.Steps) != 1 || r.Steps[0].Command != "echo hello" {
		t.Fatalf("steps = %+v, want the default pipeline's step", r.Steps)
	}
}
