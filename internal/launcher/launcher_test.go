package launcher

import (
	"regexp"
	"strings"
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

// jobNamePattern describes the generated name: an optional project segment, the
// job key, then the fixed trailing -<YYYYMMDD-HHMMSS>-<4-hex>.
var jobNamePattern = regexp.MustCompile(`^neutron-(?:[a-z0-9]{1,20}-)[a-z0-9][a-z0-9-]*-\d{12}-[0-9a-f]{4}$`)

func TestBuildJobNameIncludesProject(t *testing.T) {
	name := buildJobName("order-service", "build", "260916120000")
	// The repo-derived segment is squeezed of every non-alphanumeric character.
	if !strings.HasPrefix(name, "neutron-orderservice-build-260916120000-") {
		t.Errorf("name = %q, want neutron-orderservice-build-<ts>-<rand>", name)
	}
}

func TestBuildJobNameProjectKeepsOnlyAlnum(t *testing.T) {
	// Uppercase, underscores and dots are common in repo names and none survive
	// as-is in a K8s object name.
	if got := projectNamePart("My_Repo.Service", maxNamePartLength); got != "myreposervice" {
		t.Errorf("projectNamePart = %q, want %q", got, "myreposervice")
	}
	name := buildJobName("My_Repo.Service", "build", "260916120000")
	if !strings.HasPrefix(name, "neutron-myreposervice-build-") {
		t.Errorf("name = %q, want the sanitized project segment", name)
	}
}

func TestBuildJobNamePreservesJobKeyDashes(t *testing.T) {
	name := buildJobName("order-service", "build-image", "260916120000")
	if !strings.HasPrefix(name, "neutron-orderservice-build-image-") {
		t.Errorf("name = %q, want the job key dashes preserved", name)
	}
}

// The K8s label derived from a Job name is capped at 63 chars, so the generated
// name must never exceed it — the project gives way to keep the job key intact.
func TestBuildJobNameStaysWithinLabelLimit(t *testing.T) {
	name := buildJobName("payment-gateway-integration-service", "end-to-end-integration-suite", "260916120000")
	if len(name) > jobNameMaxLength {
		t.Errorf("name length = %d, want <= %d (%q)", len(name), jobNameMaxLength, name)
	}
	if !strings.Contains(name, "-end-to-end-integ") {
		t.Errorf("name = %q, want the job key kept whole and the project trimmed", name)
	}
}

// A repo URL from which no usable name can be extracted falls back to the
// pre-existing format rather than producing a stray separator.
func TestBuildJobNameWithoutProject(t *testing.T) {
	name := buildJobName("", "build", "260916120000")
	if !strings.HasPrefix(name, "neutron-build-260916120000-") {
		t.Errorf("name = %q, want the legacy neutron-<job>-<ts>-<rand> shape", name)
	}
	if strings.Contains(name, "--") {
		t.Errorf("name = %q, want no empty segment", name)
	}
}

func TestBuildJobNameAlwaysWellFormed(t *testing.T) {
	for _, tc := range [][2]string{
		{"payment-service", "deploy"},
		{"My_Repo", "Build Image"},
		{"payment-gateway-integration-service", "end-to-end-integration-suite"},
	} {
		name := buildJobName(tc[0], tc[1], "260916120000")
		if !jobNamePattern.MatchString(name) {
			t.Errorf("name = %q, does not match the expected shape", name)
		}
		if len(name) > jobNameMaxLength {
			t.Errorf("name = %q, length %d exceeds %d", name, len(name), jobNameMaxLength)
		}
	}
}
