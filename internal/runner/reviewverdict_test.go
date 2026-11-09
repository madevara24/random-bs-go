package runner

import "testing"

// TestParseReviewVerdict covers RBG-52: a reviewer whose VERDICT: line
// isn't the first line of its answer must still parse correctly, while a
// missing/malformed verdict still reads as CONCERNS and a first-line
// verdict still parses exactly as before.
func TestParseReviewVerdict(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		wantVerdict  ReviewVerdict
		wantFeedback string
	}{
		{
			name:         "verdict on first line",
			text:         "VERDICT: APPROVE\n\nok",
			wantVerdict:  VerdictApprove,
			wantFeedback: "ok",
		},
		{
			name:         "verdict on second line",
			text:         "Checked the diff.\nVERDICT: APPROVE\n\nall good",
			wantVerdict:  VerdictApprove,
			wantFeedback: "all good",
		},
		{
			name:         "concerns after other text",
			text:         "Ran the tests.\n\nVERDICT: CONCERNS\n\nfix X",
			wantVerdict:  VerdictConcerns,
			wantFeedback: "fix X",
		},
		{
			name:        "only first verdict line counts",
			text:        "VERDICT: CONCERNS\nlater I say APPROVE",
			wantVerdict: VerdictConcerns,
		},
		{
			name:        "no verdict line at all",
			text:        "looks fine to me",
			wantVerdict: VerdictConcerns,
		},
		{
			name:         "case and whitespace ignored",
			text:         "  verdict: approve  \nx",
			wantVerdict:  VerdictApprove,
			wantFeedback: "x",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotVerdict, gotFeedback := parseReviewVerdict(tc.text)
			if gotVerdict != tc.wantVerdict {
				t.Errorf("verdict = %q, want %q", gotVerdict, tc.wantVerdict)
			}
			if tc.wantFeedback != "" && gotFeedback != tc.wantFeedback {
				t.Errorf("feedback = %q, want %q", gotFeedback, tc.wantFeedback)
			}
		})
	}
}
