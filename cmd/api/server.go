package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"gorm.io/gorm"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"neutron/internal"
	"neutron/internal/ccwork"
	"neutron/internal/codeup"
	"neutron/internal/gitlab"
	"neutron/internal/launcher"
	"neutron/internal/model"
	"neutron/internal/notify"
	"neutron/internal/parser"
)

// Server holds the dependencies shared by all HTTP handlers.
type Server struct {
	config       model.Config
	repo         *internal.Repository
	clientSet    kubernetes.Interface
	notifyClient *notify.Client
	ccworkRobot  *ccwork.Robot
}

// NewServer wires the server dependencies together.
func NewServer(config model.Config, repo *internal.Repository, clientSet kubernetes.Interface, notifyClient *notify.Client, ccworkRobot *ccwork.Robot) *Server {
	return &Server{
		config:       config,
		repo:         repo,
		clientSet:    clientSet,
		notifyClient: notifyClient,
		ccworkRobot:  ccworkRobot,
	}
}

// registerRoutes registers all API and webhook routes on the given engine.
func (s *Server) registerRoutes(r *gin.Engine) {
	r.GET("/api/config", s.handleConfig)
	r.GET("/api/projects", s.handleListProjects)
	r.GET("/api/projects/:id/jobs", s.handleListProjectJobs)
	r.GET("/api/projects/:id/job-names", s.handleListProjectJobNames)
	// Manual-trigger toolbar inputs: the project's branch/tag lists (through
	// the codebase gateway) and the shared dependency branches.
	r.GET("/api/projects/:id/branches", s.handleProjectBranches)
	r.GET("/api/projects/:id/tags", s.handleProjectTags)
	r.GET("/api/dependency/branches", s.handleDependencyBranches)
	r.GET("/api/dependency/default", s.handleDependencyDefault)
	r.GET("/api/jobs/recent", s.handleRecentJobs)
	r.GET("/api/jobs/:jobName/running-siblings", s.handleRunningSiblings)
	r.POST("/api/register", s.handleRegister)
	r.GET("/api/status/:jobName", s.handleStatus)
	r.POST("/api/report/:jobName", s.handleReport)
	r.POST("/api/report/:jobName/pod", s.handleReportPod)
	r.POST("/api/report/:jobName/link", s.handleReportLink)
	r.POST("/api/jobs/:jobName/rerun", s.handleRerun)
	r.POST("/webhook/:id", s.handleWebhook)
	r.POST("/api/trigger", s.handleTrigger)
}

func (s *Server) handleConfig(c *gin.Context) {
	codebaseUrls := make(map[string]string)
	for k, v := range s.config.BaseConfig {
		codebaseUrls[k] = v.Url
	}
	c.JSON(http.StatusOK, gin.H{
		"logUrl":       s.config.LogUrl,
		"namespace":    s.config.Kubernetes.Namespace,
		"codebaseUrls": codebaseUrls,
	})
}

func (s *Server) handleListProjects(c *gin.Context) {
	projects, err := s.repo.ListProjects()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"projects": projects})
}

// parsePageParams reads the page/page_size query params, clamping them so a
// caller cannot defeat pagination by asking for a single enormous page.
func parsePageParams(c *gin.Context) (page, pageSize int) {
	page, pageSize = 1, internal.DefaultPageSize
	if v, err := strconv.Atoi(c.Query("page")); err == nil && v > 0 {
		page = v
	}
	if v, err := strconv.Atoi(c.Query("page_size")); err == nil && v > 0 {
		pageSize = v
	}
	if pageSize > internal.MaxPageSize {
		pageSize = internal.MaxPageSize
	}
	if page > internal.MaxPage {
		page = internal.MaxPage
	}
	return page, pageSize
}

// defaultJobWindowDays is the recency window every job listing falls back to.
// The project page can bypass it with ?all=1 to page through the full history;
// see jobWindowDays.
const defaultJobWindowDays = 7

// jobWindowDays reports how far back the project job list should look: the
// default window, or 0 (no limit) when the caller asked for the full history.
// 0 is what the repository helpers read as "drop the recency window".
func jobWindowDays(c *gin.Context) int {
	if c.Query("all") == "1" {
		return 0
	}
	return defaultJobWindowDays
}

func (s *Server) handleListProjectJobs(c *gin.Context) {
	id := c.Param("id")
	page, pageSize := parsePageParams(c)
	jobs, total, err := s.repo.ListProjectJobsPaged(id, c.Query("job_name"), jobWindowDays(c), pageSize, page)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"jobs":      jobs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// handleListProjectJobNames feeds the job-name filter on the project page. It is
// a separate endpoint because the dropdown used to be built client-side from
// every row — impossible once the list is paginated. The lookup is deliberately
// unbounded (days=0) so a job that has not run inside the default window is
// still selectable.
func (s *Server) handleListProjectJobNames(c *gin.Context) {
	names, err := s.repo.ListProjectJobNames(c.Param("id"), 0)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job_names": names})
}

func (s *Server) handleRecentJobs(c *gin.Context) {
	page, pageSize := parsePageParams(c)
	jobs, total, err := s.repo.ListRecentJobsPaged(c.Query("q"), defaultJobWindowDays, pageSize, page)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"jobs":      jobs,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

// handleRunningSiblings reports whether other, still-running jobs exist in the
// same project as :jobName. The project is derived from the (unique) job name,
// so no project_id param is needed. A job is "running" when it has not been
// marked completed (success or failure both count as completed).
//
// Caveat: a job whose pod crashed without reporting terminal status (OOMKill,
// node failure, eviction) stays completed=false in the DB. To mitigate this,
// each candidate is cross-checked against the K8s API: only jobs with at least
// one active pod are reported as running. Jobs with no live pod are excluded.
func (s *Server) handleRunningSiblings(c *gin.Context) {
	jobName := c.Param("jobName")
	projectId, err := s.repo.GetJobProjectId(jobName)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		} else {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		}
		return
	}
	siblings, err := s.repo.ListRunningJobs(projectId, jobName, defaultJobWindowDays)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Cross-check against K8s: a job whose pod crashed without reporting
	// (OOMKill, node failure, eviction) stays completed=false in the DB.
	// We only consider a sibling "running" if it has at least one active pod
	// in K8s. Jobs not found in K8s, already finished, or without active pods
	// are excluded — this prevents a single crash from creating a phantom
	// "running" sibling that blocks future deployments.
	alive := make([]internal.PipelineJob, 0, len(siblings))
	for _, sib := range siblings {
		k8sJob, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), sib.Name, metav1.GetOptions{})
		if err != nil || k8sJob.Status.Active == 0 {
			continue
		}
		alive = append(alive, sib)
	}
	c.JSON(http.StatusOK, gin.H{
		"project_id": projectId,
		"running":    len(alive) > 0,
		"count":      len(alive),
		"jobs":       alive,
	})
}

func (s *Server) handleRegister(c *gin.Context) {
	p := internal.PipelineProject{
		Id:          uuid.New().String(),
		WebhookType: c.PostForm("webhookType"),
		RepoUrl:     c.PostForm("repoUrl"),
	}
	if err := s.repo.AddWebhookConfig(p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Determine webhook URL: use platform-specific URL if configured, otherwise use config.Host
	webhookHost := s.config.Host
	if cb, ok := s.config.BaseConfig[p.WebhookType]; ok && cb.WebhookUrl != "" {
		webhookHost = cb.WebhookUrl
	}
	webhookUrl := fmt.Sprintf("%s/webhook/%s", webhookHost, p.Id)
	c.JSON(http.StatusOK, gin.H{
		"id":          p.Id,
		"webhookType": p.WebhookType,
		"repoUrl":     p.RepoUrl,
		"webhookUrl":  webhookUrl,
	})
}

func (s *Server) handleStatus(c *gin.Context) {
	jobName := c.Param("jobName")

	// Check if job is completed in database - if yes, return from DB only
	dbJob, dbErr := s.repo.GetJobByName(jobName)
	if dbErr == nil && dbJob.Completed {
		c.JSON(http.StatusOK, s.jobStatusFromDB(dbJob))
		return
	}

	// Job not completed, fetch from K8s
	jobClient := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace)
	job, err := jobClient.Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		// The row exists but its K8s Job is gone — finished Jobs are deleted
		// by the K8s TTL controller (kubernetes.job-ttl-seconds), and manual
		// deletes work too. Serve what the DB has rather than erroring: it is
		// the only record left. Note this is read-only — the row is NOT marked
		// completed, so a later reconcile still gets a chance if the Job shows
		// up again.
		if dbErr == nil {
			log.Printf("status %s: K8s Job not found (%v), serving last known status from DB", jobName, err)
			c.JSON(http.StatusOK, s.jobStatusFromDB(dbJob))
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	podClient := s.clientSet.CoreV1().Pods(s.config.Kubernetes.Namespace)
	selector, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: job.Spec.Selector.MatchLabels,
	})
	pods, err := podClient.List(context.Background(), metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Derive status from K8s job
	ann := job.Annotations
	k8sStatus := internal.JobStatus{
		WebhookType: ann["sourceType"],
		TriggerType: ann["triggerType"],
		RepoUrl:     ann["gitPath"],
		ProjectUrl:  ann["sourceLink"],
		SourceUrl:   ann["sourceUrl"],
	}
	if job.Status.Active > 0 {
		k8sStatus.Active = 1
	}
	if job.Status.Succeeded > 0 {
		k8sStatus.Succeeded = 1
	}
	if job.Status.Failed > 0 {
		k8sStatus.Failed = 1
	}
	// Don't clobber an already-terminal DB status with a stale K8s read. The
	// runner's final report (final:true) is authoritative for the outcome; a
	// status poll racing it can read the K8s Job while it still shows Active>0
	// and overwrite the terminal succeeded/failed with active:1, leaving the
	// job stuck as "running" once it is marked completed.
	//
	// The status is re-read here (rather than trusting dbJob.Status from the
	// top of this handler) because the runner's final report can land between
	// the initial read and this write. This is best-effort — the reconciler's
	// healStuckCompletedJobs is the guarantee that such jobs converge — but it
	// removes the common race where a poll writes active:1 over a terminal
	// outcome it simply hadn't observed yet.
	//
	// The guard requires Final, matching isFinalReport. Per-step reports also
	// carry succeeded/failed without Final; treating those as terminal would
	// freeze a mid-pipeline {succeeded:1} over K8s's active:1 and briefly show
	// a still-running multi-step job as "succeeded".
	if existing, err := s.repo.GetJobStatus(jobName); err == nil &&
		existing.Final && (existing.Succeeded > 0 || existing.Failed > 0) {
		k8sStatus = existing
	}
	// Update database with derived status
	_ = s.repo.UpdateJobStatus(jobName, k8sStatus)

	// Store pod info in database
	if dbErr == nil {
		for _, pod := range pods.Items {
			// Check if pod already exists
			var existingPod internal.PipelinePod
			result := s.repo.DB().Where("job_id = ? AND pod_uid = ?", dbJob.Id, string(pod.UID)).First(&existingPod)
			if result.Error != nil {
				// Pod doesn't exist, create it
				_ = s.repo.AddPod(internal.PipelinePod{
					JobId:   dbJob.Id,
					PodName: pod.Name,
					PodUid:  string(pod.UID),
					Phase:   string(pod.Status.Phase),
				})
			} else {
				// Update existing pod status
				_ = s.repo.UpdatePodStatus(string(pod.UID), string(pod.Status.Phase))
			}
		}
	}

	// If job is completed, mark it in database
	if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
		_ = s.repo.MarkJobCompleted(jobName)
	}

	var reportUrl string
	if url, err := s.repo.GetJobReportUrl(jobName); err == nil {
		reportUrl = url
	}
	var projectId string
	var jobKey string
	var params *model.JobParams
	if dbErr == nil {
		projectId = dbJob.ProjectId
		jobKey = dbJob.JobName
		params = parseParams(dbJob.Params)
	}
	c.JSON(http.StatusOK, gin.H{
		"jobName":    jobName,
		"status":     k8sStatus,
		"job":        job,
		"pods":       pods,
		"source":     "kubernetes",
		"reportUrl":  reportUrl,
		"rerunnable": dbErr == nil && dbJob.Spec != "",
		"projectId":  projectId,
		"jobKey":     jobKey,
		"params":     params,
	})
}

// jobStatusFromDB renders a status response entirely from a DB row, in the same
// shape the K8s branch produces. Used for completed jobs (cheaper and
// authoritative, since the K8s Job may already be TTL-cleaned) and as the
// fallback when the K8s Job no longer exists.
//
// stuck=true tells the caller that the row is not completed and its K8s Job is
// gone: with no terminal outcome recorded anywhere, nothing can move it forward
// any more (see healStuckCompletedJobs in cmd/api/reconcile.go).
func (s *Server) jobStatusFromDB(dbJob *internal.PipelineJob) gin.H {
	var status internal.JobStatus
	_ = json.Unmarshal([]byte(dbJob.Status), &status)
	// Convert pods to K8s-like format for frontend compatibility
	var podItems []gin.H
	for _, pod := range dbJob.Pods {
		podItems = append(podItems, gin.H{
			"metadata": gin.H{"name": pod.PodName, "uid": pod.PodUid},
			"status":   gin.H{"phase": pod.Phase},
		})
	}
	var reportUrl string
	if url, err := s.repo.GetJobReportUrl(dbJob.Name); err == nil {
		reportUrl = url
	}
	return gin.H{
		"jobName":    dbJob.Name,
		"status":     status,
		"job":        gin.H{"metadata": gin.H{"name": dbJob.Name}},
		"pods":       gin.H{"items": podItems},
		"source":     "database",
		"reportUrl":  reportUrl,
		"rerunnable": dbJob.Spec != "",
		"projectId":  dbJob.ProjectId,
		"stuck":      !dbJob.Completed,
		// Trigger context (ref/env) for the detail box. Empty on rows created
		// before the column existed.
		"jobKey": dbJob.JobName,
		"params": parseParams(dbJob.Params),
	}
}

// isFinalReport reports whether a status payload carries the job-level terminal
// flag with a terminal outcome. Only such reports trigger the completion
// notification and MarkJobCompleted; per-step reports (which also carry
// succeeded/failed) must not.
func isFinalReport(status internal.JobStatus) bool {
	return status.Final && (status.Succeeded > 0 || status.Failed > 0)
}

func (s *Server) handleReport(c *gin.Context) {
	jobName := c.Param("jobName")
	var status internal.JobStatus
	if err := c.ShouldBindJSON(&status); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	// Preserve existing SourceUrl from DB if not provided in report (runners don't send it)
	if status.SourceUrl == "" {
		if oldStatus, err := s.repo.GetJobStatus(jobName); err == nil {
			status.SourceUrl = oldStatus.SourceUrl
		}
	}
	// Fallback: load SourceUrl from K8s Job annotations if still empty
	if status.SourceUrl == "" {
		if k8sJob, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), jobName, metav1.GetOptions{}); err == nil {
			if srcUrl := k8sJob.Annotations["sourceUrl"]; srcUrl != "" {
				status.SourceUrl = srcUrl
			}
		}
	}
	if err := s.repo.UpdateJobStatus(jobName, status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Sync pod phase from K8s on every non-terminal report
	if status.Succeeded == 0 && status.Failed == 0 {
		if dbJob, err := s.repo.GetJobByName(jobName); err == nil {
			for _, pod := range dbJob.Pods {
				if k8sPod, err := s.clientSet.CoreV1().Pods(s.config.Kubernetes.Namespace).Get(context.Background(), pod.PodName, metav1.GetOptions{}); err == nil {
					_ = s.repo.UpdatePodStatus(pod.PodUid, string(k8sPod.Status.Phase))
				}
			}
		}
	}
	// A final, job-level terminal report (sent by the runner exactly once,
	// after the last step) marks the job completed and triggers the completion
	// notification. Per-step reports no longer trigger either — previously
	// every step's terminal status was mistaken for job completion.
	if isFinalReport(status) {
		failed := status.Failed > 0
		if dbJob, err := s.repo.GetJobByName(jobName); err == nil {
			s.notifyJobCompleted(dbJob, failed, status.Description)
		} else {
			log.Printf("notify: job %s not found, skipping completion notification: %v", jobName, err)
		}
		finalPhase := "Succeeded"
		if failed {
			finalPhase = "Failed"
		}
		go func() {
			// Retry until pod reaches terminal phase or is gone
			for i := 0; i < 5; i++ {
				time.Sleep(2 * time.Second)
				if dbJob, err := s.repo.GetJobByName(jobName); err == nil {
					allSynced := true
					for _, pod := range dbJob.Pods {
						k8sPod, err := s.clientSet.CoreV1().Pods(s.config.Kubernetes.Namespace).Get(context.Background(), pod.PodName, metav1.GetOptions{})
						if err != nil {
							// Pod gone — use runner's reported phase
							_ = s.repo.UpdatePodStatus(pod.PodUid, finalPhase)
							continue
						}
						phase := string(k8sPod.Status.Phase)
						if phase == "Succeeded" || phase == "Failed" {
							_ = s.repo.UpdatePodStatus(pod.PodUid, phase)
						} else {
							allSynced = false
						}
					}
					if allSynced {
						break
					}
				}
			}
			_ = s.repo.MarkJobCompleted(jobName)
		}()
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleReportPod(c *gin.Context) {
	jobName := c.Param("jobName")

	var req struct {
		PodName   string `json:"pod_name"`
		Namespace string `json:"namespace"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// Look up job in database
	dbJob, dbErr := s.repo.GetJobByName(jobName)
	if dbErr != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}

	// Get pod details from K8s
	podClient := s.clientSet.CoreV1().Pods(req.Namespace)
	pod, err := podClient.Get(context.Background(), req.PodName, metav1.GetOptions{})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("pod not found: %v", err)})
		return
	}

	// Upsert pod info
	var existingPod internal.PipelinePod
	result := s.repo.DB().Where("job_id = ? AND pod_uid = ?", dbJob.Id, string(pod.UID)).First(&existingPod)
	if result.Error != nil {
		_ = s.repo.AddPod(internal.PipelinePod{
			JobId:   dbJob.Id,
			PodName: pod.Name,
			PodUid:  string(pod.UID),
			Phase:   string(pod.Status.Phase),
		})
	} else {
		_ = s.repo.UpdatePodStatus(string(pod.UID), string(pod.Status.Phase))
	}

	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (s *Server) handleReportLink(c *gin.Context) {
	jobName := c.Param("jobName")

	var req struct {
		ReportUrl string `json:"report_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ReportUrl == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "report_url is required"})
		return
	}

	if err := s.repo.SetJobReportUrl(jobName, req.ReportUrl); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// handleRerun reruns a previously webhook-created job by recreating an
// identical K8s Job from its persisted spec (same commit, params, and trigger,
// so it reports to the platform like the original run). It produces a new job
// record; the original is untouched.
func (s *Server) handleRerun(c *gin.Context) {
	jobName := c.Param("jobName")

	dbJob, err := s.repo.GetJobByName(jobName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	spec, ok := parseSpec(dbJob.Spec)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "job is not rerunnable (no spec; only webhook jobs can be rerun)"})
		return
	}
	if _, ok := s.config.BaseConfig[spec.Platform]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s codebase not configured", spec.Platform)})
		return
	}

	notify := parseNotify(dbJob.Notify)
	createdName, err := s.createJobFromSpec(dbJob.ProjectId, spec, notify)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to rerun job: %v", err)})
		return
	}

	// Notify the job's targets: rerun triggered
	statusUrl := fmt.Sprintf("%s/#/status/%s", s.config.Host, createdName)
	title := "🔁 流水线重跑通知"
	content := fmt.Sprintf("📂 项目: %s\n📋 任务: %s\n♻️ 重跑自: %s\n🔄 触发: %s\n🔗 查看: %s", spec.GitRepoUrl, createdName, jobName, spec.Trigger, statusUrl)
	if spec.SourceUrl != "" {
		content += fmt.Sprintf("\n📎 源码: %s", spec.SourceUrl)
	}
	s.sendJobNotifications(notify, title, content)

	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"job_name": createdName,
		"job_url":  statusUrl,
	})
}

// parsedHook holds the platform-agnostic result of parsing an incoming webhook.
type parsedHook struct {
	pipeline     model.Pipeline
	trigger      string
	codeSha      string
	reportSha    string
	targetBranch string
	codeRef      string
	sourceUrl    string
	projectId    int
}

// parseWebhook parses a GitLab or Codeup webhook body and normalizes the
// fields the launcher and notifications need.
func parseWebhook(platform string, body io.ReadCloser, cb model.CodeBase, repoUrl string) (parsedHook, error) {
	var ph parsedHook
	switch platform {
	case "GitLab":
		p, err := gitlab.NewGitLabParser(body, cb.Url, cb.Token, cb.SkipTLSVerify)
		if err != nil {
			return ph, err
		}
		// Populate webhook-derived fields first; these come from the parsed body
		// and are valid even if the neutron.yaml fetch below fails (e.g. 404),
		// so the caller can still fall back to a default pipeline.
		ph.trigger = p.Trigger
		ph.codeSha = p.CodeSha
		ph.reportSha = p.ReportSha
		ph.targetBranch = p.TargetBranch
		ph.codeRef = codeRefForTrigger(p.Trigger, p.Request.Ref)
		ph.projectId = p.Request.Project.Id
		ph.sourceUrl = parser.BuildSourceUrl("GitLab", p.Trigger, cb.Url, repoUrl, p.Request.Ref, p.CodeSha, p.Request.Attributes.Iid)
		pipeline, err := p.Parse()
		if err != nil {
			return ph, fmt.Errorf("failed to parse pipeline: %w", err)
		}
		ph.pipeline = pipeline
	case "Codeup":
		p, err := codeup.NewCodeupParser(body, cb.Url, cb.Token, cb.SkipTLSVerify)
		if err != nil {
			return ph, err
		}
		ph.trigger = p.Trigger
		ph.codeSha = p.CodeSha
		ph.reportSha = p.ReportSha
		ph.targetBranch = p.TargetBranch
		ph.codeRef = codeRefForTrigger(p.Trigger, p.Request.Ref)
		ph.projectId = p.Request.Project.Id
		if ph.projectId == 0 {
			ph.projectId = p.Request.ProjectId
		}
		if ph.projectId == 0 {
			ph.projectId = p.Request.Attributes.ProjectId
		}
		ph.sourceUrl = parser.BuildSourceUrl("Codeup", p.Trigger, cb.Url, repoUrl, p.Request.Ref, p.CodeSha, p.Request.Attributes.Iid)
		pipeline, err := p.Parse()
		if err != nil {
			return ph, fmt.Errorf("failed to parse pipeline: %w", err)
		}
		ph.pipeline = pipeline
	default:
		return ph, fmt.Errorf("unsupported platform: %s", platform)
	}
	return ph, nil
}

// defaultPipeline loads and parses the globally-configured default pipeline,
// used as a fallback when a repository has no neutron.yaml. It errors when no
// default is configured so the caller can surface a clear 4xx.
func (s *Server) defaultPipeline() (model.Pipeline, error) {
	content, err := s.repo.GetSetting(internal.SettingDefaultPipeline)
	if err != nil {
		return model.Pipeline{}, fmt.Errorf("failed to load default pipeline: %w", err)
	}
	if strings.TrimSpace(content) == "" {
		return model.Pipeline{}, fmt.Errorf("neutron.yaml not found and no default pipeline configured")
	}
	var pipeline model.Pipeline
	if err := yaml.Unmarshal([]byte(content), &pipeline); err != nil {
		return model.Pipeline{}, fmt.Errorf("default pipeline is invalid yaml: %w", err)
	}
	for name, job := range pipeline.Jobs {
		log.Printf("default pipeline: job %q triggers=%v", name, job.Trigger)
	}
	log.Printf("default pipeline loaded: %d job(s)", len(pipeline.Jobs))
	return pipeline, nil
}

func (s *Server) handleWebhook(c *gin.Context) {
	id := c.Param("id")
	webhookConfig := s.repo.GetWebhookConfig(id)
	if webhookConfig.Id == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "webhook not found"})
		return
	}

	platform := webhookConfig.WebhookType
	if _, ok := s.config.BaseConfig[platform]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("%s codebase not configured", platform)})
		return
	}

	ph, err := parseWebhook(platform, c.Request.Body, s.config.BaseConfig[platform], webhookConfig.RepoUrl)
	if err != nil {
		// Fall back to the configured default pipeline when the repo has no
		// neutron.yaml; other errors (auth, network, malformed) still fail.
		if errors.Is(err, parser.ErrPipelineNotFound) {
			defaultPipeline, derr := s.defaultPipeline()
			if derr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": derr.Error()})
				return
			}
			log.Printf("webhook %s: repo %s has no neutron.yaml, using default pipeline", id, webhookConfig.RepoUrl)
			ph.pipeline = defaultPipeline
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	jobNames := make([]string, 0, len(ph.pipeline.Jobs))
	for name := range ph.pipeline.Jobs {
		jobNames = append(jobNames, name)
	}
	log.Printf("webhook %s: trigger=%s, resolved pipeline defines %d job(s): %v", id, ph.trigger, len(ph.pipeline.Jobs), jobNames)

	var jobs []string
	for jobName, job := range ph.pipeline.Jobs {
		if !isValidTrigger(ph.trigger, job.Trigger) {
			log.Printf("webhook %s: skipping job %q — trigger %s not in %v", id, jobName, ph.trigger, job.Trigger)
			continue
		}

		// Build the rerun snapshot from this webhook's parsed inputs.
		spec := model.JobSpec{
			Platform:     platform,
			JobName:      jobName,
			Image:        job.Image,
			Resources:    job.Resources,
			ProjectId:    strconv.Itoa(ph.projectId),
			CommitSha:    ph.codeSha,
			ReportSha:    ph.reportSha,
			Trigger:      ph.trigger,
			GitRepoUrl:   webhookConfig.RepoUrl,
			TargetBranch: ph.targetBranch,
			CodeRef:      ph.codeRef,
			SourceUrl:    ph.sourceUrl,
			QueryParams:  firstQueryValues(c.Request.URL.Query()),
		}

		createdName, err := s.createJobFromSpec(id, spec, job.Notify)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		jobs = append(jobs, createdName)

		// Notify this job's targets: pipeline triggered
		statusUrl := fmt.Sprintf("%s/#/status/%s", s.config.Host, createdName)
		title := "🚀 流水线触发通知"
		content := fmt.Sprintf("📂 项目: %s\n📋 任务: %s\n🔄 触发: %s\n🔗 查看: %s", webhookConfig.RepoUrl, createdName, ph.trigger, statusUrl)
		if ph.sourceUrl != "" {
			content += fmt.Sprintf("\n📎 源码: %s", ph.sourceUrl)
		}
		s.sendJobNotifications(job.Notify, title, content)
	}

	if len(jobs) == 0 {
		log.Printf("webhook %s: no jobs launched — none of %v matched trigger %s", id, jobNames, ph.trigger)
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "pipeline": ph.pipeline, "jobs": jobs})
}

// launcherFromSpec rebuilds the RunnerConfig + extra env from a JobSpec and
// returns a configured launcher. Tokens/URLs are resolved from the current
// config (not the spec). This is the pure manifest-construction step shared by
// the webhook and rerun paths; it has no side effects, so it is unit-testable.
func (s *Server) launcherFromSpec(spec model.JobSpec) *launcher.Launcher {
	platform := spec.Platform
	baseCfg := s.config.BaseConfig[platform]
	if pod, ok := s.config.PodCodeBase[platform]; ok {
		baseCfg = pod
	}

	runnerConfig := model.RunnerConfig{
		CodebaseToken: baseCfg.Token,
		CodebaseUrl:   baseCfg.Url,
		ProjectId:     spec.ProjectId,
		CommitSha:     spec.CommitSha,
		ReportSha:     spec.ReportSha,
		JobName:       spec.JobName,
		Trigger:       spec.Trigger,
		GitRepoUrl:    spec.GitRepoUrl,
		GitPrivateKey: "/etc/ssh/id_rsa",
		TargetBranch:  spec.TargetBranch,
		CodeRef:       spec.CodeRef,
		SourceUrl:     spec.SourceUrl,
	}

	var extraEnv []v1.EnvVar
	extraEnv = append(extraEnv, v1.EnvVar{Name: "RUNNER_PLATFORM", Value: strings.ToLower(platform)})
	if baseCfg.SkipTLSVerify {
		extraEnv = append(extraEnv, v1.EnvVar{Name: "SKIP_TLS_VERIFY", Value: "true"})
	}
	if platform == "GitLab" && spec.TargetBranch != "" {
		extraEnv = append(extraEnv, v1.EnvVar{Name: "TARGET_BRANCH", Value: spec.TargetBranch})
	}
	for key, value := range spec.QueryParams {
		extraEnv = append(extraEnv, v1.EnvVar{Name: key, Value: value})
	}

	return s.buildLauncher(runnerConfig, spec.Image, spec.Resources, platform, extraEnv)
}

// createJobFromSpec builds the K8s Job from a JobSpec (via launcherFromSpec),
// creates it, and persists the DB row carrying the same spec (so the job can be
// rerun again). Returns the generated K8s Job name.
func (s *Server) createJobFromSpec(projectId string, spec model.JobSpec, notify *model.Notify) (string, error) {
	l := s.launcherFromSpec(spec)
	jobClient := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace)
	createdJob, err := jobClient.Create(context.Background(), l.CreateJob(s.config.Host), metav1.CreateOptions{})
	if err != nil {
		return "", err
	}
	if err := s.repo.AddJob(internal.PipelineJob{
		ProjectId: projectId,
		Name:      createdJob.Name,
		Status:    "",
		Notify:    marshalNotify(notify),
		Spec:      marshalSpec(spec),
		JobName:   spec.JobName,
		Params:    marshalParams(paramsFromSpec(spec)),
	}); err != nil {
		return "", err
	}
	return createdJob.Name, nil
}

// paramsFromSpec rebuilds the trigger context shown on the status page from the
// same snapshot the rerun path uses, so both read one source of truth.
func paramsFromSpec(spec model.JobSpec) model.JobParams {
	ref := spec.CodeRef
	if ref == "" {
		ref = spec.CommitSha
	}
	return model.JobParams{
		Ref:       ref,
		CommitSha: spec.CommitSha,
		Trigger:   spec.Trigger,
		SourceUrl: spec.SourceUrl,
		Env:       spec.QueryParams,
	}
}

func (s *Server) handleTrigger(c *gin.Context) {
	var req struct {
		RepoUrl string            `json:"repo_url"`
		JobName string            `json:"job_name"`
		Ref     string            `json:"ref"`
		Env     map[string]string `json:"env"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.RepoUrl == "" || req.JobName == "" || req.Ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "repo_url, job_name, and ref are required"})
		return
	}

	// Find project by repo URL
	project := s.repo.GetProjectByRepoUrl(req.RepoUrl)
	if project.Id == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "project not found for repo_url: " + req.RepoUrl})
		return
	}
	platform := project.WebhookType

	// Get platform config
	baseCfg, ok := s.config.BaseConfig[platform]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("platform %s not configured", platform)})
		return
	}
	podCfg := baseCfg
	if pod, ok := s.config.PodCodeBase[platform]; ok {
		podCfg = pod
	}

	// Fetch neutron.yaml from repo at given ref
	pipeline, err := parser.FetchPipeline(platform, req.RepoUrl, req.Ref, baseCfg.Url, baseCfg.Token, baseCfg.SkipTLSVerify)
	// defaultPipeline is loaded lazily, at most once, and reused for both the
	// file-level (404) and job-level fallbacks.
	var defaultPipeline *model.Pipeline
	if err != nil {
		// Fall back to the configured default pipeline when the repo has no neutron.yaml.
		if errors.Is(err, parser.ErrPipelineNotFound) {
			dp, derr := s.defaultPipeline()
			if derr != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": derr.Error()})
				return
			}
			defaultPipeline = &dp
			pipeline = dp
			log.Printf("trigger: repo %s has no neutron.yaml at ref %s, using default pipeline", req.RepoUrl, req.Ref)
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("failed to fetch pipeline: %v", err)})
			return
		}
	}

	// Resolve the requested job: the repo's neutron.yaml first, then the global
	// default pipeline. The repo job always wins for same-named jobs
	// (whole-job override, no field-level merge).
	job, ok := pipeline.Jobs[req.JobName]
	if !ok {
		if defaultPipeline == nil {
			if dp, derr := s.defaultPipeline(); derr == nil {
				defaultPipeline = &dp
			} else {
				log.Printf("trigger: job %q not in repo neutron.yaml and default pipeline unavailable: %v", req.JobName, derr)
			}
		}
		if defaultPipeline != nil {
			job, ok = model.ResolveJob(pipeline, *defaultPipeline, req.JobName)
		}
	}
	if !ok {
		jobNames := make([]string, 0, len(pipeline.Jobs))
		for name := range pipeline.Jobs {
			jobNames = append(jobNames, name)
		}
		log.Printf("trigger: repo=%s ref=%s job %q not found in resolved pipeline (available: %v)", req.RepoUrl, req.Ref, req.JobName, jobNames)
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("job '%s' not found in pipeline", req.JobName)})
		return
	}

	// Build runner config
	runnerConfig := model.RunnerConfig{
		CodebaseToken:      podCfg.Token,
		CodebaseUrl:        podCfg.Url,
		ProjectId:          "api",
		CommitSha:          req.Ref,
		ReportSha:          req.Ref,
		JobName:            req.JobName,
		Trigger:            "API",
		GitRepoUrl:         req.RepoUrl,
		GitPrivateKey:      "/etc/ssh/id_rsa",
		SkipTriggerCheck:   true,
		SkipPlatformReport: true,
		CodeRef:            codeRefForTrigger("API", req.Ref),
	}

	// Build extra env vars
	var extraEnv []v1.EnvVar
	extraEnv = append(extraEnv, v1.EnvVar{Name: "RUNNER_PLATFORM", Value: strings.ToLower(platform)})
	if podCfg.SkipTLSVerify {
		extraEnv = append(extraEnv, v1.EnvVar{Name: "SKIP_TLS_VERIFY", Value: "true"})
	}
	// Inject user-provided env vars
	for key, value := range req.Env {
		extraEnv = append(extraEnv, v1.EnvVar{Name: key, Value: value})
	}

	// Create K8s Job
	l := s.buildLauncher(runnerConfig, job.Image, job.Resources, platform, extraEnv)
	jobClient := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace)
	createdJob, err := jobClient.Create(context.Background(), l.CreateJob(s.config.Host), metav1.CreateOptions{})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to create job: %v", err)})
		return
	}

	// Save job to database
	if err := s.repo.AddJob(internal.PipelineJob{
		ProjectId: project.Id,
		Name:      createdJob.Name,
		Status:    "",
		Notify:    marshalNotify(job.Notify),
		// No Spec: API-triggered jobs are not rerunnable (they bypass the
		// trigger/platform-report behaviour a rerun would have to reproduce).
		// Params is what makes the ref and env visible on the status page.
		JobName: req.JobName,
		Params: marshalParams(model.JobParams{
			Ref:       req.Ref,
			CommitSha: req.Ref,
			Trigger:   "API",
			Env:       req.Env,
		}),
	}); err != nil {
		log.Printf("failed to save job to database: %v", err)
	}

	// Send notifications
	statusUrl := fmt.Sprintf("%s/#/status/%s", s.config.Host, createdJob.Name)
	title := "🚀 流水线触发通知 (API)"
	content := fmt.Sprintf("📂 项目: %s\n📋 作业: %s\n🏷️ Ref: %s\n🔗 查看: %s", req.RepoUrl, req.JobName, req.Ref, statusUrl)
	s.sendJobNotifications(job.Notify, title, content)

	c.JSON(http.StatusOK, gin.H{
		"status":   "ok",
		"job_name": createdJob.Name,
		"job_url":  statusUrl,
	})
}

// buildLauncher constructs a launcher with the K8s settings shared by the
// webhook and trigger flows.
func (s *Server) buildLauncher(rc model.RunnerConfig, image string, resources *model.Resources, platform string, extraEnv []v1.EnvVar) *launcher.Launcher {
	return launcher.NewLauncher(
		s.config.Kubernetes.Namespace,
		rc,
		s.config.Kubernetes.InitImage,
		s.config.Kubernetes.CheckoutImage,
		image,
		s.config.Kubernetes.GitPrivateKey,
		s.config.Kubernetes.ImagePullSecrets,
		platform,
		s.config.Kubernetes.PodApiUrl,
		s.config.Kubernetes.EffectiveJobTtlSeconds(),
		resources,
		extraEnv...,
	)
}

func isValidTrigger(currentTrigger string, validTriggers []string) bool {
	for _, trigger := range validTriggers {
		if trigger == currentTrigger {
			return true
		}
	}
	return false
}

// codeRefForTrigger returns the CODE_REF value: tag name for TAG, branch name for PUSH, empty for MR.
func codeRefForTrigger(trigger, ref string) string {
	if trigger == "MR" {
		return ""
	}
	return parser.ExtractRefName(ref)
}

// marshalNotify serializes a job's notify config to JSON for persistence on
// neutron_job. A nil config yields an empty string.
func marshalNotify(n *model.Notify) string {
	if n == nil {
		return ""
	}
	b, err := json.Marshal(n)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseNotify deserializes the notify config persisted on neutron_job. An empty
// or invalid value yields nil (no notifications).
func parseNotify(s string) *model.Notify {
	if s == "" {
		return nil
	}
	var n model.Notify
	if err := json.Unmarshal([]byte(s), &n); err != nil {
		return nil
	}
	return &n
}

// marshalParams serializes a job's trigger context to JSON for persistence on
// neutron_job. A zero-value config yields an empty string.
func marshalParams(p model.JobParams) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseParams deserializes the trigger context persisted on neutron_job. An
// empty or invalid value yields nil (the status page then shows nothing).
func parseParams(s string) *model.JobParams {
	if s == "" {
		return nil
	}
	var p model.JobParams
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		return nil
	}
	return &p
}

// marshalSpec serializes a job's rerun spec to JSON for persistence on
// neutron_job. A marshal error yields an empty string (job becomes non-rerunnable).
func marshalSpec(spec model.JobSpec) string {
	b, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	return string(b)
}

// parseSpec deserializes the rerun spec persisted on neutron_job. An empty or
// invalid value yields ok=false.
func parseSpec(s string) (model.JobSpec, bool) {
	var spec model.JobSpec
	if s == "" {
		return spec, false
	}
	if err := json.Unmarshal([]byte(s), &spec); err != nil {
		return spec, false
	}
	return spec, true
}

// firstQueryValues flattens url.Values to a single value per key, matching the
// webhook handler's original behaviour of taking values[0].
func firstQueryValues(q url.Values) map[string]string {
	if len(q) == 0 {
		return nil
	}
	m := make(map[string]string, len(q))
	for key, values := range q {
		if len(values) > 0 {
			m[key] = values[0]
		}
	}
	return m
}
