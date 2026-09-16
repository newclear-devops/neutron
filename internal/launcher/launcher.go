package launcher

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"neutron/internal/model"
	"neutron/internal/parser"
	"strings"
	"time"
)

type Launcher struct {
	Namespace        string
	RunnerConfig     model.RunnerConfig
	InitImage        string
	CheckoutImage    string
	PipelineImage    string
	SshKeyName       string
	ImagePullSecrets []string
	Platform         string
	PodApiUrl        string           // override NEUTRON_API_URL for pods (local dev)
	JobTtlSeconds    *int32           // finished-Job retention handed to K8s as TTLSecondsAfterFinished; nil = never auto-cleanup
	ExtraEnv         []v1.EnvVar      // platform-specific env vars (e.g. TARGET_BRANCH for GitLab MR)
	Resources        *model.Resources // job-level resource requirements
}

func NewLauncher(namespace string, runnerConfig model.RunnerConfig, initImage string, checkoutImage string, baseImage string, keyName string, imagePullSecrets []string, platform string, podApiUrl string, jobTtlSeconds *int32, resources *model.Resources, extraEnv ...v1.EnvVar) *Launcher {
	return &Launcher{
		Namespace:        namespace,
		RunnerConfig:     runnerConfig,
		InitImage:        initImage,
		CheckoutImage:    checkoutImage,
		PipelineImage:    baseImage,
		SshKeyName:       keyName,
		ImagePullSecrets: imagePullSecrets,
		Platform:         platform,
		PodApiUrl:        podApiUrl,
		JobTtlSeconds:    jobTtlSeconds,
		ExtraEnv:         extraEnv,
		Resources:        resources,
	}
}

func (l *Launcher) CreateJob(neutronHost string) *batchv1.Job {
	ts := time.Now().Format("20060102-150405")
	fullJobName := buildJobName(parser.ExtractRepoName(l.RunnerConfig.GitRepoUrl), l.RunnerConfig.JobName, ts)
	var checkoutCommand string
	if l.RunnerConfig.Trigger == "MR" && l.RunnerConfig.TargetBranch != "" {
		// clone target branch, fetch source commit, merge
		checkoutCommand = fmt.Sprintf(
			"git clone --branch %s %s /repo && cd /repo && git config user.email neutron@ci && git config user.name neutron && git fetch origin %s && git merge --no-edit %s && chmod -R 777 /repo",
			shellEscape(l.RunnerConfig.TargetBranch), shellEscape(l.RunnerConfig.GitRepoUrl),
			shellEscape(l.RunnerConfig.CommitSha), shellEscape(l.RunnerConfig.CommitSha))
	} else {
		// for tag or push, checkout specific sha
		checkoutCommand = fmt.Sprintf("git clone %s /repo && git checkout %s && chmod -R 777 /repo",
			shellEscape(l.RunnerConfig.GitRepoUrl), shellEscape(l.RunnerConfig.CommitSha))
	}

	// common env vars for all platforms
	env := []v1.EnvVar{
		{Name: "CODEBASE_TOKEN", Value: l.RunnerConfig.CodebaseToken},
		{Name: "CODEBASE_URL", Value: l.RunnerConfig.CodebaseUrl},
		{Name: "PROJECT_ID", Value: l.RunnerConfig.ProjectId},
		{Name: "PROJECT_NAME", Value: parser.ExtractRepoName(l.RunnerConfig.GitRepoUrl)},
		{Name: "COMMIT_SHA", Value: l.RunnerConfig.CommitSha},
		{Name: "REPORT_SHA", Value: l.RunnerConfig.ReportSha},
		{Name: "TRIGGER", Value: l.RunnerConfig.Trigger},
		{Name: "CODE_REF", Value: l.RunnerConfig.CodeRef},
		{Name: "JOB_NAME", Value: l.RunnerConfig.JobName},
		{Name: "FULL_JOB_NAME", Value: fullJobName},
		{Name: "GIT_REPO_URL", Value: l.RunnerConfig.GitRepoUrl},
		{Name: "GIT_PRIVATE_KEY", Value: l.RunnerConfig.GitPrivateKey},
		{Name: "PIPELINE_URL", Value: fmt.Sprintf("%s/#/status/%s", neutronHost, fullJobName)},
		{Name: "NEUTRON_API_URL", Value: l.podApiUrl()},
		{Name: "POD_NAME", ValueFrom: &v1.EnvVarSource{
			FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.name"},
		}},
		{Name: "POD_NAMESPACE", ValueFrom: &v1.EnvVarSource{
			FieldRef: &v1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
		}},
	}
	if l.RunnerConfig.SkipTriggerCheck {
		env = append(env, v1.EnvVar{Name: "SKIP_TRIGGER_CHECK", Value: "true"})
	}
	if l.RunnerConfig.SkipPlatformReport {
		env = append(env, v1.EnvVar{Name: "SKIP_PLATFORM_REPORT", Value: "true"})
	}
	env = append(env, l.ExtraEnv...)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fullJobName,
			Namespace: l.Namespace,
			Annotations: map[string]string{
				"sourceLink":  fmt.Sprintf("%s/projects/%s", l.RunnerConfig.CodebaseUrl, l.RunnerConfig.ProjectId),
				"sourceUrl":   l.RunnerConfig.SourceUrl,
				"sourceType":  l.Platform,
				"triggerType": l.RunnerConfig.Trigger,
				"gitPath":     l.RunnerConfig.GitRepoUrl,
			},
		},
		Spec: batchv1.JobSpec{
			// No retries: re-running would repeat side-effecting steps
			// (deploys), and the runner already reported the failure.
			BackoffLimit: int32Ptr(0),
			// K8s deletes the Job this long after it reaches a terminal state,
			// cascading to its Pods. Without it finished Jobs accumulate
			// forever. See model.KubernetesConfig.EffectiveJobTtlSeconds for
			// why this must comfortably exceed the reconciler's grace period.
			TTLSecondsAfterFinished: l.JobTtlSeconds,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{
						{
							Name:    "pipeline",
							Image:   l.PipelineImage,
							Command: []string{"/pipeline/runner"},
							Env:     env,
							VolumeMounts: []v1.VolumeMount{
								{MountPath: "/pipeline", Name: "pipeline"},
								{MountPath: "/repo", Name: "repo"},
							},
							Resources: l.buildResourceRequirements(),
						},
					},
					InitContainers: []v1.Container{
						{
							Name:  "checkout",
							Image: l.CheckoutImage,
							Command: []string{
								"/bin/sh",
								"-c",
								checkoutCommand,
							},
							WorkingDir: "/repo",
							Env: []v1.EnvVar{
								{Name: "GIT_SSH_COMMAND", Value: "ssh -o StrictHostKeyChecking=no"},
							},
							VolumeMounts: []v1.VolumeMount{
								{MountPath: "/repo", Name: "repo"},
								{MountPath: "/root/.ssh/id_rsa", Name: "private-key", SubPath: "id_rsa", ReadOnly: true},
							},
						},
						{
							Name:  "init",
							Image: l.InitImage,
							Command: []string{
								"/bin/sh", "-c",
								`case "${RUNNER_PLATFORM}" in codeup) cp /runners/codeup-runner /pipeline/runner ;; *) cp /runners/gitlab-runner /pipeline/runner ;; esac`,
							},
							Env: env,
							VolumeMounts: []v1.VolumeMount{
								{MountPath: "/pipeline", Name: "pipeline"},
							},
						},
					},
					RestartPolicy:    v1.RestartPolicyNever,
					ImagePullSecrets: l.imagePullSecrets(),
					Volumes: []v1.Volume{
						{Name: "pipeline", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
						{Name: "repo", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}},
						{Name: "private-key", VolumeSource: v1.VolumeSource{
							Secret: &v1.SecretVolumeSource{
								SecretName: l.SshKeyName,
								Items: []v1.KeyToPath{
									{Key: "id_rsa", Path: "id_rsa"},
								},
								DefaultMode: int32Ptr(0400),
							},
						},
						},
					},
				},
			},
		},
	}
	return job
}

func (l *Launcher) podApiUrl() string {
	if l.PodApiUrl != "" {
		return l.PodApiUrl
	}
	return fmt.Sprintf("http://neutron-api.%s.svc.cluster.local:8888", l.Namespace)
}

func int32Ptr(i int32) *int32 {
	return &i
}

const (
	// jobNameMaxLength caps a generated Job name at the K8s label value limit, so
	// the `job-name` label the Job controller derives from it stays complete.
	jobNameMaxLength = 63
	// maxNamePartLength caps each variable segment before the combined budget
	// below is applied, so one runaway segment cannot eat the whole name.
	maxNamePartLength  = 20
	timestampLength    = 15 // YYYYMMDD-HHMMSS
	randomSuffixLength = 4
	// namePartsBudget is what remains for project+job once the fixed parts are
	// subtracted: len("neutron") + 4 separators + timestamp + random suffix.
	namePartsBudget = jobNameMaxLength - (len("neutron") + 4 + timestampLength + randomSuffixLength)
)

// projectNamePart reduces a repo-derived name to its letters and digits,
// lowercased, because a K8s object name accepts only [a-z0-9-.] while repo names
// routinely carry '_', '.' or uppercase — either would make Job creation fail
// outright. Everything else is dropped rather than transliterated to '-', which
// also keeps '-' unambiguous as the separator between name segments.
func projectNamePart(s string, maxLen int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if b.Len() >= maxLen {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// jobNamePart makes a pipeline job key safe for use in an object name: lowercase,
// every other run of characters collapsed into a single '-', leading and trailing
// separators trimmed. Unlike the project segment, dashes are preserved here —
// they are part of how job keys are written in neutron.yaml.
func jobNamePart(s string, maxLen int) string {
	var b strings.Builder
	pendingDash := true // do not open the name with a separator
	for _, r := range strings.ToLower(s) {
		if b.Len() >= maxLen {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			pendingDash = false
			continue
		}
		if pendingDash {
			continue
		}
		b.WriteRune('-')
		pendingDash = true
	}
	return strings.TrimRight(b.String(), "-")
}

// buildJobName constructs a unique K8s Job name:
//
//	neutron-<project>-<job>-<YYYYMMDD-HHMMSS>-<4-hex>
//
// <project> is the repository name extracted from the repo URL. Projects sharing
// a default pipeline launch identically named jobs, so without it their pods are
// indistinguishable in `kubectl get pods`. It is omitted when the repo URL yields
// nothing usable, leaving the pre-existing format intact.
//
// The 4-char random suffix keeps two triggers of the same job within the same
// second from colliding (the timestamp alone previously did).
//
// The trailing -<timestamp>-<random> layout must survive future edits: the SQL
// recency filter (internal.jobTimestampExpr) locates the timestamp by counting
// characters from the END of the name, and the frontend derives both the run
// duration and the pod-log expiry window from that trailing timestamp. Adding
// segments is only ever safe at the front.
func buildJobName(projectName, jobName, ts string) string {
	p := projectNamePart(projectName, maxNamePartLength)
	j := jobNamePart(jobName, maxNamePartLength)
	if j == "" {
		j = "job"
	}
	if p == "" {
		return fmt.Sprintf("neutron-%s-%s-%s", j, ts, randSuffix())
	}
	if len(p)+len(j) > namePartsBudget {
		// The job key is the more identifying half, so the project gives way.
		// j is capped at 20 < namePartsBudget, so this never goes negative.
		p = p[:namePartsBudget-len(j)]
	}
	return fmt.Sprintf("neutron-%s-%s-%s-%s", p, j, ts, randSuffix())
}

// randSuffix returns a 4-character lowercase hex string (16 random bits) for the
// job-name uniqueness suffix. crypto/rand is used so two same-second triggers
// still diverge with overwhelming probability.
func randSuffix() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is effectively unreachable; fall back to a
		// non-random suffix so job creation still proceeds rather than aborting.
		return "0000"
	}
	return hex.EncodeToString(b[:])
}

func (l *Launcher) imagePullSecrets() []v1.LocalObjectReference {
	refs := make([]v1.LocalObjectReference, 0, len(l.ImagePullSecrets))
	for _, name := range l.ImagePullSecrets {
		if name != "" {
			refs = append(refs, v1.LocalObjectReference{Name: name})
		}
	}
	return refs
}

// buildResourceRequirements converts model.Resources to K8s ResourceRequirements
func (l *Launcher) buildResourceRequirements() v1.ResourceRequirements {
	if l.Resources == nil {
		return v1.ResourceRequirements{}
	}

	req := v1.ResourceRequirements{}

	if l.Resources.Limits.Cpu != "" || l.Resources.Limits.Memory != "" {
		req.Limits = v1.ResourceList{}
		if l.Resources.Limits.Cpu != "" {
			req.Limits[v1.ResourceCPU] = resource.MustParse(l.Resources.Limits.Cpu)
		}
		if l.Resources.Limits.Memory != "" {
			req.Limits[v1.ResourceMemory] = resource.MustParse(l.Resources.Limits.Memory)
		}
	}

	if l.Resources.Requests.Cpu != "" || l.Resources.Requests.Memory != "" {
		req.Requests = v1.ResourceList{}
		if l.Resources.Requests.Cpu != "" {
			req.Requests[v1.ResourceCPU] = resource.MustParse(l.Resources.Requests.Cpu)
		}
		if l.Resources.Requests.Memory != "" {
			req.Requests[v1.ResourceMemory] = resource.MustParse(l.Resources.Requests.Memory)
		}
	}

	return req
}

// shellEscape wraps a string in single quotes for safe use in shell commands.
func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
