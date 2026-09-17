package internal

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"neutron/internal/model"
	"strings"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

type PipelineProject struct {
	Id          string `gorm:"column:id;primaryKey"`
	WebhookType string `gorm:"column:webhook_type"`
	RepoUrl     string `gorm:"column:repo_url"`
}

func (PipelineProject) TableName() string {
	return "neutron_project"
}

type PipelineJob struct {
	Id        int64  `gorm:"column:id;primaryKey;autoIncrement"`
	ProjectId string `gorm:"column:project_id;index:idx_job_project_name,priority:1"`
	Name      string `gorm:"column:name;type:varchar(255);uniqueIndex"`
	// JobName is the logical pipeline job key from neutron.yaml (as opposed to
	// Name, which is the generated K8s Job name). Stored as its own column
	// because it cannot be parsed back out of Name reliably — both the project
	// and the job segment are [a-z0-9], and either can be truncated to fit the
	// length budget. Empty on rows created before this column existed.
	JobName     string        `gorm:"column:job_name;type:varchar(255);index:idx_job_project_name,priority:2"`
	Status      string        `gorm:"column:status;type:text"`
	Notify      string        `gorm:"column:notify;type:text"` // JSON-encoded model.Notify, captured at trigger time
	Spec        string        `gorm:"column:spec;type:text"`   // JSON-encoded model.JobSpec for rerun; empty for API-triggered jobs
	Params      string        `gorm:"column:params;type:text"` // JSON-encoded model.JobParams (ref/env/...), captured at trigger time
	Completed   bool          `gorm:"column:completed;default:false"`
	CompletedAt *time.Time    `gorm:"column:completed_at"`
	Pods        []PipelinePod `gorm:"foreignKey:JobId"`
}

func (PipelineJob) TableName() string {
	return "neutron_job"
}

type PipelinePod struct {
	Id      int64  `gorm:"column:id;primaryKey;autoIncrement"`
	JobId   int64  `gorm:"column:job_id;index"`
	PodName string `gorm:"column:pod_name;type:varchar(255)"`
	PodUid  string `gorm:"column:pod_uid;type:varchar(255)"`
	Phase   string `gorm:"column:phase;type:varchar(50)"`
}

func (PipelinePod) TableName() string {
	return "neutron_pod"
}

type JobReport struct {
	Id        int64      `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	JobName   string     `gorm:"column:job_name;type:varchar(255);uniqueIndex" json:"job_name"`
	ReportUrl string     `gorm:"column:report_url;type:varchar(2048)" json:"report_url"`
	CreatedAt *time.Time `gorm:"column:created_at" json:"created_at"`
}

func (JobReport) TableName() string {
	return "neutron_job_report"
}

type Snippet struct {
	Id          int64      `gorm:"column:id;primaryKey;autoIncrement" json:"id"`
	Name        string     `gorm:"column:name;type:varchar(255);uniqueIndex" json:"name"`
	Title       string     `gorm:"column:title;type:varchar(255)" json:"title"`
	Content     string     `gorm:"column:content;type:text" json:"content"`
	Description string     `gorm:"column:description;type:text" json:"description"`
	Params      string     `gorm:"column:params;type:text" json:"params"`
	CreatedAt   *time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   *time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Snippet) TableName() string {
	return "neutron_snippet"
}

// Setting is a generic key/value store for global configuration (e.g. the
// default pipeline used when a repository has no neutron.yaml).
type Setting struct {
	Key       string     `gorm:"column:key;type:varchar(64);primaryKey" json:"key"`
	Value     string     `gorm:"column:value;type:longtext" json:"value"`
	UpdatedAt *time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (Setting) TableName() string {
	return "neutron_setting"
}

// SettingDefaultPipeline is the Setting key holding the default neutron.yaml.
const SettingDefaultPipeline = "default_pipeline"

type JobStatus struct {
	WebhookType string `json:"webhook_type"`
	RepoUrl     string `json:"repo_url"`
	ProjectUrl  string `json:"project_url"`
	SourceUrl   string `json:"source_url"`
	TriggerType string `json:"trigger_type"`
	Active      int    `json:"active"`
	Succeeded   int    `json:"succeeded"`
	Failed      int    `json:"failed"`
	// Final marks a job-level terminal report, sent by the runner exactly once
	// after the last step. Completion notifications and DB completion are gated
	// on it so per-step reports are not mistaken for job completion.
	Final bool `json:"final,omitempty"`
	// Description carries the job-level summary (failing step, or the K8s
	// failure condition for jobs the runner never reported), included in
	// completion notifications.
	Description string `json:"description,omitempty"`
}

type Repository struct {
	db *gorm.DB
}

func NewRepository(config model.Config) *Repository {
	db, err := gorm.Open(mysql.Open(config.Database), &gorm.Config{
		Logger:                                   logger.Default.LogMode(logger.Warn),
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		log.Fatalf("cannot connect to database: %v", err)
	}

	// Auto-migrate tables
	if err := db.AutoMigrate(&PipelineProject{}, &PipelineJob{}, &PipelinePod{}, &JobReport{}, &Snippet{}, &Setting{}); err != nil {
		log.Fatalf("failed to auto-migrate database: %v", err)
	}

	return &Repository{
		db: db,
	}
}

func (r *Repository) Close() {
	sqlDB, err := r.db.DB()
	if err != nil {
		return
	}
	sqlDB.Close()
}

func (r *Repository) DB() *gorm.DB {
	return r.db
}

func (r *Repository) GetWebhookConfig(id string) PipelineProject {
	var project PipelineProject
	result := r.db.Where("id = ?", id).First(&project)
	if result.Error != nil {
		return PipelineProject{}
	}
	return project
}

func (r *Repository) AddWebhookConfig(p PipelineProject) error {
	return r.db.Create(&p).Error
}

func (r *Repository) ListProjects() ([]PipelineProject, error) {
	var projects []PipelineProject
	err := r.db.Order("id").Find(&projects).Error
	return projects, err
}

func (r *Repository) GetProjectByRepoUrl(repoUrl string) PipelineProject {
	var project PipelineProject
	result := r.db.Where("repo_url = ?", repoUrl).First(&project)
	if result.Error != nil {
		return PipelineProject{}
	}
	return project
}

func (r *Repository) AddJob(job PipelineJob) error {
	return r.db.Create(&job).Error
}

func (r *Repository) UpdateJobStatus(jobName string, status JobStatus) error {
	statusBytes, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return r.db.Model(&PipelineJob{}).Where("name = ?", jobName).Update("status", string(statusBytes)).Error
}

func (r *Repository) GetJobStatus(jobName string) (JobStatus, error) {
	var job PipelineJob
	result := r.db.Where("name = ?", jobName).First(&job)
	if result.Error != nil {
		return JobStatus{}, result.Error
	}
	var status JobStatus
	if err := json.Unmarshal([]byte(job.Status), &status); err != nil {
		return JobStatus{}, err
	}
	return status, nil
}

// cutoffDays returns the cutoff timestamp value (YYYYMMDD-000000) that
// jobTimestampExpr() output is compared against as a string.
func cutoffDays(days int) string {
	return time.Now().AddDate(0, 0, -days).Format("20060102") + "-000000"
}

// jobTimestampExpr returns the SQL expression normalizing whatever timestamp a
// job name carries into the fixed 15-char `YYYYMMDD-HHMMSS` form — the form
// cutoffDays produces, and therefore the form string comparison requires.
//
// Three name layouts coexist, each located by counting from the END of the name
// (which is what makes it safe to keep adding segments at the front):
//
//	current:  neutron-<project>-<job>-YYMMDDHHMMSS-<4-hex>
//	previous: neutron-<job>-YYYYMMDD-HHMMSS-<4-hex>
//	original: neutron-<job>-YYYYMMDD-HHMMSS
//
// The discriminators only inspect the tail: with a 4-char random suffix the char
// 5 positions from the end is the '-' before it, and with the compact timestamp
// the char 18 positions from the end is the '-' before the timestamp. The
// current layout is expanded back to the century-bearing form so that rows in
// the older layouts keep aging out of the recency windows correctly — the "20"
// prefix makes this wrong from 2100 onwards.
func jobTimestampExpr() string {
	return "CASE " +
		"WHEN SUBSTRING(name, LENGTH(name) - 17, 1) = '-' " +
		"THEN CONCAT('20', SUBSTRING(name, LENGTH(name) - 16, 6), '-', SUBSTRING(name, LENGTH(name) - 10, 6)) " +
		"WHEN SUBSTRING(name, LENGTH(name) - 4, 1) = '-' " +
		"THEN SUBSTRING(name, LENGTH(name) - 19, 15) " +
		"ELSE RIGHT(name, 15) END"
}

// Paging limits for the job listings. These endpoints used to return every row
// in the recency window at once, which for an active project is thousands of
// rows including the notify/spec/status text blobs — and an extra query per row
// to preload pods.
const (
	DefaultPageSize = 20
	MaxPageSize     = 100
	// MaxPage caps how deep a caller may page. The project page can lift its
	// recency window (?all=1), so the reachable row count is no longer bounded
	// by that window: the cap still stops an accidental or hostile deep page
	// from scanning to the end of the table, and keeps (page-1)*pageSize from
	// overflowing into a negative OFFSET.
	MaxPage = 1000
)

// recentWindow scopes a query to jobs whose embedded timestamp falls inside the
// last `days` days. Shared by every listing so they all age out the same way.
func recentWindow(days int) (string, string) {
	return jobTimestampExpr() + " >= ?", cutoffDays(days)
}

// ListProjectJobsPaged returns one page of a project's jobs, newest first, plus
// the total number of rows so the caller can render pagination. days <= 0 lifts
// the recency window and pages the full history (still bounded by the page
// params). Filtering by the logical job key (JobName) is done here rather than
// in the client because the client no longer holds every row.
func (r *Repository) ListProjectJobsPaged(projectId, jobName string, days, pageSize, page int) ([]PipelineJob, int64, error) {
	q := r.db.Where("project_id = ?", projectId)
	if days > 0 {
		window, cutoff := recentWindow(days)
		q = q.Where(window, cutoff)
	}
	if jobName != "" {
		q = q.Where("job_name = ?", jobName)
	}

	var total int64
	if err := q.Model(&PipelineJob{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var jobs []PipelineJob
	err := q.Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Preload("Pods").Find(&jobs).Error
	return jobs, total, err
}

// ListProjectJobNames returns the distinct logical job keys a project has run,
// for populating the filter dropdown. days <= 0 covers the full history: the
// DISTINCT is served by idx_job_project_name (project_id, job_name), so dropping
// the recency window costs an index scan rather than a table scan. Rows
// predating the job_name column contribute nothing and are simply absent.
func (r *Repository) ListProjectJobNames(projectId string, days int) ([]string, error) {
	q := r.db.Model(&PipelineJob{}).Where("project_id = ? AND job_name <> ''", projectId)
	if days > 0 {
		window, cutoff := recentWindow(days)
		q = q.Where(window, cutoff)
	}
	var names []string
	err := q.Distinct("job_name").Order("job_name").Pluck("job_name", &names).Error
	return names, err
}

// ListRecentJobsPaged returns one page of the newest jobs across all projects,
// optionally narrowed by a free-text search, plus the total row count.
func (r *Repository) ListRecentJobsPaged(query string, days, pageSize, page int) ([]PipelineJob, int64, error) {
	window, cutoff := recentWindow(days)
	q := r.db.Where(window, cutoff)
	q = applyJobSearch(q, query)

	var total int64
	if err := q.Model(&PipelineJob{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var jobs []PipelineJob
	err := q.Order("id DESC").Limit(pageSize).Offset((page - 1) * pageSize).Preload("Pods").Find(&jobs).Error
	return jobs, total, err
}

// applyJobSearch narrows a job query by the recent page's search box. Status
// words map onto the flags inside the status JSON (that is how the outcome is
// persisted); anything else matches the generated name, the logical job key, the
// owning project's repo URL, or the status payload itself, which carries
// repo_url / trigger_type / webhook_type.
func applyJobSearch(q *gorm.DB, query string) *gorm.DB {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return q
	}
	if clause, ok := statusSearchClause(query); ok {
		return q.Where(clause)
	}
	like := "%" + query + "%"
	return q.Where(
		"(LOWER(name) LIKE ? OR LOWER(job_name) LIKE ? OR LOWER(status) LIKE ? "+
			"OR project_id IN (SELECT id FROM neutron_project WHERE LOWER(repo_url) LIKE ?))",
		like, like, like, like,
	)
}

// statusSearchClause maps a search word onto the JobStatus flags as they appear
// in the persisted JSON (Go marshals it without spaces).
func statusSearchClause(word string) (string, bool) {
	switch word {
	case "running", "active":
		return flagClause("active"), true
	case "success", "succeeded":
		return flagClause("succeeded"), true
	case "fail", "failed", "failure":
		return flagClause("failed"), true
	case "pending":
		// No outcome recorded yet — either the status is still empty (the runner
		// has not reported) or none of the three flags is set. The old
		// client-side filter treated exactly these as pending, so the search box
		// has to keep finding them. A plain LIKE is correct on the negated side:
		// any occurrence of the flag means there is activity, whatever its value.
		return `(status IS NULL OR status = '' OR ` +
			`(status NOT LIKE '%"active":1%' AND status NOT LIKE '%"succeeded":1%' AND status NOT LIKE '%"failed":1%'))`, true
	}
	return "", false
}

// flagClause matches `"flag":1` in the persisted status JSON. The value is
// always a 0/1 flag today, but a bare LIKE '%"flag":1%' would also match
// ":10", ":11", … should that ever change, so the digit is anchored to the
// JSON separator that necessarily follows it (a "," or the closing "}").
func flagClause(flag string) string {
	return strings.ReplaceAll(
		`(status LIKE '%"__FLAG__":1,%' OR status LIKE '%"__FLAG__":1}%')`,
		"__FLAG__", flag,
	)
}

// ListRunningJobs returns not-yet-completed jobs for a project, excluding one
// job by name. Scoped to recent jobs (by the name timestamp) so a zombie row
// that never reported terminal status doesn't count as "running" forever.
func (r *Repository) ListRunningJobs(projectId, excludeName string, days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	err := r.db.Where(
		"project_id = ? AND name <> ? AND completed = ? AND "+jobTimestampExpr()+" >= ?",
		projectId, excludeName, false, cutoffDays(days),
	).Order("id DESC").Find(&jobs).Error
	return jobs, err
}

// ListUncompletedJobs returns not-yet-completed recent jobs (no Pods preload),
// scoped by the name timestamp like the other listing helpers. Used
// by the reconciler to find jobs whose runner never reported a final status.
func (r *Repository) ListUncompletedJobs(days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	err := r.db.Where("completed = ? AND "+jobTimestampExpr()+" >= ?", false, cutoffDays(days)).
		Order("id DESC").Find(&jobs).Error
	return jobs, err
}

// ListStuckCompletedJobs returns recent jobs that are marked completed in the
// DB but whose status is still non-terminal (no succeeded/failed outcome).
// These are rows the runner's final report should have made terminal but that
// were left stuck as "running" by the handleStatus race (or never reported at
// all — an empty status); the reconciler heals them by deriving a terminal
// outcome from the K8s Job.
func (r *Repository) ListStuckCompletedJobs(days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	// Only name + status are needed: name keys the K8s lookup in the reconciler
	// and status decides "stuck". Pulling notify/spec (large text blobs) would
	// inflate this per-tick scan over all recently-completed jobs for nothing.
	err := r.db.Select("name", "status").
		Where("completed = ? AND "+jobTimestampExpr()+" >= ?", true, cutoffDays(days)).
		Order("id DESC").Find(&jobs).Error
	if err != nil {
		return nil, err
	}
	stuck := make([]PipelineJob, 0, len(jobs))
	for _, j := range jobs {
		if isStuckJobStatus(j.Status) {
			stuck = append(stuck, j)
		}
	}
	return stuck, nil
}

// isStuckJobStatus reports whether a persisted status value represents a job
// that has no terminal outcome — and is therefore a candidate for the
// reconciler to heal from the K8s Job. Both an empty status (the runner never
// reported anything, e.g. checkout conflict / image pull failure) and a parsed
// status with no succeeded/failed flag count as stuck. Corrupted non-empty JSON
// is not treated as stuck: its outcome is unknowable and not clearly a
// "running" state.
func isStuckJobStatus(statusJSON string) bool {
	if statusJSON == "" {
		return true
	}
	var status JobStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return false
	}
	return status.Succeeded == 0 && status.Failed == 0
}

// GetJobProjectId returns only the project_id for a job by name, avoiding the
// cost of preloading Pods and the full status field.
func (r *Repository) GetJobProjectId(name string) (string, error) {
	var job PipelineJob
	result := r.db.Select("project_id").Where("name = ?", name).First(&job)
	if result.Error != nil {
		return "", result.Error
	}
	return job.ProjectId, nil
}

func (r *Repository) GetJobByName(name string) (*PipelineJob, error) {
	var job PipelineJob
	result := r.db.Where("name = ?", name).Preload("Pods").First(&job)
	if result.Error != nil {
		return nil, result.Error
	}
	return &job, nil
}

func (r *Repository) AddPod(pod PipelinePod) error {
	return r.db.Create(&pod).Error
}

func (r *Repository) UpdatePodStatus(podUid string, phase string) error {
	return r.db.Model(&PipelinePod{}).Where("pod_uid = ?", podUid).Update("phase", phase).Error
}

func (r *Repository) MarkJobCompleted(jobName string) error {
	now := time.Now()
	return r.db.Model(&PipelineJob{}).Where("name = ?", jobName).
		Updates(map[string]interface{}{
			"completed":    true,
			"completed_at": now,
		}).Error
}

func (r *Repository) SetJobReportUrl(jobName string, reportUrl string) error {
	now := time.Now()
	var existing JobReport
	result := r.db.Where("job_name = ?", jobName).First(&existing)
	if result.Error != nil {
		return r.db.Create(&JobReport{
			JobName:   jobName,
			ReportUrl: reportUrl,
			CreatedAt: &now,
		}).Error
	}
	return r.db.Model(&existing).Updates(map[string]interface{}{
		"report_url": reportUrl,
		"created_at": now,
	}).Error
}

func (r *Repository) GetJobReportUrl(jobName string) (string, error) {
	var report JobReport
	result := r.db.Where("job_name = ?", jobName).First(&report)
	if result.Error != nil {
		return "", result.Error
	}
	return report.ReportUrl, nil
}

// --- Snippet CRUD ---

func (r *Repository) ListSnippets() ([]Snippet, error) {
	var snippets []Snippet
	err := r.db.Order("name").Find(&snippets).Error
	return snippets, err
}

func (r *Repository) GetSnippetByName(name string) (*Snippet, error) {
	var snippet Snippet
	result := r.db.Where("name = ?", name).First(&snippet)
	if result.Error != nil {
		return nil, result.Error
	}
	return &snippet, nil
}

func (r *Repository) CreateSnippet(snippet Snippet) error {
	return r.db.Create(&snippet).Error
}

func (r *Repository) UpdateSnippet(name string, updates map[string]interface{}) error {
	updates["updated_at"] = time.Now()
	return r.db.Model(&Snippet{}).Where("name = ?", name).Updates(updates).Error
}

func (r *Repository) DeleteSnippet(name string) error {
	return r.db.Where("name = ?", name).Delete(&Snippet{}).Error
}

// ReplaceAllSnippets atomically replaces every row in neutron_snippet with the
// given slice inside a single transaction (DELETE all + INSERT all). An empty
// slice clears the cache. Used by the GitLab-backed snippet sync to refresh the
// local read cache.
func (r *Repository) ReplaceAllSnippets(snippets []Snippet) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("1 = 1").Delete(&Snippet{}).Error; err != nil {
			return err
		}
		if len(snippets) == 0 {
			return nil
		}
		return tx.Create(&snippets).Error
	})
}

// --- Settings ---

// GetSetting returns the value for a key, or an empty string if the key is unset.
func (r *Repository) GetSetting(key string) (string, error) {
	var setting Setting
	result := r.db.Where("`key` = ?", key).First(&setting)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", result.Error
	}
	return setting.Value, nil
}

// SetSetting upserts a key/value pair, stamping updated_at.
func (r *Repository) SetSetting(key, value string) error {
	now := time.Now()
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "updated_at"}),
	}).Create(&Setting{Key: key, Value: value, UpdatedAt: &now}).Error
}
