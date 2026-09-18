package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"neutron/internal"
)

// Manual kill for a run that is stuck: still active in Kubernetes, neither
// finishing nor failing, so neither the TTL cleanup (#32) nor the reconciler
// ever touches it. TTL only applies to Jobs that have *finished*, and the
// reconciler skips Jobs that are not terminal — a hung Job therefore keeps its
// Pods forever, and keeps counting as a running sibling.
//
// No automatic timeout is offered instead: pipeline durations vary too much for
// any global deadline to be safe (it would eventually kill a legitimately slow
// build), so termination stays an explicit, confirmed user action.

// handleKill force-terminates a still-running job.
// POST /api/jobs/:jobName/kill
func (s *Server) handleKill(c *gin.Context) {
	jobName := c.Param("jobName")
	dbJob, err := s.repo.GetJobByName(jobName)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	// Already terminal: there is nothing to kill, and re-recording the outcome
	// would send a second completion notification. This is also what makes a
	// repeated click safe.
	if dbJob.Completed {
		c.JSON(http.StatusConflict, gin.H{"error": "job has already finished; nothing to kill"})
		return
	}

	status, err := s.killRunningJob(dbJob)
	if err != nil {
		// Nothing has been written yet, so the caller can simply retry.
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to delete the Kubernetes Job: %v", err)})
		return
	}

	if err := s.repo.UpdateJobStatus(jobName, status); err != nil {
		// The Job is already gone: the row now has no Kubernetes source of truth
		// and will surface as "stuck" (outcome unknown) on the status page. That
		// is the same degradation a TTL-cleaned never-reported job has, and it is
		// reported honestly rather than papered over.
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// The Pods went with the Job, so their cached phase would otherwise stay
	// "Running" on the status page, which serves finished jobs from the DB.
	for _, pod := range dbJob.Pods {
		_ = s.repo.UpdatePodStatus(pod.PodUid, "Failed")
	}
	_ = s.repo.MarkJobCompleted(jobName)
	s.notifyJobCompleted(dbJob, true, status.Description)

	c.JSON(http.StatusOK, gin.H{"ok": true, "job_name": jobName})
}

// killRunningJob deletes the run's Kubernetes Job and its Pods, and returns the
// terminal status to record. It is kept out of the handler so the Kubernetes
// half is testable without a database.
//
// The propagation policy is set explicitly rather than left unset. A delete of a
// batch/v1 Job carrying no policy defaults to OrphanDependents — Kubernetes
// keeps that for backwards compatibility — which removes the Job but leaves its
// Pods running. That is the opposite of what a kill is for: a hung step would
// keep its resources, and with the Job gone nothing else would ever clean them
// up. kubectl only looks like it cascades because it always sends a policy of
// its own. Background fits this endpoint: the Job goes away at once and the
// garbage collector removes the Pods, without the request waiting for them.
//
// A Job that is already gone is not an error: the outcome still gets recorded,
// which is the point of the endpoint — it also closes out a row whose Job was
// TTL-cleaned while it never reported.
func (s *Server) killRunningJob(dbJob *internal.PipelineJob) (internal.JobStatus, error) {
	// The policy is a pointer, where nil means "unset" and would fall back to
	// the default described above, so it needs a variable of its own.
	propagation := metav1.DeletePropagationBackground
	err := s.clientSet.BatchV1().Jobs(s.config.Kubernetes.Namespace).
		Delete(context.Background(), dbJob.Name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	switch {
	case err == nil:
		return killStatus(dbJob, true), nil
	case apierrors.IsNotFound(err):
		return killStatus(dbJob, false), nil
	default:
		return internal.JobStatus{}, err
	}
}

// killStatus builds the terminal status recorded for a force-killed run. The
// descriptive fields are carried over from the previous status: overwriting them
// with a zero value would blank the project/source links and the trigger type on
// the status page. `deleted` says whether the Kubernetes Job was actually found.
func killStatus(dbJob *internal.PipelineJob, deleted bool) internal.JobStatus {
	var status internal.JobStatus
	_ = json.Unmarshal([]byte(dbJob.Status), &status)
	status.Active = 0
	status.Succeeded = 0
	status.Failed = 1
	status.Final = true
	status.Description = killDescription(deleted)
	return status
}

func killDescription(deleted bool) string {
	if deleted {
		return "被手动强制终止（Kill）"
	}
	return "被手动强制终止（Kill）；Kubernetes Job 已不存在"
}
