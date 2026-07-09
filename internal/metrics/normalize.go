package metrics

import "strings"

// idMinLen is the length at which an all-hex or all-alphanumeric segment is
// treated as a synthetic id (UUIDs, long Tesla vehicle ids, GUIDs) and
// collapsed to {id}. Short non-numeric segments (api, vehicles, status) are
// kept verbatim so the route label stays human-readable.
const idMinLen = 12

// numIDMinLen is the length at which an all-digit segment is treated as a
// synthetic numeric id (e.g. a Tesla vehicle id) rather than a small constant
// like an API version. Tesla vehicle ids are ~19 digits; API versions are 1-2,
// so a threshold of 6 cleanly separates them while keeping the label readable.
const numIDMinLen = 6

// NormalizePath collapses high-cardinality path segments to "{id}" so the route
// label has bounded cardinality across protocols. A segment is collapsed when it
// is all digits (any length, e.g. a Tesla vehicle id) or a long opaque token
// (>= idMinLen chars made only of hex/alphanumeric, e.g. a UUID or GUID).
// Everything else is preserved. The leading slash and segment order are kept, so
//
//	/api/1/vehicles/5107700305316668735/vehicle_data
//
// becomes
//
//	/api/1/vehicles/{id}/vehicle_data
//
// and a GraphQL endpoint like /api/gql is left untouched.
func NormalizePath(p string) string {
	if p == "" || p == "/" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if s == "" {
			continue
		}
		if isIDSegment(s) {
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// isIDSegment reports whether a single path segment looks like a synthetic id.
func isIDSegment(s string) bool {
	if len(s) >= numIDMinLen && isAllDigits(s) {
		return true
	}
	// Long opaque tokens (UUIDs, GUIDs) that are only alphanumerics or the
	// dashes UUIDs use. A version like "1" stays (handled by all-digits, but a
	// short word like "gql" or "vehicles" is too short to qualify here).
	if len(s) >= idMinLen && isOpaqueToken(s) {
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// isOpaqueToken reports whether s is made only of hex-ish/alphanumeric chars and
// dashes AND contains at least one digit. Requiring a digit keeps long plain
// words (rare, but e.g. "subscriptions") from collapsing while still catching
// UUIDs/GUIDs, which always include digits.
func isOpaqueToken(s string) bool {
	hasDigit := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			hasDigit = true
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-':
		default:
			return false
		}
	}
	return hasDigit
}
