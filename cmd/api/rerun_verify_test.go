package main

import (
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"neutron/internal/model"
)

// TestLauncherFromSpecRebuild verifies that the production manifest-construction
// path (Server.launcherFromSpec → buildLauncher → launcher.CreateJob) rebuilds a
// K8s Job carrying every input persisted in a JobSpec: annotations, env vars
// (including webhook query params), and a checkout pinned to the exact commit.
func TestLauncherFromSpecRebuild(t *testing.T) {
	cfg := model.Config{Host: "http://neutron.local"}
	cfg.Kubernetes.Namespace = "default"
	cfg.BaseConfig = map[string]model.CodeBase{
		"GitLab": {Url: "https://gitlab.example.com", Token: "tok", SkipTLSVerify: true},
	}
	srv := &Server{config: cfg, clientSet: fake.NewSimpleClientset()}

	spec := model.JobSpec{
		Platform:    "GitLab",
		JobName:     "build",
		Image:       "maven:3.9",
		ProjectId:   "101",
		CommitSha:   "abc123def",
		ReportSha:   "abc123def",
		Trigger:     "PUSH",
		GitRepoUrl:  "git@gitlab.example.com:backend/order-service.git",
		CodeRef:     "main",
		SourceUrl:   "https://gitlab.example.com/backend/order-service/-/tree/main",
		QueryParams: map[string]string{"DEPLOY_ENV": "prod"},
	}

	job := srv.launcherFromSpec(spec).CreateJob(srv.config.Host)

	// The repo-derived project segment comes first: spec.GitRepoUrl is
	// git@gitlab.example.com:backend/order-service.git → "orderservice".
	if !strings.HasPrefix(job.Name, "neutron-orderservice-build-") {
		t.Errorf("job name = %q, want neutron-orderservice-build-*", job.Name)
	}
	if got := job.Annotations["triggerType"]; got != "PUSH" {
		t.Errorf("annotation triggerType = %q, want PUSH", got)
	}
	if got := job.Annotations["sourceUrl"]; got != spec.SourceUrl {
		t.Errorf("annotation sourceUrl = %q, want %q", got, spec.SourceUrl)
	}
	if got := job.Annotations["gitPath"]; got != spec.GitRepoUrl {
		t.Errorf("annotation gitPath = %q, want %q", got, spec.GitRepoUrl)
	}
	if got := job.Spec.Template.Spec.Containers[0].Image; got != spec.Image {
		t.Errorf("image = %q, want %q", got, spec.Image)
	}

	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for k, want := range map[string]string{
		"COMMIT_SHA": "abc123def", "REPORT_SHA": "abc123def", "TRIGGER": "PUSH",
		"CODE_REF": "main", "PROJECT_ID": "101", "PROJECT_NAME": "order-service", "DEPLOY_ENV": "prod",
		"RUNNER_PLATFORM": "gitlab", "SKIP_TLS_VERIFY": "true",
	} {
		if env[k] != want {
			t.Errorf("env[%s] = %q, want %q", k, env[k], want)
		}
	}
	// non-MR checkout clones and checks out the exact commit (no merge).
	checkout := job.Spec.Template.Spec.InitContainers[0].Command[2]
	if !strings.Contains(checkout, "abc123def") {
		t.Errorf("checkout cmd missing commit: %q", checkout)
	}
	if strings.Contains(checkout, "merge") {
		t.Errorf("non-MR checkout should not merge: %q", checkout)
	}
}

// TestLauncherFromSpecMR covers the GitLab MR branch: TARGET_BRANCH env is set
// and the checkout merges the source commit into the target branch.
func TestLauncherFromSpecMR(t *testing.T) {
	cfg := model.Config{Host: "http://neutron.local"}
	cfg.Kubernetes.Namespace = "default"
	cfg.BaseConfig = map[string]model.CodeBase{
		"GitLab": {Url: "https://gitlab.example.com", Token: "tok"},
	}
	srv := &Server{config: cfg, clientSet: fake.NewSimpleClientset()}

	spec := model.JobSpec{
		Platform:     "GitLab",
		JobName:      "test",
		Image:        "node:18",
		ProjectId:    "7",
		CommitSha:    "deadbeef",
		ReportSha:    "deadbeef",
		Trigger:      "MR",
		GitRepoUrl:   "git@gitlab.example.com:web/portal.git",
		TargetBranch: "develop",
	}

	job := srv.launcherFromSpec(spec).CreateJob(srv.config.Host)

	env := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["TARGET_BRANCH"] != "develop" {
		t.Errorf("env[TARGET_BRANCH] = %q, want develop", env["TARGET_BRANCH"])
	}
	if env["SKIP_TLS_VERIFY"] != "" {
		t.Errorf("env[SKIP_TLS_VERIFY] = %q, want empty (TLS verify on)", env["SKIP_TLS_VERIFY"])
	}
	checkout := job.Spec.Template.Spec.InitContainers[0].Command[2]
	if !strings.Contains(checkout, "develop") || !strings.Contains(checkout, "merge") {
		t.Errorf("MR checkout should clone target branch and merge: %q", checkout)
	}
}
