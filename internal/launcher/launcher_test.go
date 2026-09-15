package launcher

import (
	"testing"

	"k8s.io/api/core/v1"
	"neutron/internal/model"
)

// newTestLauncher builds a Launcher with only the fields the TTL behaviour
// depends on.
func newTestLauncher(ttl *int32) *Launcher {
	return NewLauncher(
		"default",
		model.RunnerConfig{JobName: "build", Trigger: "PUSH", CommitSha: "abc123", GitRepoUrl: "git@gitlab.example.com:g/p.git"},
		"runner:latest", "checkout:latest", "maven:3.9", "git-ssh-secret",
		nil, "GitLab", "", ttl, nil,
		v1.EnvVar{Name: "RUNNER_PLATFORM", Value: "gitlab"},
	)
}

func TestCreateJobTtlSeconds(t *testing.T) {
	job := newTestLauncher(ttlPtr(600)).CreateJob("http://neutron.local")
	if job.Spec.TTLSecondsAfterFinished == nil {
		t.Fatal("TTLSecondsAfterFinished is nil, want 600")
	}
	if got := *job.Spec.TTLSecondsAfterFinished; got != 600 {
		t.Errorf("TTLSecondsAfterFinished = %d, want 600", got)
	}
}

// A nil TTL must leave the field unset rather than writing 0, which K8s reads as
// "delete immediately after completion".
func TestCreateJobTtlDisabledLeavesFieldUnset(t *testing.T) {
	job := newTestLauncher(nil).CreateJob("http://neutron.local")
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Errorf("TTLSecondsAfterFinished = %d, want nil (no cleanup)", *job.Spec.TTLSecondsAfterFinished)
	}
}

func ttlPtr(i int32) *int32 { return &i }
