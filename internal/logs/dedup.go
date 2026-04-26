package logs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/rus-99-pk/srengine/internal/config"
)

// Compiled regexes for log line normalization.
var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)
	reIP        = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?`)
	reUUID      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reHex       = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	rePath      = regexp.MustCompile(`(/[\w.\-]+){2,}`)
	reQuoted    = regexp.MustCompile(`"[^"]{0,80}"`)
	reNumber    = regexp.MustCompile(`\b\d+(\.\d+)?\b`)
)

// LogPattern represents a deduplicated log line template with occurrence metadata.
type LogPattern struct {
	Template string
	Count    int
	First    time.Time
	Last     time.Time
	Sample   string // one original line for context
	Level    string
}

// FormatForLLM returns a compact single-line representation for the model context.
func (p *LogPattern) FormatForLLM() string {
	return strings.Join([]string{
		"[" + p.Level + "]",
		fmt.Sprintf("×%d", p.Count),
		p.Template,
		"(first: " + p.First.Format(time.RFC3339) + ")",
	}, " ")
}

type Deduplicator struct {
	cfg config.LogsConfig
}

func NewDeduplicator(cfg config.LogsConfig) *Deduplicator {
	return &Deduplicator{cfg: cfg}
}

// Process filters lines by level, deduplicates them, and returns sorted patterns.
func (d *Deduplicator) Process(lines []string) []LogPattern {
	filtered := d.filterByLevel(lines)

	// Group lines by normalized template
	patterns := make(map[string]*LogPattern)
	for _, raw := range filtered {
		key := normalize(raw)
		if p, ok := patterns[key]; ok {
			p.Count++
			p.Last = extractTime(raw)
		} else {
			patterns[key] = &LogPattern{
				Template: key,
				Count:    1,
				First:    extractTime(raw),
				Last:     extractTime(raw),
				Sample:   raw,
				Level:    extractLevel(raw),
			}
		}
	}

	// Sort by frequency descending — most common patterns first
	result := make([]LogPattern, 0, len(patterns))
	for _, p := range patterns {
		result = append(result, *p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	// Cap at MaxPatterns to stay within model context budget
	if len(result) > d.cfg.MaxPatterns {
		result = result[:d.cfg.MaxPatterns]
	}

	return result
}

// FormatForLLM joins all patterns into a single string for the model context.
func (d *Deduplicator) FormatForLLM(patterns []LogPattern) string {
	var sb strings.Builder
	for i, p := range patterns {
		sb.WriteString(p.FormatForLLM())
		if i < len(patterns)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// filterByLevel discards lines whose level is not in the configured allowlist.
func (d *Deduplicator) filterByLevel(lines []string) []string {
	if len(d.cfg.Levels) == 0 {
		return lines
	}

	levelSet := make(map[string]struct{})
	for _, l := range d.cfg.Levels {
		levelSet[strings.ToUpper(l)] = struct{}{}
	}

	result := make([]string, 0, len(lines))
	for _, line := range lines {
		lvl := extractLevel(line)
		if _, ok := levelSet[lvl]; ok {
			result = append(result, line)
		}
	}
	return result
}

// normalize replaces variable parts of a log line with placeholders.
func normalize(line string) string {
	s := reTimestamp.ReplaceAllString(line, "<TS>")
	s = reIP.ReplaceAllString(s, "<ADDR>")
	s = reUUID.ReplaceAllString(s, "<UUID>")
	s = reHex.ReplaceAllString(s, "<HEX>")
	s = rePath.ReplaceAllString(s, "<PATH>")
	s = reQuoted.ReplaceAllString(s, "<STR>")
	s = reNumber.ReplaceAllString(s, "<N>")
	return strings.TrimSpace(s)
}

// extractLevel detects the log level from a line by keyword matching.
func extractLevel(line string) string {
	upper := strings.ToUpper(line)
	for _, lvl := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		if strings.Contains(upper, lvl) {
			return lvl
		}
	}
	return "UNKNOWN"
}

// extractTime parses the first timestamp found in the line, falling back to now.
func extractTime(line string) time.Time {
	loc := reTimestamp.FindString(line)
	if loc == "" {
		return time.Now()
	}
	formats := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, loc); err == nil {
			return t
		}
	}
	return time.Now()
}