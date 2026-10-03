package tokenutil

import "testing"

func TestApplyTotalFallback(t *testing.T) {
	tests := []struct {
		name          string
		input         int64
		output        int64
		cacheCreation int64
		cacheRead     int64
		extra         int64
		total         int64
		wantOutput    int64
		wantExtra     int64
	}{
		{
			// opencode-style: components already sum exactly to total, so
			// nothing is adjusted.
			name:          "components already equal total",
			input:         11426,
			output:        33,
			cacheCreation: 0,
			cacheRead:     1840,
			extra:         0,
			total:         13299,
			wantOutput:    33,
			wantExtra:     0,
		},
		{
			// gap fill: output is 0 and there is a 50-token remainder, which
			// fills the empty output slot.
			name:          "gap fills empty output",
			input:         90,
			output:        0,
			cacheCreation: 10,
			cacheRead:     20,
			extra:         5,
			total:         175,
			wantOutput:    50,
			wantExtra:     5,
		},
		{
			// overflow: output is non-zero and there is a remainder, which
			// flows into the extra bucket.
			name:          "remainder overflows into extra",
			input:         100,
			output:        40,
			cacheCreation: 0,
			cacheRead:     0,
			extra:         0,
			total:         200,
			wantOutput:    40,
			wantExtra:     60,
		},
		{
			// overflow with a pre-existing extra: remainder is added to it.
			name:          "remainder adds to existing extra",
			input:         100,
			output:        40,
			cacheCreation: 0,
			cacheRead:     10,
			extra:         5,
			total:         200,
			wantOutput:    40,
			wantExtra:     50, // 5 + (200 - 155)
		},
		{
			// total smaller than known: never reduce a known component.
			name:          "total below known leaves inputs unchanged",
			input:         100,
			output:        40,
			cacheCreation: 0,
			cacheRead:     0,
			extra:         0,
			total:         50,
			wantOutput:    40,
			wantExtra:     0,
		},
		{
			// no total reported (0): treated as nothing to reconcile.
			name:          "zero total is a no-op",
			input:         100,
			output:        40,
			cacheCreation: 0,
			cacheRead:     0,
			extra:         0,
			total:         0,
			wantOutput:    40,
			wantExtra:     0,
		},
		{
			// all zeros: stays all zeros.
			name:          "all zeros",
			input:         0,
			output:        0,
			cacheCreation: 0,
			cacheRead:     0,
			extra:         0,
			total:         0,
			wantOutput:    0,
			wantExtra:     0,
		},
		{
			// only a total reported, no components: the whole total fills the
			// empty output.
			name:          "total only fills output",
			input:         0,
			output:        0,
			cacheCreation: 0,
			cacheRead:     0,
			extra:         0,
			total:         1234,
			wantOutput:    1234,
			wantExtra:     0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOutput, gotExtra := ApplyTotalFallback(
				tc.input, tc.output, tc.cacheCreation, tc.cacheRead, tc.extra, tc.total,
			)
			if gotOutput != tc.wantOutput {
				t.Errorf("output = %d, want %d", gotOutput, tc.wantOutput)
			}
			if gotExtra != tc.wantExtra {
				t.Errorf("extra = %d, want %d", gotExtra, tc.wantExtra)
			}
		})
	}
}

// TestApplyTotalFallbackPreservesComponentSum asserts the core invariant: after
// applying the fallback, the components (with the adjusted output/extra) sum to
// at least the reported total whenever the total exceeded the original known
// sum, and are otherwise left untouched.
func TestApplyTotalFallbackPreservesComponentSum(t *testing.T) {
	const (
		input         = 90
		origOutput    = 0
		cacheCreation = 10
		cacheRead     = 20
		origExtra     = 5
		total         = 175
	)
	newOutput, newExtra := ApplyTotalFallback(input, origOutput, cacheCreation, cacheRead, origExtra, total)
	sum := input + newOutput + cacheCreation + cacheRead + newExtra
	if sum != total {
		t.Fatalf("reconciled component sum = %d, want %d", sum, total)
	}
}
