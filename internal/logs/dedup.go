package logs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/your-org/ai-sre/internal/config"
)

var (
	reTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})?`)
	reIP        = regexp.MustCompile(`\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(:\d+)?`)
	reUUID      = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	reHex       = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	rePath      = regexp.MustCompile(`(/[\w.\-]+){2,}`)
	reQuoted    = regexp.MustCompile(`"[^"]{0,80}"`)
	reNumber    = regexp.MustCompile(`\b\d+(\.\d+)?\b`)

	// Уровни логов
	levelPatterns = map[string]*regexp.Regexp{
		"ERROR": regexp.MustCompile(`(?i)\b(error|err|fatal|panic|exception)\b`),
		"WARN":  regexp.MustCompile(`(?i)\b(warn|warning)\b`),
		"INFO":  regexp.MustCompile(`(?i)\binfo\b`),
		"DEBUG": regexp.MustCompile(`(?i)\b(debug|trace)\b`),
	}
)

type LogPattern struct {
	Template string
	Count    int
	First    time.Time
	Last     time.Time
	Sample   string // одна оригинальная строка для контекста
	Level    string
}

// FormatForLLM — форматирует паттерны для отправки в модель
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

// Process принимает сырые строки логов, возвращает дедуплицированные паттерны
func (d *Deduplicator) Process(lines []string) []LogPattern {
	// Фильтруем по уровню
	filtered := d.filterByLevel(lines)

	// Дедуплицируем
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

	// Сортируем по Count desc
	result := make([]LogPattern, 0, len(patterns))
	for _, p := range patterns {
		result = append(result, *p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Count > result[j].Count
	})

	// Ограничиваем количество паттернов
	if len(result) > d.cfg.MaxPatterns {
		result = result[:d.cfg.MaxPatterns]
	}

	return result
}

// FormatForLLM — форматирует все паттерны в одну строку для контекста модели
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

func extractLevel(line string) string {
	upper := strings.ToUpper(line)
	for _, lvl := range []string{"ERROR", "WARN", "INFO", "DEBUG"} {
		if strings.Contains(upper, lvl) {
			return lvl
		}
	}
	return "UNKNOWN"
}

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
