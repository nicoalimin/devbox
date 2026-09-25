package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	// MaxReportedErrors bounds how many distinct compiler errors are fed into
	// repair prompts and blocker reasons.
	MaxReportedErrors = 30
	// Generic (non-TypeScript) output keeps the head, where root causes
	// usually are, plus a short tail for trailing summaries.
	genericHeadLimit = 12 * 1024
	genericTailLimit = 2 * 1024
	maxLocations     = 3
)

var (
	ansiRe = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	// tsc default style, optionally prefixed by pnpm ("pkg typecheck: "):
	//   src/a.ts(12,5): error TS2345: Argument of type ...
	tsParenRe = regexp.MustCompile(`^(.*?: )?([^\s:][^\s]*?)\((\d+),(\d+)\): error (TS\d+): (.*)$`)
	// tsc --pretty style:
	//   src/a.ts:12:5 - error TS2345: Argument of type ...
	tsPrettyRe = regexp.MustCompile(`^(.*?: )?([^\s:][^\s]*?):(\d+):(\d+) - error (TS\d+): (.*)$`)
	elidedRe   = regexp.MustCompile(`\band \d+ more\b`)
	// Timestamps (ISO/RFC3339, "[10:31:02 AM]" watch prefixes, bare clock times).
	timestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?|\[?\d{1,2}:\d{2}:\d{2}(?:\s?[AP]M)?\]?`)
	// Absolute temp/worktree prefixes that differ between runs.
	tempPrefixRe = regexp.MustCompile(`/[^\s'"]*?/\.devbox-worktrees/[^/\s'"]+/|(?:/private)?/(?:tmp|var/folders/[^/\s]+/[^/\s]+/T)/[^/\s'"]+/`)
	durationRe   = regexp.MustCompile(`\d+(?:\.\d+)?\s?(?:ms|s|sec|seconds)\b`)
	digitsRe     = regexp.MustCompile(`\d+`)
	spaceRe      = regexp.MustCompile(`\s+`)
)

// TSError is one distinct TypeScript diagnostic. Message includes indented
// continuation lines (joined with "\n").
type TSError struct {
	Code      string
	File      string
	Line      int
	Col       int
	Message   string
	Count     int
	Locations []string // up to maxLocations example "file(line,col)"
}

// TSSummary is the deduplicated view of tsc output, in first-seen order so
// root errors at the head of the output are never dropped.
type TSSummary struct {
	Errors []TSError
	Total  int  // raw error lines
	Elided bool // TypeScript elided members ("... and N more")
}

// Distinct returns the number of distinct errors.
func (s TSSummary) Distinct() int { return len(s.Errors) }

type rawTSError struct {
	code, file, message string
	line, col           int
}

// ExtractTSErrors parses tsc diagnostics (plain, pnpm-prefixed, or --pretty)
// and dedupes them by (code, normalized message).
func ExtractTSErrors(output string) TSSummary {
	raws := parseTSErrors(output)
	var summary TSSummary
	index := map[string]int{}
	for _, raw := range raws {
		summary.Total++
		if elidedRe.MatchString(raw.message) {
			summary.Elided = true
		}
		key := raw.code + "\x00" + normalizeMessage(raw.message, "")
		loc := fmt.Sprintf("%s(%d,%d)", raw.file, raw.line, raw.col)
		if i, ok := index[key]; ok {
			summary.Errors[i].Count++
			if len(summary.Errors[i].Locations) < maxLocations {
				summary.Errors[i].Locations = append(summary.Errors[i].Locations, loc)
			}
			continue
		}
		index[key] = len(summary.Errors)
		summary.Errors = append(summary.Errors, TSError{
			Code: raw.code, File: raw.file, Line: raw.line, Col: raw.col,
			Message: raw.message, Count: 1, Locations: []string{loc},
		})
	}
	return summary
}

func parseTSErrors(output string) []rawTSError {
	lines := strings.Split(ansiRe.ReplaceAllString(strings.ReplaceAll(output, "\r\n", "\n"), ""), "\n")
	var errs []rawTSError
	for i := 0; i < len(lines); i++ {
		m := tsParenRe.FindStringSubmatch(lines[i])
		if m == nil {
			m = tsPrettyRe.FindStringSubmatch(lines[i])
		}
		if m == nil {
			continue
		}
		prefix := m[1]
		line, _ := strconv.Atoi(m[3])
		col, _ := strconv.Atoi(m[4])
		message := []string{strings.TrimSpace(m[6])}
		// Indented continuation lines (possibly carrying the same pnpm prefix).
		for i+1 < len(lines) {
			next := lines[i+1]
			if prefix != "" {
				if !strings.HasPrefix(next, prefix) {
					break
				}
				next = strings.TrimPrefix(next, prefix)
			}
			if strings.TrimSpace(next) == "" || (!strings.HasPrefix(next, "  ") && !strings.HasPrefix(next, "\t")) {
				break
			}
			if tsParenRe.MatchString(lines[i+1]) || tsPrettyRe.MatchString(lines[i+1]) {
				break
			}
			message = append(message, "  "+strings.TrimSpace(next))
			i++
		}
		errs = append(errs, rawTSError{
			code: m[5], file: strings.TrimPrefix(m[2], "./"),
			line: line, col: col, message: strings.Join(message, "\n"),
		})
	}
	return errs
}

// Format renders at most limit distinct errors plus total/distinct counts.
func (s TSSummary) Format(limit int) string {
	if limit <= 0 {
		limit = MaxReportedErrors
	}
	var b strings.Builder
	fmt.Fprintf(&b, "TypeScript errors: %d total, %d distinct", s.Total, s.Distinct())
	if s.Distinct() > limit {
		fmt.Fprintf(&b, " (showing first %d)", limit)
	}
	b.WriteString("\n")
	for i, e := range s.Errors {
		if i >= limit {
			fmt.Fprintf(&b, "... %d more distinct error(s) omitted\n", s.Distinct()-limit)
			break
		}
		fmt.Fprintf(&b, "%d. %s(%d,%d): error %s: %s", i+1, e.File, e.Line, e.Col, e.Code, e.Message)
		if e.Count > 1 {
			fmt.Fprintf(&b, "\n   [%d occurrences; e.g. at %s]", e.Count, strings.Join(e.Locations, ", "))
		}
		b.WriteString("\n")
	}
	if s.Elided {
		b.WriteString("Note: TypeScript elided part of a member list (\"... and N more\"). Open the named interface/type declaration and implement every member it requires, not only the ones listed.\n")
	}
	return b.String()
}

// Failure is a validation command failure with a deduplicated summary and a
// stable signature for repeat-failure detection.
type Failure struct {
	Command   string
	Err       error
	Output    []byte
	TS        TSSummary
	Signature string
}

// NewFailure builds a Failure from a command's raw output. worktreePath is
// stripped from paths so signatures are stable across worktrees.
func NewFailure(command string, err error, output []byte, worktreePath string) *Failure {
	text := string(output)
	return &Failure{
		Command:   command,
		Err:       err,
		Output:    output,
		TS:        ExtractTSErrors(text),
		Signature: Signature(command, text, worktreePath),
	}
}

func (f *Failure) Error() string {
	return fmt.Sprintf("local validation failed: %s: %v\n%s", f.Command, f.Err, f.Summary())
}

func (f *Failure) Unwrap() error { return f.Err }

// Summary is the deduped TypeScript report, or head+tail of other output.
func (f *Failure) Summary() string {
	if f.TS.Total > 0 {
		return f.TS.Format(MaxReportedErrors)
	}
	return "Output:\n" + headTail(f.Output, genericHeadLimit, genericTailLimit)
}

// Signature returns a stable hash of a failure: normalized, sorted, deduped
// TypeScript code+file+message entries (no line numbers, timestamps, or
// absolute temp paths). Non-TypeScript output falls back to normalized
// error-looking lines.
func Signature(command, output, worktreePath string) string {
	var entries []string
	for _, raw := range parseTSErrors(output) {
		entries = append(entries, raw.code+"|"+normalizePath(raw.file, worktreePath)+"|"+normalizeMessage(raw.message, worktreePath))
	}
	if len(entries) == 0 {
		entries = genericSignatureLines(output, worktreePath)
		entries = append(entries, "cmd|"+normalizeMessage(command, worktreePath))
	}
	sort.Strings(entries)
	h := sha256.New()
	prev := ""
	for i, e := range entries {
		if i > 0 && e == prev {
			continue
		}
		prev = e
		h.Write([]byte(e))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func genericSignatureLines(output, worktreePath string) []string {
	var matched, first []string
	for _, line := range strings.Split(ansiRe.ReplaceAllString(output, ""), "\n") {
		norm := normalizeMessage(line, worktreePath)
		norm = durationRe.ReplaceAllString(norm, "")
		norm = strings.TrimSpace(digitsRe.ReplaceAllString(norm, "#"))
		if norm == "" {
			continue
		}
		lower := strings.ToLower(norm)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") {
			matched = append(matched, norm)
		}
		if len(first) < 40 {
			first = append(first, norm)
		}
	}
	if len(matched) > 0 {
		return matched
	}
	return first
}

func normalizePath(path, worktreePath string) string {
	path = strings.ReplaceAll(path, "\\", "/")
	if worktreePath != "" {
		root := strings.TrimSuffix(strings.ReplaceAll(worktreePath, "\\", "/"), "/") + "/"
		path = strings.TrimPrefix(path, root)
	}
	path = tempPrefixRe.ReplaceAllString(path, "")
	return strings.TrimPrefix(path, "./")
}

func normalizeMessage(message, worktreePath string) string {
	message = ansiRe.ReplaceAllString(message, "")
	if worktreePath != "" {
		message = strings.ReplaceAll(message, strings.TrimSuffix(worktreePath, "/")+"/", "")
	}
	message = tempPrefixRe.ReplaceAllString(message, "")
	message = timestampRe.ReplaceAllString(message, "")
	return strings.TrimSpace(spaceRe.ReplaceAllString(message, " "))
}

// headTail keeps the head of output (root causes) plus a short tail.
func headTail(output []byte, headLimit, tailLimit int) string {
	if len(output) <= headLimit+tailLimit {
		return string(output)
	}
	omitted := len(output) - headLimit - tailLimit
	return string(output[:headLimit]) +
		fmt.Sprintf("\n... %d bytes omitted ...\n", omitted) +
		string(output[len(output)-tailLimit:])
}
