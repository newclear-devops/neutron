package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"neutron/internal"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// launchJobWithConcurrency keeps the check and creation inside a per-key MySQL
// advisory lock. The lock is shared by API replicas and is released as soon as
// the K8s Job and its history row have both been created.
func (s *Server) launchJobWithConcurrency(projectID, jobKey string, parallel bool, skippedName string, create func() (string, error)) (string, bool, error) {
	release, err := s.repo.AcquireJobLock(context.Background(), projectID, jobKey)
	if err != nil {
		return "", false, fmt.Errorf("cannot safely check job concurrency: %w", err)
	}
	defer release()

	if parallel {
		name, err := create()
		return name, false, err
	}
	candidates, err := s.repo.ListUncompletedJobKey(projectID, jobKey, defaultJobWindowDays)
	if err != nil {
		return "", false, fmt.Errorf("cannot safely check existing job runs: %w", err)
	}
	jobClient := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace)
	for _, candidate := range candidates {
		job, err := jobClient.Get(context.Background(), candidate.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("cannot confirm status of existing job %s: %w", candidate.Name, err)
		}
		terminal := false
		for _, condition := range job.Status.Conditions {
			if (condition.Type == "Complete" || condition.Type == "Failed") && condition.Status == "True" {
				terminal = true
				break
			}
		}
		if terminal {
			continue
		}
		now := time.Now()
		status, _ := json.Marshal(internal.JobStatus{Skipped: true, Description: fmt.Sprintf("Skipped because job %s is still running", candidate.Name)})
		if err := s.repo.AddJob(internal.PipelineJob{
			ProjectId:   projectID,
			Name:        skippedName,
			JobName:     jobKey,
			Status:      string(status),
			Completed:   true,
			CompletedAt: &now,
		}); err != nil {
			return "", false, fmt.Errorf("failed to record skipped job: %w", err)
		}
		return skippedName, true, nil
	}
	name, err := create()
	return name, false, err
}

// recordJobCreateFailure only closes a prewritten history row when Kubernetes
// confirms that the Job does not exist. A timeout or other ambiguous response
// must leave it uncompleted so later triggers fail closed against that row.
func (s *Server) recordJobCreateFailure(name string, createErr error) {
	_, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return
	}
	status := internal.JobStatus{Failed: 1, Final: true, Description: fmt.Sprintf("Kubernetes Job creation failed: %v", createErr)}
	_ = s.repo.UpdateJobStatus(name, status)
	_ = s.repo.MarkJobCompleted(name)
}
