package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"neutron/internal/model"
	"os"
	"time"
)

type message struct {
	State       string `json:"state"`
	TargetUrl   string `json:"target_url"`
	Description string `json:"description"`
	Context     string `json:"context"`
}

type GitlabReporter struct {
	client    *http.Client
	url       string
	token     string
	targetUrl string
}

func NewGitlabReporterFromEnv(skipTLSVerify bool) (*GitlabReporter, error) {
	codebaseUrl := os.Getenv("CODEBASE_URL")
	projectId := os.Getenv("PROJECT_ID")
	reportSha := os.Getenv("REPORT_SHA")
	token := os.Getenv("CODEBASE_TOKEN")
	pipelineUrl := os.Getenv("PIPELINE_URL")

	url := fmt.Sprintf("%s/api/v4/projects/%s/statuses/%s", codebaseUrl, projectId, reportSha)
	return &GitlabReporter{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify},
			},
		},
		url:       url,
		token:     token,
		targetUrl: pipelineUrl,
	}, nil
}

// ReportJobFinal is a no-op: GitLab commit statuses are per-step contexts, so
// job-level completion is only reported to the Neutron API.
func (r *GitlabReporter) ReportJobFinal(status model.StepResult, description string) {}

func (r *GitlabReporter) Report(jobName string, stepName string, status model.StepResult, description string) {
	m := message{
		TargetUrl:   r.targetUrl,
		Description: description,
		Context:     fmt.Sprintf("%s/%s", jobName, stepName),
	}
	switch status {
	case model.Pending:
		m.State = "pending"
	case model.Running:
		m.State = "running"
	case model.Fail:
		m.State = "failed"
	case model.Success:
		m.State = "success"
	default:
		m.State = "failed"
	}
	body, err := json.Marshal(m)
	if err != nil {
		log.Printf("Warning: failed to marshal status: %v", err)
		return
	}
	req, err := http.NewRequest("POST", r.url, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Warning: failed to create request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("Warning: failed to report pipeline status to gitlab: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("Warning: gitlab returned %s for %s/%s", resp.Status, jobName, stepName)
	} else {
		log.Printf("Pipeline status reported to gitlab: %s", resp.Status)
	}
}
