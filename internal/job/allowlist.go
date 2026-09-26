package job

import (
	"fmt"
	"strings"
)

// ParseAllowlist extracts path prefixes from an ALLOWLIST: block in free-form
// text (Linear description and/or operator_context).
//
// Syntax (UTA-103):
//
//	ALLOWLIST:
//	packages/database/src/repositories/
//	packages/database/src/__tests__/
//
// Lines after ALLOWLIST: are collected until a blank line or a markdown
// heading. Prefix match is enough (directory or file). Empty allowlist means
// the gate is inactive (backward compatible).
func ParseAllowlist(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, p := range parseAllowlistBlock(text) {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func parseAllowlistBlock(text string) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	var out []string
	inBlock := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		lower := strings.ToLower(line)
		if !inBlock {
			if lower == "allowlist:" || strings.HasPrefix(lower, "allowlist:") {
				inBlock = true
				// ALLOWLIST: path on same line
				rest := strings.TrimSpace(line[len("allowlist:"):])
				if rest != "" {
					if p := normalizeAllowlistPath(rest); p != "" {
						out = append(out, p)
					}
				}
				continue
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "##") {
			break
		}
		// bullet / backtick wrappers
		p := normalizeAllowlistPath(line)
		if p == "" {
			// non-path content ends the block
			if strings.HasPrefix(lower, "forbidden") || strings.HasPrefix(lower, "done when") {
				break
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

func normalizeAllowlistPath(line string) string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "-")
	line = strings.TrimPrefix(line, "*")
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "`")
	line = strings.TrimSpace(line)
	// reject prose
	if line == "" || strings.Contains(line, " ") {
		return ""
	}
	if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
		return ""
	}
	// must look like a path
	if !strings.Contains(line, "/") && !strings.Contains(line, ".") {
		return ""
	}
	return line
}

// AllowlistViolations returns changed paths that do not match any allowlist
// prefix. Nil/empty allowlist → no violations (gate inactive).
func AllowlistViolations(allowlist, changed []string) []string {
	if len(allowlist) == 0 {
		return nil
	}
	var bad []string
	for _, f := range changed {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !pathAllowed(f, allowlist) {
			bad = append(bad, f)
		}
	}
	return bad
}

func pathAllowed(file string, allowlist []string) bool {
	for _, prefix := range allowlist {
		if prefix == "" {
			continue
		}
		if file == prefix || strings.HasPrefix(file, prefix) {
			return true
		}
		// allow "dir" to match "dir/..." even without trailing slash
		if !strings.HasSuffix(prefix, "/") && strings.HasPrefix(file, prefix+"/") {
			return true
		}
	}
	return false
}

// FormatAllowlistFailure builds a clear job-failure message.
func FormatAllowlistFailure(allowlist, violations []string) string {
	return fmt.Sprintf(
		"allowlist push gate failed: %d path(s) outside ALLOWLIST %v: %v",
		len(violations), allowlist, violations,
	)
}
