// Package hirify: official Agent API client (api.hirify.me/docs,
// OpenAPI 3.1). Bearer key from hirify.me/account/api-access — the same key
// powers their REST, MCP, CLI, and webhooks. Apply requires the
// "agent:apply" ability on the key and costs one application unit.
package hirify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Grshor/jobwatch/internal/model"
)

// Filters mirror the site's own search filters (hirify.me/blog/filter-guide).
type Filters struct {
	Search     string   `json:"search,omitempty"`
	Params     []string `json:"params,omitempty"`  // where to search: title, company
	SortBy     string   `json:"sort_by,omitempty"` // e.g. newest
	Period     string   `json:"period,omitempty"`
	Grade      []string `json:"grade,omitempty"`       // junior, middle, senior, lead
	WorkFormat []string `json:"work_format,omitempty"` // remote, hybrid, onsite
	RemoteType []string `json:"remote_type,omitempty"` // global, russia …
}

type Client struct {
	Key     string
	BaseURL string
	hc      *http.Client
}

func New(key string) *Client {
	return &Client{Key: key, BaseURL: "https://api.hirify.me", hc: &http.Client{Timeout: 30 * time.Second}}
}

// grades accepts both shapes hirify serves: ["senior"] (Agent API) and
// [{"id":3,"name":"senior"}] (anonymous endpoint).
type grades []struct{ Name string }

func (g *grades) UnmarshalJSON(b []byte) error {
	var elems []json.RawMessage
	if err := json.Unmarshal(b, &elems); err != nil {
		return err
	}
	for _, e := range elems {
		var name string
		if len(e) > 0 && e[0] == '"' {
			if err := json.Unmarshal(e, &name); err != nil {
				return err
			}
		} else {
			var o struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(e, &o); err != nil {
				return err
			}
			name = o.Name
		}
		*g = append(*g, struct{ Name string }{name})
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("hirify %s %s: %w (http %d)", method, path, err, resp.StatusCode)
	}
	if !envelope.OK {
		msg := "unknown error"
		if envelope.Error != nil {
			msg = envelope.Error.Message
		}
		return fmt.Errorf("hirify %s %s: %s (http %d)", method, path, msg, resp.StatusCode)
	}
	if out == nil || len(envelope.Data) == 0 {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}

// Me returns account identity, plan, and remaining daily quota.
func (c *Client) Me(ctx context.Context) (json.RawMessage, error) {
	var me json.RawMessage
	err := c.get(ctx, "/api/agent/me", &me)
	return me, err
}

// card is the Agent API's compact vacancy card. Note: the company title is
// deliberately absent (company_masked) — anti-doxxing; the agent reads it
// from the vacancy page.
type card struct {
	VacancyID    int      `json:"vacancy_id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	Grades       grades   `json:"grades"`
	EnglishLevel string   `json:"english_level"`
	RemoteType   string   `json:"remote_type"`
	WorkFormat   []string `json:"work_format"`
	Salary       *struct {
		Currency    string `json:"currency"`
		Min         *int   `json:"min"`
		Max         *int   `json:"max"`
		SalaryInUSD *int   `json:"salary_in_usd"`
	} `json:"salary"`
	Skills           []string `json:"skills"`
	Cities           []string `json:"cities"`
	CanApplyDirectly bool     `json:"can_apply_directly"`
	HasApplied       bool     `json:"has_applied"`
	CompanyMasked    bool     `json:"company_masked"`
}

func (c *Client) search(ctx context.Context, f Filters, page, perPage int) ([]card, error) {
	q := url.Values{}
	if f.Search != "" {
		q.Set("search", f.Search)
	}
	if len(f.Params) > 0 {
		q.Set("params", strings.Join(f.Params, ","))
	}
	if f.SortBy != "" {
		q.Set("sort_by", f.SortBy)
	}
	if f.Period != "" {
		q.Set("period", f.Period)
	}
	if len(f.Grade) > 0 {
		q.Set("grade", strings.Join(f.Grade, ","))
	}
	if len(f.WorkFormat) > 0 {
		q.Set("work_format", strings.Join(f.WorkFormat, ","))
	}
	if len(f.RemoteType) > 0 {
		q.Set("remote_type", strings.Join(f.RemoteType, ","))
	}
	q.Set("page", fmt.Sprint(page))
	q.Set("per_page", fmt.Sprint(perPage))

	var data []card
	if err := c.get(ctx, "/api/agent/vacancies?"+q.Encode(), &data); err != nil {
		return nil, err
	}
	return data, nil
}

// Search walks up to pages pages of Agent API results.
func (c *Client) Search(ctx context.Context, f Filters, pages, perPage int) ([]model.Vacancy, error) {
	var out []model.Vacancy
	for p := 1; p <= pages; p++ {
		cards, err := c.search(ctx, f, p, perPage)
		if err != nil {
			return out, err
		}
		for _, v := range cards {
			out = append(out, v.toModel())
		}
		if len(cards) < perPage {
			break
		}
	}
	return out, nil
}

// Full fetches one vacancy in full (counts toward the daily request limit).
func (c *Client) Full(ctx context.Context, vacancyID int) (json.RawMessage, error) {
	var full json.RawMessage
	err := c.get(ctx, fmt.Sprintf("/api/agent/vacancies/%d", vacancyID), &full)
	return full, err
}

// Apply responds to a vacancy for the account holder. Requires the
// agent:apply ability; costs one application unit.
func (c *Client) Apply(ctx context.Context, slug, coverLetter string, profileID *int) error {
	body := map[string]any{"cover_letter": coverLetter}
	if profileID != nil {
		body["profile_id"] = *profileID
	}
	return c.post(ctx, "/api/agent/vacancies/"+slug+"/apply", body, nil)
}

// FeedVacancies reads a saved search's matches (initial snapshot / recovery).
func (c *Client) FeedVacancies(ctx context.Context, feedID, page, perPage int) ([]model.Vacancy, error) {
	var data []card
	path := fmt.Sprintf("/api/agent/feeds/%d/vacancies?page=%d&per_page=%d", feedID, page, perPage)
	if err := c.get(ctx, path, &data); err != nil {
		return nil, err
	}
	out := make([]model.Vacancy, 0, len(data))
	for _, v := range data {
		out = append(out, v.toModel())
	}
	return out, nil
}

func gradeNames(g grades) []string {
	names := make([]string, len(g))
	for i, x := range g {
		names[i] = x.Name
	}
	return names
}

func (v card) toModel() model.Vacancy {
	m := model.Vacancy{
		Source:     "hirify",
		ID:         fmt.Sprintf("hirify:%d", v.VacancyID),
		Slug:       v.Slug,
		Title:      v.Title,
		URL:        "https://hirify.me/jobs/" + v.Slug,
		Experience: strings.Join(gradeNames(v.Grades), "/"),
	}
	for _, f := range v.WorkFormat {
		if f == "remote" {
			m.Remote = true
			break
		}
	}
	if v.Salary != nil {
		s := v.Salary
		switch {
		case s.Min != nil && s.Max != nil:
			m.Compensation = fmt.Sprintf("%d–%d %s", *s.Min, *s.Max, s.Currency)
		case s.Min != nil:
			m.Compensation = fmt.Sprintf("от %d %s", *s.Min, s.Currency)
		case s.Max != nil:
			m.Compensation = fmt.Sprintf("до %d %s", *s.Max, s.Currency)
		}
		if s.SalaryInUSD != nil {
			m.Compensation += fmt.Sprintf(" (~$%d)", *s.SalaryInUSD)
		}
	}
	var extra []string
	if len(v.Skills) > 0 {
		s := v.Skills
		if len(s) > 8 {
			s = s[:8]
		}
		extra = append(extra, "навыки: "+strings.Join(s, ", "))
	}
	if v.EnglishLevel != "" {
		extra = append(extra, "английский: "+v.EnglishLevel)
	}
	if v.RemoteType != "" {
		extra = append(extra, "remote: "+v.RemoteType)
	}
	switch {
	case v.HasApplied:
		extra = append(extra, "уже откликался")
	case !v.CanApplyDirectly:
		extra = append(extra, "прямой отклик недоступен")
	}
	m.Extra = strings.Join(extra, "; ")
	return m
}

// FullVacancy is the full vacancy payload from GET /vacancies/{id}.
type FullVacancy struct {
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	Company string `json:"company"`
	Salary  *struct {
		Currency    string `json:"currency"`
		Min         *int   `json:"min"`
		Max         *int   `json:"max"`
		SalaryInUSD *int   `json:"salary_in_usd"`
	} `json:"salary"`
	Grades       grades   `json:"grades"`
	EnglishLevel string   `json:"english_level"`
	WorkFormat   []string `json:"work_format"`
	WorkType     string   `json:"work_type"`
	Skills       []string `json:"skills"`
	Description  string   `json:"description"`
	Regions      []struct {
		Name string `json:"name"`
	} `json:"regions"`
}

// FullVacancy fetches the full card (counts toward the daily request limit).
func (c *Client) FullVacancy(ctx context.Context, vacancyID int) (*FullVacancy, error) {
	var f FullVacancy
	if err := c.get(ctx, fmt.Sprintf("/api/agent/vacancies/%d", vacancyID), &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Render renders the full vacancy as plain text for the analysis prompt.
func (f *FullVacancy) Render() string {
	var b strings.Builder
	b.WriteString(f.Title)
	if f.Company != "" {
		b.WriteString("\nКомпания: " + f.Company)
	}
	if len(f.Grades) > 0 {
		b.WriteString("\nГрейды: " + strings.Join(gradeNames(f.Grades), "/"))
	}
	if f.EnglishLevel != "" {
		b.WriteString("\nАнглийский: " + f.EnglishLevel)
	}
	if len(f.WorkFormat) > 0 {
		b.WriteString("\nФормат: " + strings.Join(f.WorkFormat, ", "))
	}
	if f.WorkType != "" {
		b.WriteString("\nЗанятость: " + f.WorkType)
	}
	if len(f.Skills) > 0 {
		b.WriteString("\nНавыки: " + strings.Join(f.Skills, ", "))
	}
	for _, r := range f.Regions {
		b.WriteString("\nРегион: " + r.Name)
	}
	if f.Salary != nil {
		s := f.Salary
		switch {
		case s.Min != nil && s.Max != nil:
			b.WriteString(fmt.Sprintf("\nЗарплата: %d–%d %s", *s.Min, *s.Max, s.Currency))
		case s.Min != nil:
			b.WriteString(fmt.Sprintf("\nЗарплата: от %d %s", *s.Min, s.Currency))
		}
	}
	b.WriteString("\n\n" + f.Description)
	return b.String()
}

// Feeds lists the account's saved searches (hirify.me filters).
func (c *Client) Feeds(ctx context.Context) (json.RawMessage, error) {
	var feeds json.RawMessage
	err := c.get(ctx, "/api/agent/feeds", &feeds)
	return feeds, err
}
