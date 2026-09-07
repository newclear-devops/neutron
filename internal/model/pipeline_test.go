package model

import "testing"

func TestResolveJob_PrefersRepoJob(t *testing.T) {
	repo := Pipeline{Jobs: map[string]Job{
		"build": {Image: "repo-image"},
	}}
	def := Pipeline{Jobs: map[string]Job{
		"build": {Image: "default-image"},
	}}

	job, ok := ResolveJob(repo, def, "build")
	if !ok {
		t.Fatal("expected job to be found")
	}
	if job.Image != "repo-image" {
		t.Fatalf("expected repo job to win (whole-job override), got image %q", job.Image)
	}
}

func TestResolveJob_FallsBackToDefault(t *testing.T) {
	repo := Pipeline{Jobs: map[string]Job{
		"build": {Image: "repo-image"},
	}}
	def := Pipeline{Jobs: map[string]Job{
		"deploy": {Image: "default-image"},
	}}

	job, ok := ResolveJob(repo, def, "deploy")
	if !ok {
		t.Fatal("expected fallback to default pipeline job")
	}
	if job.Image != "default-image" {
		t.Fatalf("expected default job, got image %q", job.Image)
	}
}

func TestResolveJob_NotFound(t *testing.T) {
	repo := Pipeline{Jobs: map[string]Job{
		"build": {Image: "repo-image"},
	}}
	def := Pipeline{Jobs: map[string]Job{
		"deploy": {Image: "default-image"},
	}}

	if _, ok := ResolveJob(repo, def, "missing"); ok {
		t.Fatal("expected job to be missing from both pipelines")
	}
	if _, ok := ResolveJob(repo, Pipeline{}, "deploy"); ok {
		t.Fatal("expected job to be missing when default pipeline is empty")
	}
}
