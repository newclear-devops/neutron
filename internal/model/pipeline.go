package model

type Pipeline struct {
	Jobs map[string]Job `yaml:"jobs"`
}

type Job struct {
	Image     string     `yaml:"image"`
	Trigger   []string   `yaml:"trigger"`
	Steps     []Step     `yaml:"steps"`
	Resources *Resources `yaml:"resources,omitempty"`
	Notify    *Notify    `yaml:"notify,omitempty"`
}

// Notify declares the per-job notification targets. Both fields are optional;
// an empty or absent Notify block means the job sends no notifications.
type Notify struct {
	Users  []string `yaml:"users,omitempty" json:"users,omitempty"`   // IM personal message recipients (user ids)
	Groups []string `yaml:"groups,omitempty" json:"groups,omitempty"` // CCWork group robot webhook URLs
}

// JobSpec is the persisted snapshot of a webhook-created job's inputs, kept on
// the DB row so an identical K8s Job can be recreated later (rerun). Tokens are
// intentionally NOT stored — they are re-resolved from config at rerun time.
// Steps are not stored either — the runner reads neutron.yaml from the same
// immutable commit, so the same file is reproduced exactly.
type JobSpec struct {
	Platform     string            `json:"platform"`               // GitLab / Codeup
	JobName      string            `json:"job_name"`               // pipeline job key (e.g. "build")
	Image        string            `json:"image"`
	Resources    *Resources        `json:"resources,omitempty"`
	ProjectId    string            `json:"project_id"`             // RunnerConfig.ProjectId (numeric string)
	CommitSha    string            `json:"commit_sha"`
	ReportSha    string            `json:"report_sha"`
	Trigger      string            `json:"trigger"`                // PUSH / MR / TAG
	GitRepoUrl   string            `json:"git_repo_url"`
	TargetBranch string            `json:"target_branch,omitempty"`
	CodeRef      string            `json:"code_ref,omitempty"`
	SourceUrl    string            `json:"source_url,omitempty"`
	QueryParams  map[string]string `json:"query_params,omitempty"` // webhook URL query params → pod env
}

type Step struct {
	StepName string `yaml:"name"`
	Command  string `yaml:"cmd"`
}

type Resources struct {
	Limits   ResourceSpec `yaml:"limits,omitempty"`
	Requests ResourceSpec `yaml:"requests,omitempty"`
}

type ResourceSpec struct {
	Cpu    string `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"`
}

type RunnerConfig struct {
	CodebaseToken      string // codebase access token
	CodebaseUrl        string // codebase API base URL
	ProjectId          string
	CommitSha          string
	ReportSha          string // GitLab: commit SHA for status reporting; Codeup: unused
	Trigger            string
	JobName            string
	GitRepoUrl         string
	GitPrivateKey      string
	TargetBranch       string // MR target branch, GitLab only
	CodeRef            string // tag name for TAG, branch name for PUSH, empty for MR
	SourceUrl          string // URL to the source branch/MR on the code hosting platform
	SkipTriggerCheck   bool   // skip trigger type validation (for API-triggered jobs)
	SkipPlatformReport bool   // skip reporting commit status to platform (for API-triggered jobs)
}

type StepResult string

const (
	Pending StepResult = "Pending"
	Running StepResult = "Running"
	Fail    StepResult = "Fail"
	Success StepResult = "Success"
)

type Reporter interface {
	Report(jobName string, stepName string, status StepResult, description string)
	// ReportJobFinal reports the job-level terminal status (Success or Fail).
	// The runner sends it exactly once, after the last step, so the API server
	// can distinguish "job finished" from per-step reports.
	ReportJobFinal(status StepResult, description string)
}
