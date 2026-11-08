package main

import (
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestJobTerminalTime(t *testing.T) {
	now := time.Now().Truncate(time.Second)

	cases := []struct {
		name      string
		status    batchv1.JobStatus
		wantOK    bool
		wantWhen  time.Time
	}{
		{
			name:   "running job is not terminal",
			status: batchv1.JobStatus{Active: 1},
			wantOK: false,
		},
		{
			name:     "completion time wins",
			status:   batchv1.JobStatus{CompletionTime: &metav1.Time{Time: now}},
			wantOK:   true,
			wantWhen: now,
		},
		{
			name: "failed condition last transition",
			status: batchv1.JobStatus{
				Failed: 1,
				Conditions: []batchv1.JobCondition{{
					Type:               batchv1.JobFailed,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: now},
				}},
			},
			wantOK:   true,
			wantWhen: now,
		},
		{
			name: "failed condition without timestamp is not usable",
			status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:   batchv1.JobFailed,
					Status: v1.ConditionTrue,
				}},
			},
			wantOK: false,
		},
		{
			name: "non-terminal condition is ignored",
			status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{{
					Type:               batchv1.JobSuspended,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: now},
				}},
			},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			when, ok := jobTerminalTime(&tc.status)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !when.Equal(tc.wantWhen) {
				t.Errorf("when = %v, want %v", when, tc.wantWhen)
			}
		})
	}
}

func TestResolveHealOutcome(t *testing.T) {
	terminalSuccess := `{"succeeded":1,"final":true}`
	terminalFail := `{"failed":1,"final":true}`
	active := `{"active":1}`
	empty := ""

	cases := []struct {
		name     string
		k8sJob   *batchv1.Job
		status   string
		wantFail bool
		wantOK   bool
	}{
		{
			name:   "k8s job succeeded wins",
			k8sJob: &batchv1.Job{Status: batchv1.JobStatus{Succeeded: 1}},
			status: active,
			wantOK: true,
		},
		{
			name:     "k8s job failed wins",
			k8sJob:   &batchv1.Job{Status: batchv1.JobStatus{Failed: 1}},
			status:   terminalSuccess,
			wantFail: true,
			wantOK:   true,
		},
		{
			name:   "k8s job gone, db status succeeded",
			status: terminalSuccess,
			wantOK: true,
		},
		{
			name:     "k8s job gone, db status failed",
			status:   terminalFail,
			wantFail: true,
			wantOK:   true,
		},
		{
			name:   "k8s job gone, db status active only",
			status: active,
			wantOK: false,
		},
		{
			name:   "k8s job gone, empty db status",
			status: empty,
			wantOK: false,
		},
		{
			name:   "k8s job gone, corrupted db status",
			status: `{not-json`,
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failed, ok := resolveHealOutcome(tc.k8sJob, tc.status)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && failed != tc.wantFail {
				t.Errorf("failed = %v, want %v", failed, tc.wantFail)
			}
		})
	}
}

func TestTerminalPodPhase(t *testing.T) {
	if got := terminalPodPhase(false); got != "Succeeded" {
		t.Errorf("terminalPodPhase(false) = %q, want Succeeded", got)
	}
	if got := terminalPodPhase(true); got != "Failed" {
		t.Errorf("terminalPodPhase(true) = %q, want Failed", got)
	}
}

func TestK8sFailureReason(t *testing.T) {
	cases := []struct {
		name   string
		job    *batchv1.Job
		want   string
	}{
		{
			name: "no conditions",
			job:  &batchv1.Job{},
			want: "",
		},
		{
			name: "failed with reason and message",
			job: &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  v1.ConditionTrue,
				Reason:  "BackoffLimitExceeded",
				Message: "Job has reached the specified backoff limit",
			}}}},
			want: "BackoffLimitExceeded: Job has reached the specified backoff limit",
		},
		{
			name: "failed with reason only",
			job: &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobFailed,
				Status: v1.ConditionTrue,
				Reason: "DeadlineExceeded",
			}}}},
			want: "DeadlineExceeded",
		},
		{
			name: "non-failure condition ignored",
			job: &batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
				Type:   batchv1.JobComplete,
				Status: v1.ConditionTrue,
				Reason: "Complete",
			}}}},
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := k8sFailureReason(tc.job); got != tc.want {
				t.Errorf("k8sFailureReason() = %q, want %q", got, tc.want)
			}
		})
	}
}
