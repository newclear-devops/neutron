package main

import (
	"testing"

	"neutron/internal/model"
)

// A config file with no `codebase:` section leaves BaseConfig nil. Applying
// codebase env overrides on top of it used to panic on the nil-map write; the
// map is now allocated on demand.
func TestApplyEnvOverridesOnEmptyConfig(t *testing.T) {
	t.Setenv("NEUTRON_GITLAB_URL", "https://gitlab.example.com")
	t.Setenv("NEUTRON_GITLAB_TOKEN", "tok")
	t.Setenv("NEUTRON_CODEUP_WEBHOOK_URL", "https://neutron.example.com")
	t.Setenv("NEUTRON_JOB_TTL_MINUTES", "60")

	var config model.Config
	applyEnvOverrides(&config)

	if got := config.BaseConfig["GitLab"].Url; got != "https://gitlab.example.com" {
		t.Errorf("GitLab url = %q", got)
	}
	if got := config.BaseConfig["GitLab"].Token; got != "tok" {
		t.Errorf("GitLab token = %q", got)
	}
	if got := config.BaseConfig["Codeup"].WebhookUrl; got != "https://neutron.example.com" {
		t.Errorf("Codeup webhook url = %q", got)
	}
	if config.Kubernetes.JobTtlMinutes == nil || *config.Kubernetes.JobTtlMinutes != 60 {
		t.Errorf("job ttl = %v, want 60", config.Kubernetes.JobTtlMinutes)
	}
}

func TestApplyEnvOverridesKeepsExistingCodebase(t *testing.T) {
	t.Setenv("NEUTRON_GITLAB_URL", "https://overridden.example.com")

	config := model.Config{BaseConfig: map[string]model.CodeBase{
		"GitLab": {Url: "https://from-file.example.com", Token: "file-token"},
	}}
	applyEnvOverrides(&config)

	cb := config.BaseConfig["GitLab"]
	if cb.Url != "https://overridden.example.com" {
		t.Errorf("url = %q, want the env override", cb.Url)
	}
	if cb.Token != "file-token" {
		t.Errorf("token = %q, want the file value preserved", cb.Token)
	}
}
