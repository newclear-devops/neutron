package model

import "math"

type Config struct {
	Host       string              `yaml:"host"`
	Port       int                 `yaml:"port"`
	Database   string              `yaml:"database"`
	LogUrl     string              `yaml:"log_url,omitempty"` // 日志平台链接模板，支持 {namespace} 和 {podId} 占位符
	BaseConfig map[string]CodeBase `yaml:"codebase"`
	// PodCodeBase 覆盖 K8s Pod 内 runner 访问 codebase 的地址（当 Pod 网络与宿主机不同时使用）
	PodCodeBase map[string]CodeBase `yaml:"pod_codebase,omitempty"`
	Kubernetes  KubernetesConfig    `yaml:"kubernetes"`
	Notify      NotifyConfig        `yaml:"notify,omitempty"`
	Snippets    SnippetsConfig      `yaml:"snippets,omitempty"`
}

type NotifyConfig struct {
	Url           string `yaml:"url"`
	CorpId        string `yaml:"corp_id"`
	AppId         string `yaml:"app_id"`
	SkipTLSVerify bool   `yaml:"skip_tls_verify,omitempty"`
}

type KubernetesConfig struct {
	KubeConfig       string   `yaml:"kube-config"`
	Namespace        string   `yaml:"namespace"`
	GitPrivateKey    string   `yaml:"git-private-key"`
	InitImage        string   `yaml:"init-image"`
	CheckoutImage    string   `yaml:"checkout-image"`      // dedicated image for git checkout (must include git + ssh)
	ImagePullSecrets []string `yaml:"image-pull-secrets,omitempty"` // K8s image pull secret names
	PodApiUrl        string   `yaml:"pod-api-url,omitempty"`        // Pod 内访问 API server 的地址（本地开发用，覆盖集群内地址）
	// JobTtlMinutes is the finished-Job retention window in minutes: how long a
	// pipeline Job stays in Kubernetes after reaching a terminal state before
	// the K8s TTL controller deletes it (which cascades to its Pods).
	// Unset → DefaultJobTtlMinutes; a negative value disables the cleanup.
	JobTtlMinutes *int `yaml:"job-ttl-minutes,omitempty"`
}

// DefaultJobTtlMinutes is the retention applied to finished pipeline Jobs when
// kubernetes.job-ttl-minutes is not configured: 8 hours. It is deliberately much
// larger than the reconciler's grace period, so a job that finished without ever
// delivering a final runner report is reconciled — and its terminal status
// written back — long before its K8s Job disappears. Once the K8s Job is gone it
// is the only remaining source of truth for such rows, so deleting it too early
// leaves them stuck as "running" forever.
const DefaultJobTtlMinutes = 8 * 60

// EffectiveJobTtlSeconds resolves the TTL stamped on every created pipeline Job,
// in the seconds unit the K8s Job field requires. Returns nil (leaving the field
// unset) when cleanup is disabled via a negative value.
func (k KubernetesConfig) EffectiveJobTtlSeconds() *int32 {
	// An explicit 0 also means "use the default" rather than "delete
	// immediately", which is what K8s would do with a literal 0 TTL.
	if k.JobTtlMinutes == nil || *k.JobTtlMinutes == 0 {
		return int32Ptr(DefaultJobTtlMinutes * 60)
	}
	if *k.JobTtlMinutes < 0 {
		return nil
	}
	seconds := *k.JobTtlMinutes * 60
	if seconds > math.MaxInt32 {
		// Clamp rather than silently overflowing into a negative TTL.
		seconds = math.MaxInt32
	}
	return int32Ptr(seconds)
}

func int32Ptr(i int) *int32 {
	v := int32(i)
	return &v
}

// SnippetsConfig configures an optional GitLab-backed snippet store. When RepoUrl
// is non-empty, snippets are synced from the repository on startup and via the
// refresh API; the MySQL neutron_snippet table acts as a local read cache.
type SnippetsConfig struct {
	RepoUrl  string `yaml:"repo_url"`  // GitLab project URL, e.g. git@gitlab.example.com:platform/snippets.git
	Platform string `yaml:"platform"`  // which codebase entry to reuse for API token/auth, e.g. "GitLab"
	Ref      string `yaml:"ref"`       // branch to read from (defaults to "main" when empty)
}

type CodeBase struct {
	Url            string `yaml:"url"`
	Token          string `yaml:"token"`
	SkipTLSVerify  bool   `yaml:"skip_tls_verify,omitempty"`
	WebhookUrl     string `yaml:"webhook_url,omitempty"` // 外部可访问的 webhook URL（覆盖 config.Host）
}
