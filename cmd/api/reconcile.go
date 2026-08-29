package main

import (
	"context"
	"fmt"
	"log"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"neutron/internal"
)

const (
	// reconcileGracePeriod is how long the reconciler waits after a K8s Job
	// reaches a terminal state before assuming the runner's final report will
	// never arrive and closing the job out itself.
	reconcileGracePeriod = 2 * time.Minute
	// reconcileStaleAfter is the terminal age past which a never-reported job
	// is closed out silently instead of sending a late completion notification.
	reconcileStaleAfter = time.Hour
)

// startReconciler launches a background loop that closes out jobs whose K8s
// Job reached a terminal state without the runner delivering its final report
// (checkout conflict, image pull failure, OOM kill, crash). Such jobs would
// otherwise never send a completion notification nor be marked completed.
func (s *Server) startReconciler(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcileOnce()
			}
		}
	}()
}

// reconcileOnce closes out uncompleted jobs whose K8s Job is terminal but that
// never delivered a final runner report. It is deliberately single-instance:
// with multiple API replicas each running a reconciler, a job could be closed
// out (and notified) more than once. The grace window after the K8s terminal
// time is what keeps a normally-finishing job — whose final report arrives
// within that window — from being double-notified.
func (s *Server) reconcileOnce() {
	jobs, err := s.repo.ListUncompletedJobs(7)
	if err != nil {
		log.Printf("reconciler: failed to list uncompleted jobs: %v", err)
		return
	}
	for _, j := range jobs {
		k8sJob, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), j.Name, metav1.GetOptions{})
		if err != nil {
			// Deleted or not visible in K8s — leave the row alone; the
			// running-siblings endpoint already filters these out.
			continue
		}
		terminalAt, ok := jobTerminalTime(&k8sJob.Status)
		if !ok {
			continue // still running
		}
		age := time.Since(terminalAt)
		if age < reconcileGracePeriod {
			continue // give the runner time to deliver its final report
		}
		if age > reconcileStaleAfter {
			// Stale zombie (e.g. from before this feature existed): close it
			// out silently instead of sending a flood of late notifications.
			log.Printf("reconciler: closing stale job %s (terminal %s ago, no notification)", j.Name, age.Truncate(time.Second))
			_ = s.repo.MarkJobCompleted(j.Name)
			continue
		}
		failed := k8sJob.Status.Succeeded == 0
		reason := k8sFailureReason(k8sJob)
		if failed && reason == "" {
			reason = "runner 未上报最终状态（容器异常退出或被杀死）"
		}
		s.updateTerminalStatus(j.Name, k8sJob, failed)
		s.notifyJobCompleted(&j, failed, reason)
		_ = s.repo.MarkJobCompleted(j.Name)
		outcome := "succeeded"
		if failed {
			outcome = "failed"
		}
		log.Printf("reconciler: job %s %s without a final runner report (terminal %s ago), sent completion notification", j.Name, outcome, age.Truncate(time.Second))
	}
	s.healStuckCompletedJobs()
}

// healStuckCompletedJobs corrects jobs that are marked completed in the DB but
// whose status is still non-terminal (stuck as "running"). This happens when a
// status poll races the runner's final report and overwrites the terminal
// outcome with active:1 before MarkJobCompleted runs. The K8s Job is the
// source of truth for the outcome; the terminal status is written back so the
// status page stops showing these jobs as running.
func (s *Server) healStuckCompletedJobs() {
	jobs, err := s.repo.ListStuckCompletedJobs(7)
	if err != nil {
		log.Printf("reconciler: failed to list stuck completed jobs: %v", err)
		return
	}
	for _, j := range jobs {
		k8sJob, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), j.Name, metav1.GetOptions{})
		if err != nil {
			// Deleted or not visible in K8s — nothing to derive from; leave alone.
			continue
		}
		if _, ok := jobTerminalTime(&k8sJob.Status); !ok {
			continue // K8s job still running; not yet a candidate
		}
		failed := k8sJob.Status.Succeeded == 0
		s.updateTerminalStatus(j.Name, k8sJob, failed)
		outcome := "succeeded"
		if failed {
			outcome = "failed"
		}
		log.Printf("reconciler: healed stuck completed job %s -> %s", j.Name, outcome)
	}
}

// updateTerminalStatus writes a terminal JobStatus derived from K8s Job
// annotations so the status page (which serves DB data for completed jobs)
// shows an outcome even though the runner never reported one.
func (s *Server) updateTerminalStatus(jobName string, k8sJob *batchv1.Job, failed bool) {
	ann := k8sJob.Annotations
	status := internal.JobStatus{
		WebhookType: ann["sourceType"],
		TriggerType: ann["triggerType"],
		RepoUrl:     ann["gitPath"],
		ProjectUrl:  ann["sourceLink"],
		SourceUrl:   ann["sourceUrl"],
		Description: k8sFailureReason(k8sJob),
	}
	if failed {
		status.Failed = 1
	} else {
		status.Succeeded = 1
	}
	_ = s.repo.UpdateJobStatus(jobName, status)
}

// jobTerminalTime returns when the K8s Job reached a terminal state, and
// whether it is terminal at all.
func jobTerminalTime(st *batchv1.JobStatus) (time.Time, bool) {
	if st.CompletionTime != nil {
		return st.CompletionTime.Time, true
	}
	for _, c := range st.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == v1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			return c.LastTransitionTime.Time, true
		}
	}
	return time.Time{}, false
}

// k8sFailureReason returns the JobFailed condition reason/message, if present.
func k8sFailureReason(k8sJob *batchv1.Job) string {
	for _, c := range k8sJob.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == v1.ConditionTrue {
			if c.Message != "" {
				return fmt.Sprintf("%s: %s", c.Reason, c.Message)
			}
			return c.Reason
		}
	}
	return ""
}
