package service

import (
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v3"
	"log"
	"net/http"
	"neutron/internal/model"
	"os"
	"os/exec"
	"path"
	"time"
)

type Runner struct {
	WorkingDir string
	JobName    string
	Trigger    string
	Steps      []model.Step
	Reporter   model.Reporter
}

// fetchDefaultPipeline retrieves the globally-configured default neutron.yaml
// from the Neutron API. Used as a fallback when the repository has no
// neutron.yaml. Returns empty content (not an error) when none is configured.
func fetchDefaultPipeline(apiUrl string) ([]byte, error) {
	if apiUrl == "" {
		return nil, fmt.Errorf("NEUTRON_API_URL not set")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(apiUrl + "/api/default-pipeline")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("default-pipeline API returned status %d", resp.StatusCode)
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return []byte(body.Content), nil
}

func NewRunner(workingDir string, triggerType string, jobName string, reporter model.Reporter, apiUrl string, skipTriggerCheck ...bool) *Runner {
	data, err := os.ReadFile(path.Join(workingDir, "neutron.yaml"))
	if err != nil {
		// Repo has no neutron.yaml — fall back to the globally-configured
		// default pipeline fetched from the Neutron API.
		fallback, ferr := fetchDefaultPipeline(apiUrl)
		if ferr != nil {
			log.Fatalf("neutron.yaml not found and default pipeline unavailable: %v (read error: %v)", ferr, err)
		}
		if len(fallback) == 0 {
			log.Fatalf("neutron.yaml not found and no default pipeline configured")
		}
		data = fallback
	}
	var pipeline model.Pipeline
	err = yaml.Unmarshal(data, &pipeline)
	if err != nil {
		log.Fatal(err)
	}
	if _, ok := pipeline.Jobs[jobName]; !ok {
		log.Fatalf("pipeline job %s not found", jobName)
	}
	// Skip trigger check if requested (e.g. API-triggered jobs)
	skip := len(skipTriggerCheck) > 0 && skipTriggerCheck[0]
	if !skip {
		matched := false
		for _, t := range pipeline.Jobs[jobName].Trigger {
			if t == triggerType {
				matched = true
				break
			}
		}
		if !matched {
			reporter.Report(jobName, "", model.Success, fmt.Sprintf("Current job skipped in %s.", triggerType))
			os.Exit(0)
		}
	}
	return &Runner{
		WorkingDir: workingDir,
		Trigger:    triggerType,
		JobName:    jobName,
		Steps:      pipeline.Jobs[jobName].Steps,
		Reporter:   reporter,
	}
}

func (r *Runner) Run() {
	// create all step status
	for _, step := range r.Steps {
		r.Reporter.Report(r.JobName, step.StepName, model.Pending, "pipeline created.")
	}

	// run in seq
	for runStepIndex, step := range r.Steps {
		if step.Command == "" {
			r.Reporter.Report(r.JobName, step.StepName, model.Fail, "empty command.")
			r.failRemaining(runStepIndex)
			os.Exit(1)
		}
		r.Reporter.Report(r.JobName, step.StepName, model.Running, "pipeline started.")
		cmd := exec.Command("sh", "-c", step.Command)
		cmd.Dir = r.WorkingDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			errMsg := fmt.Sprintf("step failed: %v", err)
			r.Reporter.Report(r.JobName, step.StepName, model.Fail, errMsg)
			r.failRemaining(runStepIndex + 1)
			os.Exit(1)
		}
		r.Reporter.Report(r.JobName, step.StepName, model.Success, "pipeline finished.")
	}
}

func (r *Runner) failRemaining(fromIndex int) {
	for i := fromIndex; i < len(r.Steps); i++ {
		r.Reporter.Report(r.JobName, r.Steps[i].StepName, model.Fail, "pipeline failed.")
	}
}
