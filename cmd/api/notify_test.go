package main

import (
	"strings"
	"testing"

	"neutron/internal"
)

func TestIsFinalReport(t *testing.T) {
	cases := []struct {
		name   string
		status internal.JobStatus
		want   bool
	}{
		{"running report", internal.JobStatus{Active: 1}, false},
		{"step success without final", internal.JobStatus{Succeeded: 1}, false},
		{"step failure without final", internal.JobStatus{Failed: 1}, false},
		{"final success", internal.JobStatus{Final: true, Succeeded: 1}, true},
		{"final failure", internal.JobStatus{Final: true, Failed: 1}, true},
		{"final but not terminal", internal.JobStatus{Final: true, Active: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isFinalReport(tc.status); got != tc.want {
				t.Errorf("isFinalReport(%+v) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestCompletionMessage(t *testing.T) {
	cases := []struct {
		name       string
		failed     bool
		reason     string
		wantTitle  string
		wantReason bool
	}{
		{"success without description", false, "", "✅ 流水线执行成功", false},
		{"success with description must not print reason", false, "pipeline finished.", "✅ 流水线执行成功", false},
		{"failure with reason", true, "step build failed", "❌ 流水线执行失败", true},
		{"failure without reason", true, "", "❌ 流水线执行失败", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, content := completionMessage("repo", "job", "https://src", "https://host/#/status/job", tc.failed, tc.reason)
			if title != tc.wantTitle {
				t.Errorf("title = %q, want %q", title, tc.wantTitle)
			}
			if got := strings.Contains(content, "📝 原因"); got != tc.wantReason {
				t.Errorf("content contains reason line = %v, want %v; content=%q", got, tc.wantReason, content)
			}
		})
	}
}
