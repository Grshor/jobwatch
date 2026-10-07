// Package daemon: the orchestration loop — fetch, dedup, gate, analyze,
// notify. Progressive autonomy in code: the analysis quota is small, cards
// go out with a URL button, and the caps keep the pipeline from ever
// spamming the user or the boards.
package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Grshor/jobwatch/internal/agent"
	"github.com/Grshor/jobwatch/internal/gate"
	"github.com/Grshor/jobwatch/internal/hh"
	"github.com/Grshor/jobwatch/internal/hirify"
	"github.com/Grshor/jobwatch/internal/model"
	"github.com/Grshor/jobwatch/internal/store"
	"github.com/Grshor/jobwatch/internal/tg"
)

// Config mirrors config.yml.
type Config struct {
	Interval  time.Duration `yaml:"interval"`
	StatePath string        `yaml:"state_path"`
	Queries   []struct {
		Source      string   `yaml:"source"` // hh | hirify
		Text        string   `yaml:"text"`
		AreaID      int      `yaml:"area_id"`
		RemoteOnly  bool     `yaml:"remote_only"`
		HirifyPages int      `yaml:"hirify_pages"`
		FeedID      int      `yaml:"feed_id"` // source: hirify-feed — site-saved filter
		Grade       []string `yaml:"grade"`
		WorkFormat  []string `yaml:"work_format"`
		RemoteType  []string `yaml:"remote_type"`
	} `yaml:"queries"`
	Gate struct {
		Bin      string `yaml:"bin"`
		Question string `yaml:"question"`
	} `yaml:"gate"`
	Agent struct {
		WorkDir    string `yaml:"work_dir"`
		ResumeHint string `yaml:"resume_hint"`
	} `yaml:"agent"`
	TG struct {
		Token  string `yaml:"token"`
		ChatID int64  `yaml:"chat_id"`
	} `yaml:"telegram"`
	Rules struct {
		RejectRegex []string `yaml:"reject_regex"` // deterministic pre-gate rejections
	} `yaml:"rules"`
	Hirify struct {
		Key          string `yaml:"key"` // or env HIRIFY_AGENT_KEY; needs agent:apply for buttons
		ProfileID    *int   `yaml:"profile_id"`
		ApplyEnabled bool   `yaml:"apply_enabled"`
	} `yaml:"hirify"`
	MaxAnalyzePerCycle int `yaml:"max_analyze_per_cycle"`
	AnalysisWorkers    int `yaml:"analysis_workers"`
	MaxCardsPerDay     int `yaml:"max_cards_per_day"`
}

type Daemon struct {
	cfg        Config
	hirify     *hirify.Client
	resumeText string
	reject     []*regexp.Regexp
	gate       gate.Gate
	agent      agent.Analyzer
	tg         *tg.Client
	stdout     bool
	st         *store.State

	mu        sync.Mutex
	sentToday int
	day       string
}

// New wires everything from config; stdout=true prints cards instead of
// sending them (dry run).
func New(cfg Config, st *store.State, stdout bool) *Daemon {
	if key := os.Getenv("HIRIFY_AGENT_KEY"); key != "" {
		cfg.Hirify.Key = key
	}
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if home != "" && strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	cfg.Agent.WorkDir = expand(cfg.Agent.WorkDir)
	cfg.Agent.ResumeHint = expand(cfg.Agent.ResumeHint)
	d := &Daemon{cfg: cfg, stdout: stdout, st: st}
	st.BootstrapPending() // upgrade path: queue the pre-queue "yes" backlog
	for _, p := range cfg.Rules.RejectRegex {
		re, err := regexp.Compile(p)
		if err != nil {
			log.Printf("rule %q: %v — skipped", p, err)
			continue
		}
		d.reject = append(d.reject, re)
	}
	if b, err := os.ReadFile(cfg.Agent.ResumeHint); err == nil {
		d.resumeText = string(b)
		log.Printf("resume loaded: %s (%d chars)", cfg.Agent.ResumeHint, len(b))
	} else {
		log.Printf("resume not readable (%v) — анализ будет без контекста резюме", err)
	}
	if cfg.Hirify.Key != "" {
		d.hirify = hirify.New(cfg.Hirify.Key)
	}
	d.gate = gate.Gate{LMBin: cfg.Gate.Bin, Question: cfg.Gate.Question}
	d.agent = agent.Analyzer{Bin: "omp", WorkDir: cfg.Agent.WorkDir, ResumeText: d.resumeText, Timeout: 5 * time.Minute}
	if !stdout {
		d.tg = tg.New(cfg.TG.Token, cfg.TG.ChatID)
	}
	return d
}

// ServeCallbacks runs the TG apply-button loop until ctx is done. Only
// hirify vacancies are appliable through the official API; hh stays URL-only.
func (d *Daemon) ServeCallbacks(ctx context.Context) {
	if d.tg == nil || d.hirify == nil {
		return
	}
	d.tg.Poll(ctx, "apply:", func(cbID, data string) {
		id := strings.TrimPrefix(data, "apply:")
		slug, letter, ok := d.st.Cover(id)
		if !ok {
			d.tg.AnswerCallback(ctx, cbID, "Отклик недоступен (нет письма или уже отправлен)")
			return
		}
		if !d.cfg.Hirify.ApplyEnabled {
			d.tg.AnswerCallback(ctx, cbID, "apply_enabled: false в конфиге")
			return
		}
		if err := d.hirify.Apply(ctx, slug, letter, d.cfg.Hirify.ProfileID); err != nil {
			d.tg.AnswerCallback(ctx, cbID, "Ошибка: "+err.Error())
			log.Printf("apply %s: %v", id, err)
			return
		}
		d.st.MarkApplied(id)
		d.tg.AnswerCallback(ctx, cbID, "✅ Отклик отправлен")
		log.Printf("applied via hirify API: %s", id)
	})
}

// Cycle runs one fetch→gate→analyze→notify pass. Blocking; errors per stage
// are logged, not fatal.
func (d *Daemon) Cycle(ctx context.Context) {
	start := time.Now()
	log.Printf("cycle begin")
	defer func() { log.Printf("cycle end (%s)", time.Since(start).Round(time.Second)) }()
	d.countReset()
	var candidates []model.Vacancy
	for _, q := range d.cfg.Queries {
		var (
			vs  []model.Vacancy
			err error
		)
		switch q.Source {
		case "hirify":
			if d.hirify == nil {
				log.Printf("hirify key not configured — skipping hirify query %q", q.Text)
				continue
			}
			pages := q.HirifyPages
			if pages == 0 {
				pages = 2
			}
			vs, err = d.searchHirify(ctx, q, pages)
		case "hirify-feed":
			if d.hirify == nil {
				log.Printf("hirify key not configured — skipping feed %d", q.FeedID)
				continue
			}
			vs, err = d.hirify.FeedVacancies(ctx, q.FeedID, 1, 50)
		default:
			vs, err = hh.Search(ctx, hh.SearchURL(q.Text, q.AreaID, q.RemoteOnly))
		}
		if err != nil {
			log.Printf("query %s/%q: %v", q.Source, q.Text, err)
			continue
		}
		for _, v := range vs {
			if d.st.FirstSeen(v.ID) {
				candidates = append(candidates, v)
			}
		}
	}
	if len(candidates) > 0 {
		log.Printf("new candidates: %d", len(candidates))
	}

	byID := map[string]model.Vacancy{}
	for _, v := range candidates {
		byID[v.ID] = v
	}

	// process(): schedule one vacancy for analysis within this cycle's quota
	analyzed := 0
	var jobs []model.Vacancy
	process := func(v model.Vacancy) bool {
		if ctx.Err() != nil || analyzed >= d.cfg.MaxAnalyzePerCycle {
			return false
		}
		if !d.budgetLeft() {
			log.Printf("daily card budget spent (%d)", d.cfg.MaxCardsPerDay)
			return false
		}
		analyzed++
		jobs = append(jobs, v)
		return true
	}

	// pass 1: gate every new candidate; classify
	var pendingIDs []string // "yes"/uncertain without an analysis slot this cycle
	for _, v := range candidates {
		if ctx.Err() != nil {
			return
		}
		if why, blocked := d.rejectMatch(v); blocked {
			d.st.Mark(v.ID, "rule-отклонено")
			log.Printf("rules: skip %s (%s): %s", v.ID, v.Title, why)
			continue
		}
		verdict, err := d.gate.Decide(ctx, v)
		d.st.Mark(v.ID, verdict.Answer)
		if err != nil {
			log.Printf("gate %s: %v", v.ID, err)
		}
		switch verdict.Answer {
		case "нет":
			continue
		case "да", "сомнительно", "ungated":
			if !process(v) {
				pendingIDs = append(pendingIDs, v.ID)
			}
		}
	}

	log.Printf("pending queue: %d", len(d.st.Pending))
	// pass 2: backlog from previous cycles — oldest first, still in quota.
	// Ids absent from this fetch are dropped: Seen keeps them marked, so no
	// re-carding; their text is refetchable only via a search hit anyway.
	for _, id := range d.st.PopPending(d.cfg.MaxAnalyzePerCycle) {
		if v, ok := byID[id]; ok {
			process(v)
			continue
		}
		// not in this fetch: analyzeAndNotify refetches the full text by ID
		process(model.Vacancy{Source: "hirify", ID: id})
	}

	// parallel analyses: each is one plain model completion now
	workers := d.cfg.AnalysisWorkers
	if workers < 1 {
		workers = 1
	}
	jch := make(chan model.Vacancy)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for v := range jch {
				d.analyzeAndNotify(ctx, v)
			}
		}()
	}
	for _, v := range jobs {
		jch <- v
	}
	close(jch)
	wg.Wait()
	if err := d.st.Save(); err != nil {
		log.Printf("state save: %v", err)
	}
}

// searchHirify walks up to pages pages of the official Agent API search.
func (d *Daemon) searchHirify(ctx context.Context, q struct {
	Source      string   `yaml:"source"`
	Text        string   `yaml:"text"`
	AreaID      int      `yaml:"area_id"`
	RemoteOnly  bool     `yaml:"remote_only"`
	HirifyPages int      `yaml:"hirify_pages"`
	FeedID      int      `yaml:"feed_id"` // source: hirify-feed — site-saved filter
	Grade       []string `yaml:"grade"`
	WorkFormat  []string `yaml:"work_format"`
	RemoteType  []string `yaml:"remote_type"`
}, pages int,
) ([]model.Vacancy, error) {
	f := hirify.Filters{Search: q.Text, Params: []string{"title", "company"}, Grade: q.Grade, WorkFormat: q.WorkFormat, RemoteType: q.RemoteType}
	var out []model.Vacancy
	for p := 1; p <= pages; p++ {
		vs, err := d.hirify.Search(ctx, f, p, 50)
		if err != nil {
			return out, err
		}
		out = append(out, vs...)
		if len(vs) < 50 {
			break
		}
	}
	return out, nil
}

func (d *Daemon) analyzeAndNotify(ctx context.Context, v model.Vacancy) {
	vacText := v.Compact()
	if v.Source == "hirify" {
		if idStr := strings.TrimPrefix(v.ID, "hirify:"); idStr != "" {
			if id, err := strconv.Atoi(idStr); err == nil {
				if fv, err := d.hirify.FullVacancy(ctx, id); err == nil {
					vacText = fv.Render()
					if v.Slug == "" {
						v.Slug = fv.Slug
					}
					if v.Title == "" {
						v.Title = fv.Title
					}
				} else {
					log.Printf("full vacancy %s: %v (fallback: карточка)", v.ID, err)
				}
			}
		}
	}

	a, err := d.agent.AnalyzeText(ctx, vacText)
	if err != nil {
		log.Printf("agent %s: %v", v.ID, err)
		// notify without analysis rather than silently dropping
		a = agent.Analysis{Verdict: "unknown"}
	}
	switch a.Verdict {
	case "пропускать":
		d.st.Mark(v.ID, "agent-пропускать")
		log.Printf("agent skips %s (%s)", v.ID, v.Title)
		return
	case "откликаться":
		d.st.Mark(v.ID, "agent-откликаться")
	}
	if a.Letter != "" && v.Source == "hirify" && v.Slug != "" {
		d.st.SetCover(v.ID, v.Slug, a.Letter)
	}
	d.send(v, a)
}

// rejectMatch applies deterministic pre-gate rules; returns the matched
// pattern when the vacancy must be skipped without any model calls.
func (d *Daemon) rejectMatch(v model.Vacancy) (string, bool) {
	if len(d.reject) == 0 {
		return "", false
	}
	haystack := strings.Join([]string{v.Title, v.Employer, v.Compensation, v.Extra}, " \n")
	for _, re := range d.reject {
		if re.MatchString(haystack) {
			return re.String(), true
		}
	}
	return "", false
}

func (d *Daemon) send(v model.Vacancy, a agent.Analysis) {
	text := fmt.Sprintf("🔧 %s\n%s", v.Title, v.Compact())
	if a.Traps != "" {
		text += "\n\n⚠️ Ловушки:\n" + a.Traps
	}
	if a.Advice != "" {
		text += "\n\n💡 " + a.Advice
	}
	if a.Letter != "" {
		text += "\n\n✉️ " + a.Letter
	}
	if a.Verdict == "unknown" {
		text += "\n\n(агент-разбор не удался — только карточка)"
	}
	label := "Открыть вакансию"
	switch v.Source {
	case "hh":
		label = "Открыть на hh.ru"
	case "hirify":
		label = "Открыть на hirify"
	}
	if !d.stdout {
		_, canApply := "", false
		if _, letter, ok := d.st.Cover(v.ID); ok && letter != "" && d.cfg.Hirify.ApplyEnabled {
			canApply = true
		}
		var err error
		if canApply {
			err = d.tg.CardWithApply(context.Background(), text, "✅ Откликнуться (hirify)", "apply:"+v.ID, label, v.URL)
		} else {
			err = d.tg.Card(context.Background(), text, label, v.URL)
		}
		if err != nil {
			log.Printf("telegram: %v", err)
			return
		}
	} else {
		hint := ""
		if _, _, ok := d.st.Cover(v.ID); ok {
			hint = " [кнопка отклика активна]"
		}
		fmt.Printf("=== CARD ===\n%s\n%s%s\n", text, v.URL, hint)
	}
	d.countCard()
}

func (d *Daemon) budgetLeft() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.countReset()
	return d.sentToday < d.cfg.MaxCardsPerDay
}

func (d *Daemon) countCard() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.countReset()
	d.sentToday++
}

func (d *Daemon) countReset() {
	today := time.Now().Format("2006-01-02")
	if d.day != today {
		d.day = today
		d.sentToday = 0
	}
}
