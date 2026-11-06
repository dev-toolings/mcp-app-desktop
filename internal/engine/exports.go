package engine

import (
	"sort"
	"strings"
)

// Entry points for drivers that do not run on CDP (the Tauri bridge): they
// share the engine's target syntax, extraction types and ref tables, so a
// window reads and diffs the same way whatever drives it.

// ParseLocator parses a getByRole/getByText/getByLabel target. isLocator is
// false for anything else (a ref or a CSS selector).
func ParseLocator(target string) (loc Locator, isLocator bool, err error) {
	return parsePlaywrightLocator(target)
}

// ChainedTargetError rejects the unsupported ">>" chaining syntax.
func ChainedTargetError(target string) error { return chainedTargetError(target) }

// RefSnapshotsOf builds the ref table of an extraction (role, name and nth
// of every interactive node), as BuildSnapshot does for a CDP page. Nodes
// have no backend node id outside CDP: every ref is kept.
func RefSnapshotsOf(result *ExtractionResult) map[string]RefSnapshot {
	out := map[string]RefSnapshot{}
	if result == nil {
		return out
	}
	keys := make([]string, 0, len(result.Refs))
	for ref := range result.Refs {
		keys = append(keys, ref)
	}
	sort.Slice(keys, func(i, j int) bool { return refLess(keys[i], keys[j]) })
	nthByKey := map[string]int{}
	for _, ref := range keys {
		node := result.Refs[ref]
		key := strings.ToLower(node.Role) + "|" + strings.ToLower(strings.TrimSpace(node.Name))
		nth := nthByKey[key]
		nthByKey[key] = nth + 1
		out[ref] = RefSnapshot{BackendNodeID: node.BackendNodeID, Role: node.Role, Name: node.Name, Nth: nth, Href: node.Href, FrameID: node.FrameID}
	}
	return out
}

// CanonicalRole turns an agent-mode abbreviation ("b", "t", "a") into its
// role and lower-cases anything else.
func CanonicalRole(role string) string {
	r := strings.TrimSpace(role)
	for full, abbrev := range roleAbbrev {
		if abbrev == r && len(abbrev) <= 2 && full != "StaticText" && full != "image" {
			return full
		}
	}
	return strings.ToLower(r)
}
