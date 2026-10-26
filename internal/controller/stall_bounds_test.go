package controller

// Boundary and branch-coverage tests for the pure string/byte helpers used to
// bound condition messages (maxConditionMessageBytes) and old-worker stall
// event notes (maxEventNoteBytes). No clients needed.

import (
	"fmt"
	"strings"
	"testing"
)

// sampleText is the truncation sample shared by the table cases below.
const sampleText = "hello"

// msA names the MachineSet used by the stall-message table cases.
const msA = "ms-a"

func TestTruncateToBytes(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"maxBytes zero", sampleText, 0, ""},
		{"maxBytes negative", sampleText, -5, ""},
		{"empty input", "", 5, ""},
		{"len less than max", "hi", 5, "hi"},
		// boundary: exactly max, max-1, max+1
		{"len exactly max", sampleText, 5, sampleText},
		{"len max minus one keeps all", sampleText, 6, sampleText},
		{"len max plus one truncates", sampleText, 4, "hell"},
		{"truncate to zero length prefix", sampleText, 0, ""},
		{"truncate one byte", sampleText, 1, "h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateToBytes(tt.in, tt.maxBytes); got != tt.want {
				t.Fatalf("truncateToBytes(%q, %d) = %q, want %q", tt.in, tt.maxBytes, got, tt.want)
			}
		})
	}
}

func TestCountMachineEntries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"whitespace only", "   ", 0},
		{"single name", "a", 1},
		{"single name no separator", "machine-abc", 1},
		{"comma separated", "a, b, c", 3},
		{"two comma separated", "a, b", 2},
		// ")"-style entries take precedence over comma counting
		{"paren entries", "m1), m2)", 2},
		{"paren entry single", "m1)", 1},
		{"parens beat commas", "a, b), c", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countMachineEntries(tt.in); got != tt.want {
				t.Fatalf("countMachineEntries(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestStallOmitSuffix(t *testing.T) {
	if got := stallOmitSuffix(nil); got != "" {
		t.Fatalf("stallOmitSuffix(nil) = %q, want empty", got)
	}
	if got := stallOmitSuffix([]string{"a", "b"}); got != "; ... and 2 more machine(s) omitted: [a, b]" {
		t.Fatalf("stallOmitSuffix([a b]) = %q", got)
	}
}

func TestStallOmitSuffixFit(t *testing.T) {
	t.Run("empty omitted", func(t *testing.T) {
		if got := stallOmitSuffixFit(nil, 100); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
	t.Run("non-positive budget truncates to empty", func(t *testing.T) {
		if got := stallOmitSuffixFit([]string{"a"}, 0); got != "" {
			t.Fatalf("got %q, want empty for maxSuffixBytes=0", got)
		}
		if got := stallOmitSuffixFit([]string{"a"}, -3); got != "" {
			t.Fatalf("got %q, want empty for negative budget", got)
		}
	})
	t.Run("suffix fits returned as-is", func(t *testing.T) {
		omitted := []string{"a", "b"}
		want := stallOmitSuffix(omitted)
		if got := stallOmitSuffixFit(omitted, len(want)+10); got != want {
			t.Fatalf("got %q, want full suffix %q", got, want)
		}
	})
	t.Run("names dropped one by one until it fits", func(t *testing.T) {
		omitted := []string{"alpha", "bravo", "charlie"}
		// A budget that fits the count prefix plus exactly one short name but
		// not all three, so the loop must drop names.
		maxSuffixBytes := len("; ... and 3 more machine(s) omitted: [alpha]")
		got := stallOmitSuffixFit(omitted, maxSuffixBytes)
		if len(got) > maxSuffixBytes {
			t.Fatalf("got %d bytes (%q), want <= %d", len(got), got, maxSuffixBytes)
		}
		if !strings.HasPrefix(got, "; ... and 3 more machine(s) omitted: [") {
			t.Fatalf("got %q, want the 3-more omitted prefix", got)
		}
	})
	t.Run("budget only fits the count", func(t *testing.T) {
		omitted := []string{"a", "b", "c"}
		// countPrefix is 37 bytes; a 10-byte budget leaves no room for names, so
		// the count-only form is truncated to the budget.
		got := stallOmitSuffixFit(omitted, 10)
		if len(got) > 10 {
			t.Fatalf("got %d bytes (%q), want <= 10", len(got), got)
		}
		if !strings.HasPrefix(got, "; ... and") {
			t.Fatalf("got %q, want the count-only truncated form", got)
		}
	})
	t.Run("budget fits prefix but no name (names exhausted fallback)", func(t *testing.T) {
		omitted := []string{"ab"}
		countPrefix := fmt.Sprintf("; ... and %d more machine(s) omitted: [", len(omitted))
		// available > 0 (enter the loop) but even the single name makes the trial
		// exceed the budget, so all names are dropped and the post-loop count-only
		// fallback is returned (distinct from the early available<=0 return).
		maxSuffixBytes := len(countPrefix) + 2
		got := stallOmitSuffixFit(omitted, maxSuffixBytes)
		if got != "; ... and 1 more machine(s) omitted" {
			t.Fatalf("got %q, want the count-only fallback", got)
		}
	})
}

func TestBoundOldWorkerStallEventNote(t *testing.T) {
	t.Run("within limit unchanged", func(t *testing.T) {
		parts := []stallDetailPart{
			{machineSetName: msA, text: "machines [a, b] stalled", machineNames: []string{"a", "b"}},
		}
		want := oldWorkerStallEventPrefix + "machines [a, b] stalled"
		if got := boundOldWorkerStallEventNote(parts); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
	t.Run("over limit omits trailing machines", func(t *testing.T) {
		parts := []stallDetailPart{
			{machineSetName: msA, text: strings.Repeat("x", 600), machineNames: []string{"m1", "m2"}},
			{machineSetName: "ms-b", text: strings.Repeat("y", 600), machineNames: []string{"m3"}},
		}
		got := boundOldWorkerStallEventNote(parts)
		if len(got) > maxEventNoteBytes {
			t.Fatalf("got %d bytes, want <= %d", len(got), maxEventNoteBytes)
		}
		if !strings.HasPrefix(got, oldWorkerStallEventPrefix) {
			t.Fatalf("got %q, want the stall prefix", got)
		}
		if !strings.HasSuffix(got, "; ... and 1 more machine(s) omitted: [m3]") {
			t.Fatalf("got %q, want m3 summarized as omitted", got)
		}
	})
	t.Run("over limit with no includable part", func(t *testing.T) {
		parts := []stallDetailPart{
			{machineSetName: msA, text: strings.Repeat("z", 1100), machineNames: []string{"m1"}},
		}
		got := boundOldWorkerStallEventNote(parts)
		if len(got) > maxEventNoteBytes {
			t.Fatalf("got %d bytes, want <= %d", len(got), maxEventNoteBytes)
		}
		want := oldWorkerStallEventPrefix + "; ... and 1 more machine(s) omitted: [m1]"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

// TestBoundConditionMessageBounds covers the truncation branches of
// boundConditionMessage not exercised by TestBoundConditionMessage (which
// covers the within-limit and comma-separator paths): the exact-limit boundary,
// the "; " separator path, the no-separator hard-truncate path, and the
// omitted-clamp-to-one path.
func TestBoundConditionMessageBounds(t *testing.T) {
	t.Run("exactly max unchanged", func(t *testing.T) {
		msg := strings.Repeat("a", maxConditionMessageBytes)
		if got := boundConditionMessage(msg); got != msg {
			t.Fatalf("len(got)=%d, want %d unchanged", len(got), maxConditionMessageBytes)
		}
	})
	t.Run("one over max with no separators", func(t *testing.T) {
		msg := strings.Repeat("a", maxConditionMessageBytes+1)
		got := boundConditionMessage(msg)
		if len(got) > maxConditionMessageBytes {
			t.Fatalf("got %d bytes, want <= %d", len(got), maxConditionMessageBytes)
		}
		if !strings.HasSuffix(got, "; ... and 1 more machine(s) omitted") {
			t.Fatalf("got %q, want the omitted marker", got)
		}
	})
	t.Run("over limit with semicolon separators", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 1600; i++ {
			fmt.Fprintf(&b, "[w%d-0, w%d-1, w%d-2, w%d-3]; ", i, i, i, i)
		}
		msg := b.String()
		if len(msg) <= maxConditionMessageBytes {
			t.Fatalf("test setup: msg is %d bytes, want > %d", len(msg), maxConditionMessageBytes)
		}
		got := boundConditionMessage(msg)
		if len(got) > maxConditionMessageBytes {
			t.Fatalf("got %d bytes, want <= %d", len(got), maxConditionMessageBytes)
		}
		if !strings.HasSuffix(got, " more machine(s) omitted") {
			t.Fatalf("got %q, want the omitted marker", got)
		}
		if !strings.Contains(got, "[w0-0, w0-1, w0-2, w0-3]") {
			t.Fatalf("got %q, want the first machine group preserved", got)
		}
	})
	t.Run("omitted clamped to one when prefix preserves all machines", func(t *testing.T) {
		// All machines sit before the "; " separator; cutting at the separator
		// preserves every machine, so omitted would be 0 and must clamp to 1.
		msg := "[a, b, c]; " + strings.Repeat("f", maxConditionMessageBytes)
		if len(msg) <= maxConditionMessageBytes {
			t.Fatalf("test setup: msg is %d bytes, want > %d", len(msg), maxConditionMessageBytes)
		}
		got := boundConditionMessage(msg)
		if !strings.HasSuffix(got, "; ... and 1 more machine(s) omitted") {
			t.Fatalf("got %q, want omitted clamped to 1", got)
		}
	})
}

// Residual (uncovered) branches in boundConditionMessage, both effectively
// unreachable given its construction:
//   - L2569 (maxPrefixLen <= 0): needs len(suffixEst) >= maxConditionMessageBytes,
//     i.e. the machine count to have ~32,700 decimal digits (~10^32700 machines),
//     which a just-over-limit message cannot contain.
//   - L2587 (final truncate): len(prefix) <= maxPrefixLen and len(suffix) <=
//     len(suffixEst) (omitted <= totalMachines, so the count has no more digits),
//     so len(prefix)+len(suffix) can never exceed maxConditionMessageBytes.

func TestCountMachinesInMessage(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want int
	}{
		{"no groups", "hello world", 0},
		{"empty message", "", 0},
		{"single group comma", "failed [a, b, c]", 3},
		{"single group single", "failed [a]", 1},
		{"multiple groups", "x [a, b] y [c, d, e]", 5},
		{"empty group", "x [] y", 0},
		{"paren entries", "x [m1), m2)] y", 2},
		// unclosed bracket: counts the remainder
		{"unclosed group", "x [a, b, c", 3},
		{"unclosed group single", "x [a", 1},
		{"nested-looking groups", "x [a] mid [b, c]", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countMachinesInMessage(tt.msg); got != tt.want {
				t.Fatalf("countMachinesInMessage(%q) = %d, want %d", tt.msg, got, tt.want)
			}
		})
	}
}
