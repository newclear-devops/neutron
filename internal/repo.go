package internal

import (
	_ "embed"
	"encoding/json"
	"errors"
	"log"
	"neutron/internal/model"
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
	Id          int64         `gorm:"column:id;primaryKey;autoIncrement"`
	ProjectId   string        `gorm:"column:project_id"`
	Name        string        `gorm:"column:name;type:varchar(255);uniqueIndex"`
	Status      string        `gorm:"column:status;type:text"`
	Notify      string        `gorm:"column:notify;type:text"` // JSON-encoded model.Notify, captured at trigger time
	Spec        string        `gorm:"column:spec;type:text"`   // JSON-encoded model.JobSpec for rerun; empty for API-triggered jobs
	Completed   bool          `gorm:"column:completed;default:false"`
	CompletedAt *time.Time    `gorm:"column:completed_at"`
	Pods        []PipelinePod `gorm:"foreignKey:JobId"`
}

func (PipelineJob) TableName() string {
	return "neutron_job"
}

type PipelinePod struct {
	Id     int64  `gorm:"column:id;primaryKey;autoIncrement"`
	JobId  int64  `gorm:"column:job_id;index"`
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
}

type Repository struct {
	db *gorm.DB
}

func NewRepository(config model.Config) *Repository {
	db, err := gorm.Open(mysql.Open(config.Database), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
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

// cutoffDays returns the timestamp suffix used to filter job names by recency.
// Job names follow the format neutron-<job>-YYYYMMDD-HHMMSS; the last 15 chars
// encode the timestamp and can be compared as a string.
func cutoffDays(days int) string {
	return time.Now().AddDate(0, 0, -days).Format("20060102") + "-000000"
}

func (r *Repository) ListProjectJobs(projectId string, days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	err := r.db.Where("project_id = ? AND RIGHT(name, 15) >= ?", projectId, cutoffDays(days)).
		Order("id DESC").Preload("Pods").Find(&jobs).Error
	return jobs, err
}

func (r *Repository) ListAllRecentJobs(days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	err := r.db.Where("RIGHT(name, 15) >= ?", cutoffDays(days)).
		Order("id DESC").Preload("Pods").Find(&jobs).Error
	return jobs, err
}

// ListRunningJobs returns not-yet-completed jobs for a project, excluding one
// job by name. Scoped to recent jobs (RIGHT(name,15) timestamp) so a zombie row
// that never reported terminal status doesn't count as "running" forever.
func (r *Repository) ListRunningJobs(projectId, excludeName string, days int) ([]PipelineJob, error) {
	var jobs []PipelineJob
	err := r.db.Where(
		"project_id = ? AND name <> ? AND completed = ? AND RIGHT(name, 15) >= ?",
		projectId, excludeName, false, cutoffDays(days),
	).Order("id DESC").Find(&jobs).Error
	return jobs, err
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
