package model

import "testing"

func TestEffectiveJobTtlSeconds(t *testing.T) {
	cases := []struct {
		name    string
		ttl     *int // job-ttl-minutes
		wantNil bool
		want    int32 // seconds handed to the K8s Job
	}{
		{name: "unset falls back to the 8h default", ttl: nil, want: DefaultJobTtlMinutes * 60},
		{name: "configured minutes are converted", ttl: intp(30), want: 30 * 60},
		{name: "zero means default too", ttl: intp(0), want: DefaultJobTtlMinutes * 60},
		{name: "negative disables cleanup", ttl: intp(-1), wantNil: true},
		{name: "oversized value is clamped", ttl: intp(1 << 40), want: 1<<31 - 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := KubernetesConfig{JobTtlMinutes: tc.ttl}.EffectiveJobTtlSeconds()
			if tc.wantNil {
				if got != nil {
					t.Fatalf("ttl = %ds, want nil (cleanup disabled)", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ttl = nil, want %ds", tc.want)
			}
			if *got != tc.want {
				t.Errorf("ttl = %ds, want %ds", *got, tc.want)
			}
		})
	}
}

func intp(i int) *int { return &i }
