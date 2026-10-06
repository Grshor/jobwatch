// Package model: the vacancy type shared by all sources, the gate, the
// agent, and the notifier.
package model

import "strings"

// Vacancy is one source-normalized job card. ID is source-prefixed
// ("hh:123", "hirify:456") — dedup happens over it.
type Vacancy struct {
	Source       string `json:"source"` // hh | hirify
	ID           string `json:"id"`
	Title        string `json:"title"`
	Employer     string `json:"employer,omitempty"`
	Compensation string `json:"compensation,omitempty"`
	Address      string `json:"address,omitempty"`
	Experience   string `json:"experience,omitempty"`
	Remote       bool   `json:"remote,omitempty"`
	URL          string `json:"url"`
	// Slug: source-native identifier for API actions (hirify apply).
	Slug string `json:"slug,omitempty"`
	// Extra: source-specific context the gate should see (company type,
	// stack, scam flags) — rendered by Compact.
	Extra string `json:"extra,omitempty"`
}

// Compact renders the card as one plain-text line for prompts.
func (v Vacancy) Compact() string {
	parts := []string{"[" + v.Source + "] " + v.Title}
	if v.Employer != "" {
		parts = append(parts, "работодатель: "+v.Employer)
	}
	if v.Compensation != "" {
		parts = append(parts, "зарплата: "+v.Compensation)
	}
	if v.Experience != "" {
		parts = append(parts, "опыт: "+v.Experience)
	}
	if v.Address != "" {
		parts = append(parts, v.Address)
	}
	if v.Remote {
		parts = append(parts, "удалёнка")
	}
	if v.Extra != "" {
		parts = append(parts, v.Extra)
	}
	return strings.Join(parts, "; ")
}
