package main

import (
	"context"
	"encoding/json"
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
				// Heal runs on the same tick but as a separate step so a failure
				// to list uncompleted jobs (which returns early from
				// reconcileOnce) does not also block the heal path.
				s.healStuckCompletedJobs()
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
}

// healStuckCompletedJobs corrects jobs that are marked completed in the DB but
// whose status is still non-terminal (stuck as "running"). This happens when a
// status poll races the runner's final report and overwrites the terminal
// outcome with active:1 before MarkJobCompleted runs. The K8s Job is the
// source of truth for the outcome; the terminal status is written back so the
// status page stops showing these jobs as running.
//
// It runs on its own schedule (once per reconcile tick) rather than as a
// side-effect of the uncompleted-jobs loop above, so a failure to list
// uncompleted jobs does not also block the heal path.
func (s *Server) healStuckCompletedJobs() {
	jobs, err := s.repo.ListStuckCompletedJobs(7)
	if err != nil {
		log.Printf("reconciler: failed to list stuck completed jobs: %v", err)
		return
	}
	for _, j := range jobs {
		// Derive the terminal outcome from the K8s Job when possible. When the
		// Job is already gone (TTL-cleaned) the DB status is the only source of
		// truth left — and if it is terminal, the pod phases can still be
		// written back from it.
		k8sJob, err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).Get(context.Background(), j.Name, metav1.GetOptions{})
		if err != nil {
			// Deleted or not visible in K8s — fall through to the DB-status
			// branch below so pod phases can still converge when the status is
			// already terminal.
			k8sJob = nil
		} else if _, ok := jobTerminalTime(&k8sJob.Status); !ok {
			// No CompletionTime and no JobFailed condition (e.g. a job deleted
			// mid-run, or an unusual terminal state): no outcome to derive.
			k8sJob = nil
		}

		// Derive the terminal outcome. The K8s Job is authoritative when it is
		// still around and terminal; when it is gone (TTL-cleaned) or not
		// terminal, the DB status is the last record — if it already carries a
		// terminal outcome (the runner reported it before the Job was cleaned
		// up), pod phases can still converge from it. If neither source has an
		// outcome, the row stays stuck (re-fetched every tick, bounded by the
		// 7-day recency window).
		failed, ok := resolveHealOutcome(k8sJob, j.Status)
		if !ok {
			continue
		}
		if k8sJob != nil {
			s.updateTerminalStatus(j.Name, k8sJob, failed)
		}

		// Write back terminal pod phases. The runner's final-report goroutine
		// polls the K8s pod for at most ~10s and can give up before the pod
		// reaches a terminal phase; once the job is marked completed the status
		// page serves DB data only, so the phase would otherwise stay "Running"
		// forever even though the job succeeded.
		terminalPhase := terminalPodPhase(failed)
		for _, pod := range j.Pods {
			if pod.Phase != "Succeeded" && pod.Phase != "Failed" {
				_ = s.repo.UpdatePodStatus(pod.PodUid, terminalPhase)
			}
		}

		outcome := "succeeded"
		if failed {
			outcome = "failed"
		}
		log.Printf("reconciler: healed stuck completed job %s -> %s", j.Name, outcome)
	}
}

// resolveHealOutcome decides the terminal outcome for a stuck completed job.
// k8sJob may be nil (deleted or not terminal); in that case the persisted
// status JSON is consulted. Returns ok=false when no terminal outcome can be
// derived from either source.
func resolveHealOutcome(k8sJob *batchv1.Job, statusJSON string) (failed bool, ok bool) {
	if k8sJob != nil {
		return k8sJob.Status.Succeeded == 0, true
	}
	var status internal.JobStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return false, false
	}
	if status.Succeeded == 0 && status.Failed == 0 {
		return false, false
	}
	return status.Failed > 0, true
}

// terminalPodPhase returns the K8s pod phase a terminal job outcome maps to.
func terminalPodPhase(failed bool) string {
	if failed {
		return "Failed"
	}
	return "Succeeded"
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
