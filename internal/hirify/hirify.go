// Package hirify: client for hirify.me's live-search JSON API. Discovered
// by sniffing the site's own XHR: the SPA calls
// api.hirify.me/api/vacancies?params=title,company&search=Q&page=N
// anonymously (Laravel paginator). The payload is richer than the cards:
// company_type, grades, salary with USD conversion, tldr — and hirify's own
// scam flags, which the gate gets to see.
package hirify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Grshor/jobwatch/internal/model"
)

type Client struct {
	BaseURL string
	hc      *http.Client
}

func New() *Client {
	return &Client{BaseURL: "https://api.hirify.me", hc: &http.Client{Timeout: 30 * time.Second}}
}

// Search fetches up to maxPages pages of results for a free-text query.
func (c *Client) Search(ctx context.Context, query string, maxPages int) ([]model.Vacancy, error) {
	var out []model.Vacancy
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("params", "title,company")
		q.Set("search", query)
		q.Set("page", fmt.Sprint(page))
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.BaseURL+"/api/vacancies?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) Chrome/126.0 Safari/537.36")
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("hirify fetch: %w", err)
		}
		var body struct {
			Data     []apiVacancy `json:"data"`
			LastPage int          `json:"last_page"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("hirify decode: %w", err)
		}
		for _, v := range body.Data {
			out = append(out, v.toModel())
		}
		if page >= body.LastPage {
			break
		}
	}
	return out, nil
}

type apiVacancy struct {
	ID            int      `json:"id"`
	Slug          string   `json:"slug"`
	Title         string   `json:"title"`
	OriginalTitle string   `json:"original_title"`
	CompanyTitle  string   `json:"company_title"`
	CompanyType   string   `json:"company_type"`
	MainStack     []string `json:"main_stack"`
	WorkFormat    []string `json:"work_format"`
	Grades        []struct {
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

func (v apiVacancy) toModel() model.Vacancy {
	m := model.Vacancy{
		Source:   "hirify",
		ID:       fmt.Sprintf("hirify:%d", v.ID),
		Title:    firstNonEmpty(v.OriginalTitle, v.Title),
		Employer: v.CompanyTitle,
		URL:      "https://hirify.me/jobs/" + v.Slug,
	}
	for _, f := range v.WorkFormat {
		if f == "remote" {
			m.Remote = true
			break
		}
	}
	if len(v.Grades) > 0 {
		names := make([]string, len(v.Grades))
		for i, g := range v.Grades {
			names[i] = g.Name
		}
		m.Experience = strings.Join(names, "/")
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
	m.Extra = strings.Join(extra, "; ")
	return m
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

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
