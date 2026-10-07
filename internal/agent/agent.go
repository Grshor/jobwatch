// Package agent: deep vacancy analysis. The daemon feeds the model the
// vacancy's full text (fetched over the hirify Agent API) and the
// candidate's resume text — one plain completion, no browsing, no tools.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const defaultTimeout = 8 * time.Minute

// Analysis is the structured answer parsed out of the model's reply.
type Analysis struct {
	Verdict string // откликаться | пропускать | unknown
	Traps   string
	Advice  string
	Letter  string
	Raw     string
}

// Analyzer runs one headless omp completion.
type Analyzer struct {
	Bin        string
	WorkDir    string
	Timeout    time.Duration
	ResumeText string // the candidate's resume, read once at startup
}

const promptTmpl = `Ты — придирчивый senior-кандидат, оцениваешь вакансию перед откликом. Ожидания: senior Go backend, Москва или удалёнка, от 400к gross, не агентство и не аутстафф.

РЕЗЮМЕ КАНДИДАТА:
<resume>
%s
</resume>

ТЕКСТ ВАКАНСИИ:
<vacancy>
%s
</vacancy>

Проанализируй вакансию и ответь СТРОГО в этом формате (без markdown-разметки заголовков):

ВЕРДИКТ: откликаться | пропускать
ЛОВУШКИ: (скрининговые фильтры против резюме, вилки ниже ожиданий, агентство/аутстафф под видом продукта, тестовое до отклика, признаки мёртвой вакансии — по одной строке; если чисто — «нет»)
СОВЕТ: (1–2 строки: какие буллеты резюме подчеркнуть под эту вакансию)
ПИСЬМО: (сопроводительное от первого лица, 4–6 строк, по-русски, только реальный опыт из резюме)
`

// AnalyzeText runs one completion over resume+vacancy texts.
func (a Analyzer) AnalyzeText(ctx context.Context, vacancyText string) (Analysis, error) {
	timeout := a.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	prompt := fmt.Sprintf(promptTmpl, a.ResumeText, vacancyText)
	// --no-tools is the load-bearing flag: the analysis is a pure text
	// completion. With tools the model occasionally goes agent-mode and
	// starts "applying edits" to the resume directory instead of answering.
	args := []string{"--no-session", "--no-tools", "-p", prompt}
	if a.WorkDir != "" {
		args = append([]string{"--no-session", "--no-tools", "--cwd", a.WorkDir}, args[len(args)-2:]...)
	}
	cmd := exec.CommandContext(ctx, a.Bin, args...)
	var errBuf, outBuf strings.Builder
	cmd.Stderr = &errBuf
	cmd.Stdout = &outBuf
	if err := cmd.Start(); err != nil {
		return Analysis{}, fmt.Errorf("omp start: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var runErr error
timer:
	for {
		select {
		case runErr = <-done:
			break timer
		case <-ctx.Done():
			runErr = ctx.Err()
			break timer
		}
	}
	// The model often finishes printing the analysis before omp finishes
	// finalizing — a partial or timeout-terminated buffer still parses.
	res := parse(outBuf.String())
	if res.Verdict == "unknown" {
		out := outBuf.String()
		if runErr == nil {
			runErr = fmt.Errorf("no verdict in output (%d chars): %.200q", len(out), out)
		}
		tail := errBuf.String()
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return Analysis{}, fmt.Errorf("omp: %w: %s", runErr, tail)
	}
	return res, nil
}

// sectionRe tolerates the model's markdown habits: **ВЕРДИКТ:**, bullets.
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
