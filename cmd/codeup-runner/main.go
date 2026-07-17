package main

import (
	"log"
	"neutron/internal/model"
	"neutron/internal/reporter"
	"neutron/internal/service"
	"os"
	"strings"
)

func main() {
	apiUrl := os.Getenv("NEUTRON_API_URL")
	fullJobName := os.Getenv("FULL_JOB_NAME")
	jobName := os.Getenv("JOB_NAME")
	triggerType := os.Getenv("TRIGGER")
	webhookType := os.Getenv("RUNNER_PLATFORM")
	repoUrl := os.Getenv("GIT_REPO_URL")

	skipTLS := strings.EqualFold(os.Getenv("SKIP_TLS_VERIFY"), "true")
	skipTriggerCheck := strings.EqualFold(os.Getenv("SKIP_TRIGGER_CHECK"), "true")
	skipPlatformReport := strings.EqualFold(os.Getenv("SKIP_PLATFORM_REPORT"), "true")

	// Neutron reporter (always used)
	neutronReporter := reporter.NewNeutron(apiUrl, fullJobName, triggerType, webhookType, repoUrl, skipTLS)
	neutronReporter.RegisterPod(os.Getenv("POD_NAME"), os.Getenv("POD_NAMESPACE"))

	var composite model.Reporter
	if skipPlatformReport {
		// Only report to Neutron, skip Codeup commit statuses
		composite = neutronReporter
	} else {
		// Composite: Codeup + Neutron
		codeupReporter, err := NewCodeupReporterFromEnv(skipTLS)
		if err != nil {
			log.Fatalf("failed to create codeup reporter: %v", err)
		}
		composite = reporter.NewComposite(codeupReporter, neutronReporter)
	}

	runner := service.NewRunner("/repo", triggerType, jobName, composite, apiUrl, skipTriggerCheck)
	runner.Run()
}
