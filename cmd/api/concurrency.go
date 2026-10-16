package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"neutron/internal"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchtyped "k8s.io/client-go/kubernetes/typed/batch/v1"
)

const jobLaunchTimeout = 30 * time.Second
const jobCreateConfirmationTimeout = 5 * time.Second

// launchJobWithConcurrency keeps the check and creation inside a per-key MySQL
// advisory lock. The lock is shared by API replicas and is released as soon as
// the K8s Job and its history row have both been created.
func (s *Server) launchJobWithConcurrency(projectID, jobKey string, parallel bool, skippedName string, create func(context.Context) (string, error)) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), jobLaunchTimeout)
	defer cancel()
	release, err := s.repo.AcquireJobLock(ctx, projectID, jobKey)
	if err != nil {
		return "", false, fmt.Errorf("cannot safely check job concurrency: %w", err)
	}
	defer release()

	if !parallel {
		candidates, err := s.repo.ListUncompletedJobKey(ctx, projectID, jobKey)
		if err != nil {
			return "", false, fmt.Errorf("cannot safely check existing job runs: %w", err)
		}
		blocking, err := findBlockingJob(ctx, s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace), candidates)
		if err != nil {
			return "", false, err
		}
		if blocking != "" {
			now := time.Now()
			status, _ := json.Marshal(internal.JobStatus{Skipped: true, Description: fmt.Sprintf("同名任务 %s 仍在运行，本次触发已跳过", blocking)})
			if err := s.repo.AddJobContext(ctx, internal.PipelineJob{
				ProjectId: projectID, Name: skippedName, JobName: jobKey, Status: string(status),
				Completed: true, CompletedAt: &now,
			}); err != nil {
				return "", false, fmt.Errorf("failed to record skipped job: %w", err)
			}
			return skippedName, true, nil
		}
	}
	name, err := create(ctx)
	return name, false, err
}

func findBlockingJob(ctx context.Context, jobs batchtyped.JobInterface, candidates []internal.PipelineJob) (string, error) {
	for _, candidate := range candidates {
		job, err := jobs.Get(ctx, candidate.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("cannot confirm status of existing job %s: %w", candidate.Name, err)
		}
		if isTerminalJob(job) {
			continue
		}
		return candidate.Name, nil
	}
	return "", nil
}

func isTerminalJob(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, condition := range job.Status.Conditions {
		if (condition.Type == batchv1.JobComplete || condition.Type == batchv1.JobFailed) && condition.Status == "True" {
			return true
		}
	}
	return false
}

// recordJobCreateFailure only closes a prewritten history row when Kubernetes
// confirms that the Job does not exist. A timeout or other ambiguous response
// must leave it uncompleted so later triggers fail closed against that row.
func (s *Server) recordJobCreateFailure(name string, createErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), jobCreateConfirmationTimeout)
	defer cancel()
	_, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(ctx, name, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		return
	}
	status := internal.JobStatus{Failed: 1, Final: true, Description: fmt.Sprintf("Kubernetes Job creation failed: %v", createErr)}
	_ = s.repo.UpdateJobStatus(name, status)
	_ = s.repo.MarkJobCompleted(name)
}
