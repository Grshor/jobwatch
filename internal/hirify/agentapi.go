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

type card struct {
	ID           int      `json:"id"`
	Slug         string   `json:"slug"`
	Title        string   `json:"title"`
	CompanyTitle string   `json:"company_title"`
	CompanyType  string   `json:"company_type"`
	MainStack    []string `json:"main_stack"`
	WorkFormat   []string `json:"work_format"`
	Grades       []struct {
		Name string `json:"name"`
	} `json:"grades"`
	EnglishLevel string `json:"english_level"`
	TLDR         string `json:"tldr"`
	Salary       *struct {
		Currency    string `json:"currency"`
		Min         *int   `json:"min"`
		Max         *int   `json:"max"`
		SalaryInUSD *int   `json:"salary_in_usd"`
	} `json:"salary"`
	IsScam          bool   `json:"is_scam"`
	IsPotentialScam bool   `json:"is_potential_scam"`
	CreatedAt       string `json:"created_at"`
}

func (v card) toModel() model.Vacancy {
	m := model.Vacancy{
		Source:   "hirify",
		ID:       fmt.Sprintf("hirify:%d", v.ID),
		Slug:     v.Slug,
		Title:    v.Title,
		Employer: v.CompanyTitle,
		URL:      "https://hirify.me/jobs/" + v.Slug,
	}
	// the anonymous-adapter extras survive: the gate keeps its eyes
	m.Extra = extras(v)
	return m
}

func (c *Client) Search(ctx context.Context, f Filters, page, perPage int) ([]model.Vacancy, error) {
	filtersJSON, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	var data []card
	// query-object serialization: Laravel accepts filters as JSON string
	path := fmt.Sprintf("/api/agent/vacancies?filters=%s&page=%d&per_page=%d",
		url.QueryEscape(string(filtersJSON)), page, perPage)
	if err := c.get(ctx, path, &data); err != nil {
		return nil, err
	}
	out := make([]model.Vacancy, 0, len(data))
	for _, v := range data {
		out = append(out, v.toModel())
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

func extras(v card) string {
	var extra []string
	if v.CompanyType != "" {
		extra = append(extra, "тип компании: "+humanCompany(v.CompanyType))
	}
	if len(v.MainStack) > 0 {
		extra = append(extra, "стек: "+strings.Join(v.MainStack, ", "))
	}
	if v.EnglishLevel != "" {
		extra = append(extra, "английский: "+v.EnglishLevel)
	}
	switch {
	case v.IsScam:
		extra = append(extra, "⚠️HIRIFY-ФЛАГ: SCAM")
	case v.IsPotentialScam:
		extra = append(extra, "⚠️hirify: потенциальный скам")
	}
	if strings.TrimSpace(v.TLDR) != "" {
		t := v.TLDR
		if len(t) > 200 {
			t = t[:200] + "…"
		}
		extra = append(extra, "кратко: "+t)
	}
	return strings.Join(extra, "; ")
}

func humanCompany(t string) string {
	switch t {
	case "product_company":
		return "продукт"
	case "agency", "outsourcing", "outstaff":
		return "аутстафф/агентство ⚠️"
	}
	return t
}
