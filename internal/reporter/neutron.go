package reporter

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"neutron/internal/model"
	"time"
)

type Neutron struct {
	apiUrl      string
	jobName     string
	triggerType string
	webhookType string
	repoUrl     string
	client      *http.Client
}

func NewNeutron(apiUrl string, jobName string, triggerType string, webhookType string, repoUrl string, skipTLSVerify bool) *Neutron {
	return &Neutron{
		apiUrl:      apiUrl,
		jobName:     jobName,
		triggerType: triggerType,
		webhookType: webhookType,
		repoUrl:     repoUrl,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: skipTLSVerify},
			},
		},
	}
}

// RegisterPod reports this runner's pod info to the Neutron API server at startup.
// This ensures pod info is persisted before the pod gets cleaned up.
func (r *Neutron) RegisterPod(podName string, namespace string) {
	payload := map[string]string{
		"pod_name":  podName,
		"namespace": namespace,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal pod info: %v", err)
		return
	}

	url := fmt.Sprintf("%s/api/report/%s/pod", r.apiUrl, r.jobName)
	resp, err := r.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Failed to register pod with Neutron API: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Neutron API returned status %d for pod registration", resp.StatusCode)
	} else {
		log.Printf("Pod %s registered with Neutron API", podName)
	}
}

func (r *Neutron) Report(jobName string, stepName string, status model.StepResult, description string) {
	payload := map[string]interface{}{
		"webhook_type": r.webhookType,
		"trigger_type": r.triggerType,
		"repo_url":     r.repoUrl,
		"active":       0,
		"succeeded":    0,
		"failed":       0,
	}

	switch status {
	case model.Running, model.Pending:
		payload["active"] = 1
	case model.Success:
		payload["succeeded"] = 1
	case model.Fail:
		payload["failed"] = 1
	}

	r.postStatus(payload)
}

// ReportJobFinal reports the job-level terminal status exactly once, after the
// last step. The API server gates completion notifications and DB completion on
// the final flag, so per-step reports are not mistaken for job completion.
func (r *Neutron) ReportJobFinal(status model.StepResult, description string) {
	payload := map[string]interface{}{
		"webhook_type": r.webhookType,
		"trigger_type": r.triggerType,
		"repo_url":     r.repoUrl,
		"active":       0,
		"succeeded":    0,
		"failed":       0,
		"final":        true,
		"description":  description,
	}

	switch status {
	case model.Success:
		payload["succeeded"] = 1
	default:
		payload["failed"] = 1
	}

	r.postStatus(payload)
}

func (r *Neutron) postStatus(payload map[string]interface{}) {
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal status: %v", err)
		return
	}

	url := fmt.Sprintf("%s/api/report/%s", r.apiUrl, r.jobName)
	resp, err := r.client.Post(url, "application/json", bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Failed to report to Neutron API: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Neutron API returned status %d", resp.StatusCode)
	}
}
