// Package gate: the cheap local relevance filter. Delegates to `lm decide`
// (decider-4b, Jev-class) — ~0.1s and zero cloud per vacancy. When the
// decider is down the gate opens with a "ungated" verdict: the pipeline
// degrades to more agent calls, never to silence.
package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Grshor/jobwatch/internal/model"
)

// Verdict of one gate decision.
type Verdict struct {
	Answer string  `json:"answer"` // да | нет | сомнительно | ungated
	Prob   float64 `json:"prob"`   // probability of the answer
}

type decideOut struct {
	Answer        string             `json:"answer"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Gate wraps one localmix decider CLI.
type Gate struct {
	LMBin     string // e.g. "lm"
	Question  string // fixed criteria question from config
	StateTmpl string // optional wrapper; vacancy Compact() is appended
}

// Decide asks the local decider about one vacancy.
func (g Gate) Decide(ctx context.Context, v model.Vacancy) (Verdict, error) {
	cmd := exec.CommandContext(ctx, g.LMBin, "decide",
		"--question", g.Question,
		"--options", "да,нет,сомнительно",
		"--json")
	cmd.Stdin = strings.NewReader(v.Compact())
	var out decideOut
	body, err := cmd.Output()
	if err != nil {
		return Verdict{Answer: "ungated"}, fmt.Errorf("lm decide: %w", err)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Verdict{Answer: "ungated"}, fmt.Errorf("lm decide output: %w (%q)", err, truncate(string(body)))
	}
	return Verdict{Answer: out.Answer, Prob: out.Probabilities[out.Answer]}, nil
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
