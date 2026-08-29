package internal

import (
	"encoding/json"
	"testing"
)

func TestIsStuckJobStatus(t *testing.T) {
	terminalSuccess := mustJSON(t, JobStatus{Succeeded: 1})
	terminalFail := mustJSON(t, JobStatus{Failed: 1})
	active := mustJSON(t, JobStatus{Active: 1})

	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty status is stuck", "", true},
		{"active-only status is stuck", active, true},
		{"zero-value status is stuck", `{}`, true},
		{"succeeded status is terminal", terminalSuccess, false},
		{"failed status is terminal", terminalFail, false},
		{"corrupted non-empty json is not stuck", `{not-json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStuckJobStatus(tc.input); got != tc.want {
				t.Errorf("isStuckJobStatus(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
