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
	log.Printf("runner: fetched default pipeline from %s/api/default-pipeline (%d bytes)", apiUrl, len(body.Content))
	return []byte(body.Content), nil
}

func NewRunner(workingDir string, triggerType string, jobName string, reporter model.Reporter, apiUrl string, skipTriggerCheck ...bool) *Runner {
	data, err := os.ReadFile(path.Join(workingDir, "neutron.yaml"))
	if err != nil {
		// Repo has no neutron.yaml — fall back to the globally-configured
		// default pipeline fetched from the Neutron API.
		log.Printf("neutron.yaml not readable (%v), falling back to default pipeline from %s", err, apiUrl)
		fallback, ferr := fetchDefaultPipeline(apiUrl)
		if ferr != nil {
			log.Fatalf("neutron.yaml not found and default pipeline unavailable: %v (read error: %v)", ferr, err)
		}
		if len(fallback) == 0 {
			log.Fatalf("neutron.yaml not found and no default pipeline configured")
		}
		log.Printf("using default pipeline for job %s", jobName)
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
			// No steps will run for this trigger: report a final "skipped"
			// success and stop before Run() executes. NewRunner has no
			// skip-signal in its contract, so we exit here (code 0) rather
			// than running zero steps through Run().
			reporter.ReportJobFinal(model.Success, "job skipped: trigger mismatch")
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

// Run executes the steps sequentially and reports each step's status. It
// returns the process exit code: 0 when every step succeeded, 1 otherwise.
// A job-level terminal report (ReportJobFinal) is always sent before
// returning, so the API server knows exactly when the job finished.
func (r *Runner) Run() int {
	// create all step status
	for _, step := range r.Steps {
		r.Reporter.Report(r.JobName, step.StepName, model.Pending, "pipeline created.")
	}

	// run in seq
	for runStepIndex, step := range r.Steps {
		if step.Command == "" {
			r.Reporter.Report(r.JobName, step.StepName, model.Fail, "empty command.")
			r.failRemaining(runStepIndex)
			r.Reporter.ReportJobFinal(model.Fail, fmt.Sprintf("step %s: empty command.", step.StepName))
			return 1
		}
		r.Reporter.Report(r.JobName, step.StepName, model.Running, "pipeline started.")
		cmd := exec.Command("sh", "-c", step.Command)
		cmd.Dir = r.WorkingDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			errMsg := fmt.Sprintf("step %s failed: %v", step.StepName, err)
			r.Reporter.Report(r.JobName, step.StepName, model.Fail, errMsg)
			r.failRemaining(runStepIndex + 1)
			r.Reporter.ReportJobFinal(model.Fail, errMsg)
			return 1
		}
		r.Reporter.Report(r.JobName, step.StepName, model.Success, "pipeline finished.")
	}
	r.Reporter.ReportJobFinal(model.Success, "pipeline finished.")
	return 0
}

func (r *Runner) failRemaining(fromIndex int) {
	for i := fromIndex; i < len(r.Steps); i++ {
		r.Reporter.Report(r.JobName, r.Steps[i].StepName, model.Fail, "pipeline failed.")
	}
}
