package main

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"neutron/internal"
	"neutron/internal/model"
)

// newKillServer builds a Server with just the pieces killRunningJob needs: the
// handler's database half is deliberately out of scope here, since the test
// suite must never touch MySQL.
func newKillServer(jobs ...*batchv1.Job) *Server {
	cfg := model.Config{}
	cfg.Kubernetes.Namespace = "pipeline"
	objs := make([]runtime.Object, 0, len(jobs))
	for _, j := range jobs {
		objs = append(objs, j)
	}
	return &Server{config: cfg, clientSet: fake.NewSimpleClientset(objs...)}
}

func TestKillRunningJobDeletesJobAndRecordsFailure(t *testing.T) {
	const name = "neutron-orderservice-build-260917093012-3f2a"
	srv := newKillServer(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "pipeline"},
	})

	dbJob := &internal.PipelineJob{
		Name: name,
		Status: `{"active":1,"succeeded":0,"failed":0,"trigger_type":"PUSH",` +
			`"webhook_type":"GitLab","repo_url":"git@gitlab.example.com:g/p.git",` +
			`"source_url":"https://gitlab.example.com/g/p/-/tree/main"}`,
	}

	status, err := srv.killRunningJob(dbJob)
	if err != nil {
		t.Fatalf("killRunningJob returned an error: %v", err)
	}

	if status.Failed != 1 || status.Succeeded != 0 || status.Active != 0 || !status.Final {
		t.Errorf("status = %+v, want active=0 succeeded=0 failed=1 final=true", status)
	}
	if status.Description != killDescription(true) {
		t.Errorf("description = %q, want %q", status.Description, killDescription(true))
	}

	// Overwriting these with zero values would blank the status page's project
	// and source links, so they have to survive the kill.
	if status.WebhookType != "GitLab" || status.TriggerType != "PUSH" ||
		status.RepoUrl == "" || status.SourceUrl == "" {
		t.Errorf("descriptive fields were not carried over: %+v", status)
	}

	// Deleting the Job is what actually stops the Pods; recording a status alone
	// would leave them running and keep the run counting as a running sibling.
	if _, err := srv.clientSet.BatchV1().Jobs("pipeline").
		Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("Job still present (err = %v), want NotFound", err)
	}
}

// A Job that is already gone — TTL-cleaned, or deleted by hand — must not fail
// the kill: recording the outcome is the point of the endpoint.
func TestKillRunningJobToleratesMissingJob(t *testing.T) {
	srv := newKillServer()
	dbJob := &internal.PipelineJob{Name: "neutron-orderservice-build-260917000000-aaaa"}

	status, err := srv.killRunningJob(dbJob)
	if err != nil {
		t.Fatalf("a Job that is already gone must not fail: %v", err)
	}
	if status.Failed != 1 || !status.Final {
		t.Errorf("status = %+v, want it recorded as a terminal failure regardless", status)
	}
	if status.Description != killDescription(false) {
		t.Errorf("description = %q, want %q", status.Description, killDescription(false))
	}
}

func TestKillStatusHandlesEmptyAndInvalidStoredStatus(t *testing.T) {
	for _, stored := range []string{"", "not json", "{}", "null"} {
		got := killStatus(&internal.PipelineJob{Status: stored}, true)
		if got.Failed != 1 || got.Succeeded != 0 || got.Active != 0 || !got.Final {
			t.Errorf("killStatus(%q) = %+v, want a terminal failure", stored, got)
		}
	}
}

func TestKillDescriptionDistinguishesMissingJob(t *testing.T) {
	if killDescription(true) == "" || killDescription(false) == "" {
		t.Fatal("the description is shown in the completion notification and must not be empty")
	}
	if killDescription(true) == killDescription(false) {
		t.Error("the two outcomes must read differently, so a missing Job is distinguishable")
	}
}

// Guard against the route being dropped from registerRoutes while the handler
// stays in place — the button would then 404 with no compile-time signal.
func TestKillRouteRegistered(t *testing.T) {
	r := newRouterServer(t, model.Config{})
	for _, route := range r.Routes() {
		if route.Method == "POST" && route.Path == "/api/jobs/:jobName/kill" {
			return
		}
	}
	t.Error("POST /api/jobs/:jobName/kill is not registered")
}
