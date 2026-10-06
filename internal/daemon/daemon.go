// Package daemon: the orchestration loop — fetch, dedup, gate, analyze,
// notify. Progressive autonomy in code: the analysis quota is small, cards
// go out with a URL button, and the caps keep the pipeline from ever
// spamming the user or the boards.
package daemon

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/Grshor/jobwatch/internal/agent"
	"github.com/Grshor/jobwatch/internal/gate"
	"github.com/Grshor/jobwatch/internal/hh"
	"github.com/Grshor/jobwatch/internal/store"
	"github.com/Grshor/jobwatch/internal/tg"
)

// Config mirrors config.yml.
type Config struct {
	Interval  time.Duration `yaml:"interval"`
	StatePath string        `yaml:"state_path"`
	Queries   []struct {
		Text       string `yaml:"text"`
		AreaID     int    `yaml:"area_id"`
		RemoteOnly bool   `yaml:"remote_only"`
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
	MaxAnalyzePerCycle int `yaml:"max_analyze_per_cycle"`
	MaxCardsPerDay     int `yaml:"max_cards_per_day"`
}

type Daemon struct {
	cfg    Config
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
	d := &Daemon{cfg: cfg, stdout: stdout, st: st}
	d.gate = gate.Gate{LMBin: cfg.Gate.Bin, Question: cfg.Gate.Question}
	d.agent = agent.Analyzer{Bin: "omp", WorkDir: cfg.Agent.WorkDir, ResumeHint: cfg.Agent.ResumeHint}
	if !stdout {
		d.tg = tg.New(cfg.TG.Token, cfg.TG.ChatID)
	}
	return d
}

// Cycle runs one fetch→gate→analyze→notify pass. Blocking; errors per stage
// are logged, not fatal.
func (d *Daemon) Cycle(ctx context.Context) {
	d.countReset()
	var candidates []hh.Vacancy
	for _, q := range d.cfg.Queries {
		vs, err := hh.Search(ctx, hh.SearchURL(q.Text, q.AreaID, q.RemoteOnly))
		if err != nil {
			log.Printf("query %q: %v", q.Text, err)
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

func (d *Daemon) analyzeAndNotify(ctx context.Context, v hh.Vacancy) {
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
	d.send(v, a)
}

func (d *Daemon) send(v hh.Vacancy, a agent.Analysis) {
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
	label := "Открыть на hh.ru"
	if !d.stdout {
		if err := d.tg.Card(context.Background(), text, label, v.URL); err != nil {
			log.Printf("telegram: %v", err)
			return
		}
	} else {
		fmt.Printf("=== CARD ===\n%s\n%s\n", text, v.URL)
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
