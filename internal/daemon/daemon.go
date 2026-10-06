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
	Hirify struct {
		Key          string `yaml:"key"` // or env HIRIFY_AGENT_KEY; needs agent:apply for buttons
		ProfileID    *int   `yaml:"profile_id"`
		ApplyEnabled bool   `yaml:"apply_enabled"`
	} `yaml:"hirify"`
	MaxAnalyzePerCycle int `yaml:"max_analyze_per_cycle"`
	MaxCardsPerDay     int `yaml:"max_cards_per_day"`
}

type Daemon struct {
	cfg    Config
	hirify *hirify.Client
	gate   gate.Gate
	agent  agent.Analyzer
	tg     *tg.Client
	stdout bool
	st     *store.State

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
	d := &Daemon{cfg: cfg, stdout: stdout, st: st}
	if cfg.Hirify.Key != "" {
		d.hirify = hirify.New(cfg.Hirify.Key)
	}
	d.gate = gate.Gate{LMBin: cfg.Gate.Bin, Question: cfg.Gate.Question}
	d.agent = agent.Analyzer{Bin: "omp", WorkDir: cfg.Agent.WorkDir, ResumeHint: cfg.Agent.ResumeHint}
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
	if len(candidates) == 0 {
		return
	}
	log.Printf("new candidates: %d", len(candidates))

	analyzed := 0
	for _, v := range candidates {
		if ctx.Err() != nil {
			return
		}
		verdict, err := d.gate.Decide(ctx, v)
		d.st.Mark(v.ID, verdict.Answer)
		if err != nil {
			log.Printf("gate %s: %v", v.ID, err)
		}
		switch verdict.Answer {
		case "нет":
			continue
		case "сомнительно":
			// analyze only if quota remains at the end — handled by fallthrough order
		}
		if verdict.Answer != "да" && verdict.Answer != "сомнительно" && verdict.Answer != "ungated" {
			continue
		}
		if analyzed >= d.cfg.MaxAnalyzePerCycle {
			continue // stays seen; re-surface logic is a v0.2 concern
		}
		if !d.budgetLeft() {
			log.Printf("daily card budget spent (%d)", d.cfg.MaxCardsPerDay)
			return
		}
		analyzed++
		d.analyzeAndNotify(ctx, v)
	}
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
	Grade       []string `yaml:"grade"`
	WorkFormat  []string `yaml:"work_format"`
	RemoteType  []string `yaml:"remote_type"`
}, pages int) ([]model.Vacancy, error) {
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
	a, err := d.agent.Analyze(ctx, v)
	if err != nil {
		log.Printf("agent %s: %v", v.ID, err)
		// notify without analysis rather than silently dropping
		a = agent.Analysis{Verdict: "unknown"}
	}
	if a.Verdict == "пропускать" {
		d.st.Mark(v.ID, "agent-пропускать")
		log.Printf("agent skips %s (%s)", v.ID, v.Title)
		return
	}
	if a.Letter != "" && v.Source == "hirify" && v.Slug != "" {
		d.st.SetCover(v.ID, v.Slug, a.Letter)
	}
	d.send(v, a)
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
