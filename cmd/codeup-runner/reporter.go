package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"neutron/internal/model"
	"neutron/internal/parser"
	"os"
	"time"
)

type codeupStatusMessage struct {
	State       string `json:"state"`
	Context     string `json:"context"`
	Description string `json:"description"`
	TargetUrl   string `json:"targetUrl"`
}

type CodeupReporter struct {
	client    *http.Client
	url       string
	token     string
	targetUrl string
}

func NewCodeupReporterFromEnv(skipTLSVerify bool) (*CodeupReporter, error) {
	codebaseUrl := os.Getenv("CODEBASE_URL")
	repoUrl := os.Getenv("GIT_REPO_URL")
	reportSha := os.Getenv("REPORT_SHA")
	token := os.Getenv("CODEBASE_TOKEN")
	pipelineUrl := os.Getenv("PIPELINE_URL")

	orgId, projectPath := parser.ExtractCodeupOrgAndProject(repoUrl)
	if orgId == "" || projectPath == "" {
		return nil, fmt.Errorf("cannot extract org-id and project path from repo URL: %s", repoUrl)
	}
	encodedProjectPath := parser.EncodeCodeupProjectPath(projectPath)

	url := fmt.Sprintf("%s/oapi/v1/codeup/organizations/%s/repositories/%s/commits/%s/statuses",
		codebaseUrl, orgId, encodedProjectPath, reportSha)

	return &CodeupReporter{
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

// ReportJobFinal is a no-op: Codeup commit statuses are per-step contexts, so
// job-level completion is only reported to the Neutron API.
func (r *CodeupReporter) ReportJobFinal(status model.StepResult, description string) {}

func (r *CodeupReporter) Report(jobName string, stepName string, status model.StepResult, description string) {
	m := codeupStatusMessage{
		TargetUrl:   r.targetUrl,
		Description: description,
		Context:     fmt.Sprintf("%s/%s", jobName, stepName),
	}
	switch status {
	case model.Pending, model.Running:
		m.State = "pending"
	case model.Fail:
		m.State = "failure"
	case model.Success:
		m.State = "success"
	default:
		m.State = "failure"
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
	req.Header.Set("x-yunxiao-token", r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		log.Printf("Warning: failed to report pipeline status to codeup: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		log.Printf("Warning: codeup returned %s for %s/%s", resp.Status, jobName, stepName)
	} else {
		log.Printf("Pipeline status reported to codeup: %s", resp.Status)
	}
}
