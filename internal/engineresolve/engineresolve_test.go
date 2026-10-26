package engineresolve

import "testing"

func TestCoding(t *testing.T) {
	cases := []struct {
		name                         string
		noteEngine, repoEngine, dflt string
		want                         string
	}{
		{"note pin wins over everything", "pinned", "repo-eng", "default-eng", "pinned"},
		{"repo default wins when note unset", "", "repo-eng", "default-eng", "repo-eng"},
		{"global default when both unset", "", "", "default-eng", "default-eng"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Coding(c.noteEngine, c.repoEngine, c.dflt); got != c.want {
				t.Errorf("Coding(%q, %q, %q) = %q, want %q", c.noteEngine, c.repoEngine, c.dflt, got, c.want)
			}
		})
	}
}

func TestReview(t *testing.T) {
	cases := []struct {
		name                                     string
		noteReviewEngine, repoReviewEngine, dflt string
		want                                     string
	}{
		{"note pin wins over everything", "pinned-review", "repo-review", "default-review", "pinned-review"},
		{"repo default wins when note unset", "", "repo-review", "default-review", "repo-review"},
		{"global default when both unset", "", "", "default-review", "default-review"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Review(c.noteReviewEngine, c.repoReviewEngine, c.dflt); got != c.want {
				t.Errorf("Review(%q, %q, %q) = %q, want %q", c.noteReviewEngine, c.repoReviewEngine, c.dflt, got, c.want)
			}
		})
	}
}

// TestCodingAndReviewAreIndependent confirms the two roles never leak into
// each other -- a task coded with one engine can be reviewed by a
// completely different one.
func TestCodingAndReviewAreIndependent(t *testing.T) {
	coding := Coding("", "coder-engine", "default-eng")
	review := Review("", "reviewer-engine", "default-eng")
	if coding == review {
		t.Fatalf("coding (%q) and review (%q) resolved to the same engine from distinct repo configs -- they should resolve independently", coding, review)
	}
}
