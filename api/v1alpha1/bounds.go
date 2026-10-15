package v1alpha1

import "unicode/utf8"

// Upper bounds mirrored from the CRD validation markers; writers cut values to these before any API write.
const (
	MaxPolicyNameLength     = 40
	MaxEvidenceItems        = 10
	MaxEvidenceLength       = 256
	MaxRecentFailures       = 20
	MaxCircuits             = 10
	MaxMessageLength        = 512
	MaxAttempts             = 5
	MaxOldPodsOutstanding   = 5
	MaxCandidates           = 16
	MaxRejectReasons        = 15
	MaxDetailsItems         = 8
	MaxDetailLength         = 256
	MaxLastProbeErrorLength = 256
	TruncatedListMarker     = "...truncated"
	truncatedStringSuffix   = "..."
)

// Truncate returns s cut to at most n runes, ending in "..." when it was cut.
func Truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-len(truncatedStringSuffix)]) + truncatedStringSuffix
}

// CapList returns items limited to n entries; when cut, the last entry is TruncatedListMarker.
// Callers sort items first so the kept prefix is deterministic.
func CapList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	return append(items[:n-1:n-1], TruncatedListMarker)
}
