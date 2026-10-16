package main

import (
	"context"
	"errors"
	"testing"

	"neutron/internal"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	testingk8s "k8s.io/client-go/testing"
)

func TestIsTerminalJob(t *testing.T) {
	tests := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{name: "nil", want: false},
		{name: "pending", job: &batchv1.Job{}, want: false},
		{name: "complete true", job: jobWithCondition(batchv1.JobComplete, "True"), want: true},
		{name: "failed true", job: jobWithCondition(batchv1.JobFailed, "True"), want: true},
		{name: "complete false", job: jobWithCondition(batchv1.JobComplete, "False"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTerminalJob(tt.job); got != tt.want {
				t.Fatalf("isTerminalJob() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFindBlockingJob(t *testing.T) {
	tests := []struct {
		name       string
		objects    []runtime.Object
		candidates []internal.PipelineJob
		getError   error
		want       string
		wantErr    bool
	}{
		{
			name:       "nonterminal job blocks",
			objects:    []runtime.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "running", Namespace: "default"}}},
			candidates: []internal.PipelineJob{{Name: "running"}},
			want:       "running",
		},
		{
			name:       "terminal job does not block",
			objects:    []runtime.Object{jobWithCondition(batchv1.JobComplete, "True")},
			candidates: []internal.PipelineJob{{Name: "done"}},
		},
		{
			name:       "deleted job does not block",
			candidates: []internal.PipelineJob{{Name: "deleted"}},
		},
		{
			name:       "Kubernetes error fails closed",
			candidates: []internal.PipelineJob{{Name: "unknown"}},
			getError:   errors.New("Kubernetes unavailable"),
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset(tt.objects...)
			if tt.getError != nil {
				client.PrependReactor("get", "jobs", func(t testingk8s.Action) (bool, runtime.Object, error) {
					return true, nil, tt.getError
				})
			}
			got, err := findBlockingJob(context.Background(), client.BatchV1().Jobs("default"), tt.candidates)
			if (err != nil) != tt.wantErr {
				t.Fatalf("findBlockingJob() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("findBlockingJob() = %q, want %q", got, tt.want)
			}
		})
	}
}

func jobWithCondition(condition batchv1.JobConditionType, status corev1.ConditionStatus) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: "default"}, Status: batchv1.JobStatus{
		Conditions: []batchv1.JobCondition{{Type: condition, Status: status}},
	}}
}
