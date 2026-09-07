package route

import "testing"

func TestParseUnlimited(t *testing.T) {
	p, err := Parse("POST /messages/count_tokens:unlimited")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Unlimited {
		t.Fatal("pattern must be unlimited")
	}
	if p.Limit != 0 {
		t.Fatalf("limit = %d, want 0 (the unlimited class carries no limit)", p.Limit)
	}
	if p.Group != "" {
		t.Fatalf("group = %q, want empty", p.Group)
	}
}

func TestParseUnlimitedWithGroupRejected(t *testing.T) {
	if _, err := Parse("POST /messages/count_tokens:unlimited@count"); err == nil {
		t.Fatal(":unlimited with a group must be rejected (the unlimited class has no limiter to share)")
	}
}

func TestParseUnlimitedCoexistsWithNumericLimits(t *testing.T) {
	limited, err := Parse("POST /messages:2")
	if err != nil {
		t.Fatal(err)
	}
	if limited.Unlimited || limited.Limit != 2 {
		t.Fatalf("numeric pattern changed: %+v", limited)
	}
}

func TestMatcherIsUnlimitedFirstMatchWins(t *testing.T) {
	unlimited, _ := Parse("POST /messages/count_tokens:unlimited")
	limited, _ := Parse("POST /messages:2")
	other, _ := Parse("POST /chat/completions:4")

	// Unlimited pattern first: count_tokens is exempt.
	m := NewMatcher([]Pattern{unlimited, limited, other})
	if !m.IsUnlimited("POST", "/v1/messages/count_tokens") {
		t.Fatal("count_tokens must be unlimited when its pattern precedes broader patterns")
	}
	if m.IsUnlimited("POST", "/v1/messages") {
		t.Fatal("/messages must not be exempt")
	}
	if !m.IsLimited("POST", "/v1/messages/count_tokens") {
		t.Fatal("IsLimited still reports the route as matched (the proxy combines both)")
	}

	// Limited pattern first: /messages does NOT match /messages/count_tokens
	// (end-anchored suffix matching), so the unlimited pattern is the FIRST
	// MATCHING pattern and the request is exempt — classification and
	// limiter selection stay consistent (both use first-MATCHING-wins).
	m2 := NewMatcher([]Pattern{limited, unlimited, other})
	if !m2.IsUnlimited("POST", "/v1/messages/count_tokens") {
		t.Fatal("the first MATCHING pattern decides: /messages does not capture its sub-resource")
	}

	// A genuinely overlapping broader pattern (same path) that precedes the
	// unlimited one wins: first-MATCHING-wins.
	broaderLimited, _ := Parse("POST /messages/count_tokens:2")
	m3 := NewMatcher([]Pattern{broaderLimited, unlimited, other})
	if m3.IsUnlimited("POST", "/v1/messages/count_tokens") {
		t.Fatal("first-match precedence: a preceding limited pattern on the SAME path wins")
	}

	// No match at all.
	m4 := NewMatcher([]Pattern{other})
	if m4.IsUnlimited("POST", "/v1/messages/count_tokens") {
		t.Fatal("unmatched requests are not unlimited")
	}
}
