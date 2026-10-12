// Package agent: deep vacancy analysis. The daemon feeds the model the
// vacancy's full text (fetched over the hirify Agent API) and the
// candidate's resume text — one plain completion, no browsing, no tools.
package agent

import (
	"context"
	"encoding/json"
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
	Model      string // optional --model pin (e.g. zai/glm-5.3-flash)
}

const promptTmpl = `Ты — придирчивый senior-кандидат, оцениваешь вакансию перед откликом. Ожидания: senior Go backend (5 лет), Москва или удалёнка, от 400к gross, не агентство и не аутстафф.

РЕЗЮМЕ КАНДИДАТА:
<resume>
%s
</resume>

ТЕКСТ ВАКАНСИИ:
<vacancy>
%s
</vacancy>

ПРАВИЛА ОТСЕВА — verdict="пропускать", если выполняется ХОТЬ ОДНО:
- требуемый опыт больше 6 лет;
- вилка указана и ниже 400к gross (для RU-рынка);
- агентство, аутстафф, подрядчик на проект другого заказчика, консалтинг под видом продукта;
- обязательная релокация в другую страну;
- iGaming, букмекеры, казино.
Если стоп-факторов нет и стек совпадает — verdict="откликаться". Не выдумывай проблемы и не смягчай вердикт из вежливости: если ловушки фатальны — пропускай. Нет ловушек — traps="нет".

Ответь ОДНИМ JSON-объектом без какого-либо другого текста:
{"verdict":"откликаться|пропускать","traps":"скрининговые фильтры против резюме, вилки ниже ожиданий, агентство/аутстафф под видом продукта, тестовое до отклика, признаки мёртвой вакансии — по одной строке через ; если чисто — нет","advice":"1-2 строки: какие буллеты резюме подчеркнуть под эту вакансию","letter":"сопроводительное"}

ТРЕБОВАНИЯ К ПИСЬМУ (letter) — пишет живой уставший сеньор, НЕ ИИ:
Запрещены: жирные выделения, восторги и лесть («роль цепляет», «отличный шанс»), мелодрама («знаю цену переходам»), канцелярит, «как указано в вакансии», обещания без фактов, «я идеальный кандидат».
Обязательно: конкретная механика с числами ИЗ РЕЗЮМЕ (exactly-once на 5 000 событий/сек, 10 000+ транзакций/мин, ноль дедлоков на PostgreSQL, 76 нс на сделку — подставляй только релевантное вакансии), одна честная строка про пробел без оправданий (если пробел есть), blunt вопрос про логистику/оформление в конце. Гладкость жертвуешь в пользу конкретики.
ОБРАЗЕЦ ГОЛОСА (копируй тон, НЕ содержание):
"Здравствуйте.

Откликаюсь на позицию в Uzum Nasiya — задача близкая: последний год в Сбере делаю Kafka-консюмеры под нагрузкой. Импортёр на 5 000 событий/сек с exactly-once: дедуп-ключи, ретраи, откат без потерь и дублей. До этого в банке — сервисы под PostgreSQL: транзакционная шина, после которой взаимоблокировки ушли полностью.

Формат — удалённо из Москвы, полный день. И сразу вопрос: оформление по ТК УЗ — есть опции для тех, кто остаётся в РФ?"
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
	head := []string{"--no-session", "--no-tools"}
	if a.Model != "" {
		head = append(head, "--model", a.Model)
	}
	args := append(head, "-p", prompt)
	if a.WorkDir != "" {
		args = append(head, "--cwd", a.WorkDir, "-p", prompt)
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
	if a, ok := parseJSON(raw); ok {
		return a
	}
	return parseText(raw)
}

// parseJSON extracts the first {...} block and reads the protocol fields.
func parseJSON(raw string) (Analysis, bool) {
	i, j := strings.Index(raw, "{"), strings.LastIndex(raw, "}")
	if i < 0 || j <= i {
		return Analysis{}, false
	}
	var payload struct {
		Verdict string `json:"verdict"`
		Traps   string `json:"traps"`
		Advice  string `json:"advice"`
		Letter  string `json:"letter"`
	}
	if err := json.Unmarshal([]byte(raw[i:j+1]), &payload); err != nil {
		return Analysis{}, false
	}
	a := Analysis{Verdict: "unknown", Traps: payload.Traps, Advice: payload.Advice, Letter: payload.Letter, Raw: raw}
	switch {
	case strings.Contains(payload.Verdict, "пропуск"):
		a.Verdict = "пропускать"
	case strings.Contains(payload.Verdict, "отклик"):
		a.Verdict = "откликаться"
	default:
		return Analysis{}, false
	}
	return a, true
}

func parseText(raw string) Analysis {
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
