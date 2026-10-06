// Package agent: deep vacancy analysis through the user's own OMP agent,
// run headless and session-less: one `omp --no-session -p` per surviving
// vacancy. The agent reads the real resume files from disk — no resume text
// is copied into prompts or logs beyond what the agent itself answers with.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Grshor/jobwatch/internal/model"
)

const defaultTimeout = 12 * time.Minute

// Analyzer runs one headless omp analysis.
type Analyzer struct {
	Bin        string // "omp"
	WorkDir    string // project with the resume files (agent reads them itself)
	Timeout    time.Duration
	ResumeHint string // extra context line (paths) baked into the prompt
}

// Analysis is the structured answer parsed out of the agent's reply.
type Analysis struct {
	Verdict string // откликаться | пропускать | unknown
	Traps   string
	Advice  string
	Letter  string
	Raw     string
}

const promptTmpl = `Ты помогаешь кандидату (senior Go backend, Москва/удалёнка, ожидание от 400к gross) разобрать вакансию перед откликом. Резюме кандидата: %s — прочитай его. Карточка вакансии: %s. Полный текст открой по ссылке (прочитай URL-адрес инструментом): %s

Проанализируй как придирчивый кандидат и ответь СТРОГО в этом формате:

ВЕРДИКТ: откликаться | пропускать
ЛОВУШКИ: (скрининговые вопросы и «кодовые слова» в тексте, тестовое задание до отклика, вилка ниже ожиданий, агентство/аутстафф под видом продукта, признак мёртвой вакансии — по одной строке на пункт; если чисто — «нет»)
СОВЕТ: (1–2 строки: какие буллеты резюме подчеркнуть под эту вакансию)
ПИСЬМО: (сопроводительное от первого лица, 4–6 строк, по-русски, без воды и без выдуманного опыта)
`

// Analyze runs the agent synchronously; ctx cancellation aborts. The agent
// receives the card summary plus the vacancy URL — it opens the page itself
// with its own tools and reads the full description.
func (a Analyzer) Analyze(ctx context.Context, v model.Vacancy) (Analysis, error) {
	timeout := a.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	hint := a.ResumeHint
	if hint == "" {
		hint = "~/Projects/career/jobseeker/resume-2026.md"
	}
	prompt := fmt.Sprintf(promptTmpl, hint, v.Compact(), v.URL)
	cmd := exec.CommandContext(ctx, a.Bin, "--no-session", "--cwd", a.WorkDir, "-p", prompt)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	if err != nil {
		tail := errBuf.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return Analysis{}, fmt.Errorf("omp: %w: %s", err, tail)
	}
	return parse(string(out)), nil
}

// sectionRe tolerates the agent's markdown habits: **ВЕРДИКТ:**, bullets,
// bold headers. Case-insensitive (Cyrillic included).
var sectionRe = regexp.MustCompile(`(?i)^[\s\*\#>_-]*(ВЕРДИКТ|ЛОВУШКИ|СОВЕТ|ПИСЬМО)[\s\*\#>_-]*:?(.*)$`)

func parse(raw string) Analysis {
	a := Analysis{Verdict: "unknown", Raw: raw}
	cur := ""
	var b strings.Builder
	flush := func() {
		body := strings.TrimSpace(b.String())
		switch cur {
		case "ВЕРДИКТ":
			switch {
			case strings.Contains(body, "пропускать"):
				a.Verdict = "пропускать"
			case strings.Contains(body, "откликаться"):
				a.Verdict = "откликаться"
			}
		case "ЛОВУШКИ":
			a.Traps = body
		case "СОВЕТ":
			a.Advice = body
		case "ПИСЬМО":
			a.Letter = body
		}
		b.Reset()
	}
	for _, line := range strings.Split(raw, "\n") {
		if m := sectionRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = strings.ToUpper(m[1])
			b.WriteString(strings.TrimSpace(m[2]) + "\n")
			continue
		}
		if cur != "" {
			b.WriteString(line + "\n")
		}
	}
	flush()
	return a
}
