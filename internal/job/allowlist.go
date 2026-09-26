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
	// strip "1)" / "2." style list markers used in MUST-WRITE fences
	if i := strings.IndexAny(line, ")."); i >= 0 && i < 4 {
		prefix := line[:i]
		allDigits := true
		for _, r := range prefix {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits && prefix != "" {
			line = strings.TrimSpace(line[i+1:])
		}
	}
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

// FormatAllowlistEmptyFailure builds the UTA-105 empty-diff failure message.
func FormatAllowlistEmptyFailure(allowlist []string) string {
	return fmt.Sprintf(
		"allowlist push gate failed: no changes within ALLOWLIST %v (agent produced an empty diff)",
		allowlist,
	)
}

// InScopeCount returns how many changed paths match the allowlist.
func InScopeCount(allowlist, changed []string) int {
	if len(allowlist) == 0 {
		return len(changed)
	}
	n := 0
	for _, f := range changed {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if pathAllowed(f, allowlist) {
			n++
		}
	}
	return n
}

// ParseMustWrite extracts exact paths from a MUST-WRITE: / MUST-WRITE (…): block.
// Same line-collection rules as ALLOWLIST. Empty means inactive.
func ParseMustWrite(texts ...string) []string {
	var out []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, p := range parseLabeledPathBlock(text, "must-write") {
			if seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func parseLabeledPathBlock(text, label string) []string {
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
			if strings.HasPrefix(lower, label) {
				// "must-write:" or "must-write (...):"
				colon := strings.Index(lower, ":")
				if colon < 0 {
					continue
				}
				inBlock = true
				rest := strings.TrimSpace(line[colon+1:])
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
		p := normalizeAllowlistPath(line)
		if p == "" {
			if strings.HasPrefix(lower, "forbidden") || strings.HasPrefix(lower, "done when") || strings.HasPrefix(lower, "allowlist") {
				break
			}
			continue
		}
		out = append(out, p)
	}
	return out
}

// MissingMustWrite returns MUST-WRITE paths not present in the changed list
// (exact path match). Empty mustWrite → nil (inactive).
func MissingMustWrite(mustWrite, changed []string) []string {
	if len(mustWrite) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, f := range changed {
		f = strings.TrimSpace(f)
		if f != "" {
			have[f] = true
		}
	}
	var missing []string
	for _, req := range mustWrite {
		if !have[req] {
			missing = append(missing, req)
		}
	}
	return missing
}

func isEmptyAgentWorkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "agent produced no file changes") ||
		strings.Contains(msg, "nothing to push") && strings.Contains(msg, "format-only")
}
