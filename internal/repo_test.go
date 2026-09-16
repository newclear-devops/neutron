package internal

import (
	"encoding/json"
	"strings"
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

func TestStatusSearchClause(t *testing.T) {
	cases := []struct {
		word   string
		wantOK bool
		// what the clause must match, expressed as a JSON status payload
		matches []string
		misses  []string
	}{
		{
			word:    "running",
			wantOK:  true,
			matches: []string{mustJSON(t, JobStatus{Active: 1})},
			misses:  []string{"", mustJSON(t, JobStatus{Succeeded: 1}), mustJSON(t, JobStatus{Failed: 1})},
		},
		{
			word:    "failed",
			wantOK:  true,
			matches: []string{mustJSON(t, JobStatus{Failed: 1})},
			misses:  []string{"", mustJSON(t, JobStatus{Active: 1})},
		},
		{
			// Pending used to be a client-side notion: no outcome flag set at
			// all, usually an empty status. It must stay searchable.
			word:    "pending",
			wantOK:  true,
			matches: []string{"", `{}`, mustJSON(t, JobStatus{})},
			misses: []string{
				mustJSON(t, JobStatus{Active: 1}),
				mustJSON(t, JobStatus{Succeeded: 1}),
				mustJSON(t, JobStatus{Failed: 1}),
			},
		},
		{word: "order-service", wantOK: false},
		{word: "", wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.word, func(t *testing.T) {
			clause, ok := statusSearchClause(tc.word)
			if ok != tc.wantOK {
				t.Fatalf("statusSearchClause(%q) ok = %v, want %v", tc.word, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			for _, status := range tc.matches {
				if !clauseMatchesStatus(t, clause, status) {
					t.Errorf("clause for %q should match status %s", tc.word, status)
				}
			}
			for _, status := range tc.misses {
				if clauseMatchesStatus(t, clause, status) {
					t.Errorf("clause for %q should NOT match status %s", tc.word, status)
				}
			}
		})
	}
}

// clauseMatchesStatus evaluates one of the statusSearchClause predicates the way
// MySQL would for a single status value, so the clauses can be checked without a
// database. Supported shape: OR-ed groups of AND-ed atoms, where an atom is a
// LIKE, a negated LIKE, an emptiness check, or an IS NULL check on status.
func clauseMatchesStatus(t *testing.T, clause, status string) bool {
	t.Helper()
	clause = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(clause), "("), ")")
	for _, group := range strings.Split(clause, " OR ") {
		matched := true
		for _, atom := range strings.Split(strings.Trim(group, "()"), " AND ") {
			if !atomMatchesStatus(t, strings.TrimSpace(atom), status) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func atomMatchesStatus(t *testing.T, atom, status string) bool {
	t.Helper()
	switch {
	case atom == "status IS NULL":
		return false // the column is written as '' rather than NULL
	case atom == "status = ''":
		return status == ""
	case strings.HasPrefix(atom, "status NOT LIKE '%"):
		needle := strings.TrimSuffix(strings.TrimPrefix(atom, "status NOT LIKE '%"), "%'")
		return !strings.Contains(status, needle)
	case strings.HasPrefix(atom, "status LIKE '%"):
		needle := strings.TrimSuffix(strings.TrimPrefix(atom, "status LIKE '%"), "%'")
		return strings.Contains(status, needle)
	default:
		t.Fatalf("unsupported clause atom %q — update the test helper", atom)
		return false
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
