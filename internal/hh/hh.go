// Package hh: scraper for hh.ru's server-rendered vacancy search page.
// The official API (api.hh.ru) is IP-gated for this network (403 for every
// endpoint), while the web search page renders real cards with stable
// data-qa attributes — that is the contract this parser relies on. It will
// break when hh redesigns; the failure mode is "zero vacancies parsed",
// never wrong data.
package hh

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Grshor/jobwatch/internal/model"
	"golang.org/x/net/html"
)

// Vacancy is one parsed search-result card.
type Vacancy struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Employer     string `json:"employer"`
	Compensation string `json:"compensation,omitempty"`
	Address      string `json:"address,omitempty"`
	Experience   string `json:"experience,omitempty"`
	Remote       bool   `json:"remote,omitempty"`
	URL          string `json:"url"`
}

const chromeUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// SearchURL builds the hh.ru search page URL from query parameters.
func SearchURL(text string, areaID int, remoteOnly bool) string {
	q := url.Values{}
	q.Set("text", text)
	q.Set("area", fmt.Sprint(areaID))
	q.Set("experience", "between_3_and_6_years")
	q.Set("order_by", "publication")
	q.Set("items_per_page", "50")
	if remoteOnly {
		q.Set("label", "only_remote")
	}
	return "https://hh.ru/search/vacancy?" + q.Encode()
}

// Search fetches and parses one search page.
func Search(ctx context.Context, pageURL string) ([]model.Vacancy, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("hh fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("hh fetch: status %d", resp.StatusCode)
	}
	root, err := html.Parse(resp.Body)
	if err != nil {
		return nil, err
	}
	return parse(root), nil
}

// parse walks the DOM for data-qa="vacancy-serp__vacancy" containers.
func parse(root *html.Node) []model.Vacancy {
	var out []model.Vacancy
	seen := map[string]bool{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "article" && attr(n, "data-qa") == "vacancy-serp__vacancy" {
			if v, ok := parseCard(n); ok && !seen[v.ID] {
				seen[v.ID] = true
				out = append(out, v)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(root)
	return out
}

func parseCard(card *html.Node) (model.Vacancy, bool) {
	var v model.Vacancy
	v.Source = "hh"
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		qa := attr(n, "data-qa")
		switch {
		case n.Data == "a" && qa == "vacancy-serp__vacancy-title":
			if v.URL == "" {
				href := attr(n, "href")
				v.URL = absolute(href)
				v.ID = "hh:" + idFrom(href)
			}
			if v.Title == "" {
				v.Title = strings.TrimSpace(text(n))
			}
		case n.Data == "a" && v.URL == "" && strings.Contains(attr(n, "href"), "/vacancy/"):
			href := attr(n, "href")
			v.URL = absolute(href)
			v.ID = "hh:" + idFrom(href)
			if v.Title == "" {
				v.Title = strings.TrimSpace(text(n))
			}
		case qa == "vacancy-serp__vacancy-employer-text":
			v.Employer = strings.TrimSpace(text(n))
		case qa == "vacancy-serp__vacancy-compensation" || strings.HasPrefix(qa, "vacancy-serp__vacancy-compensation-frequency"):
			if v.Compensation == "" {
				v.Compensation = strings.TrimSpace(text(n))
			}
		case qa == "vacancy-serp__vacancy-address":
			v.Address = strings.TrimSpace(text(n))
		case strings.HasPrefix(qa, "vacancy-serp__vacancy-work-experience-"):
			v.Experience = expLabel(qa)
		case qa == "vacancy-label-work-schedule-remote":
			v.Remote = true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(card)
	if v.ID == "hh:" || v.Title == "" {
		return model.Vacancy{}, false
	}
	return v, true
}

func expLabel(qa string) string {
	switch {
	case strings.Contains(qa, "between1And3"):
		return "1–3 года"
	case strings.Contains(qa, "between3And6"):
		return "3–6 лет"
	case strings.Contains(qa, "moreThan6"):
		return "6+ лет"
	case strings.Contains(qa, "noExperience"):
		return "без опыта"
	}
	return ""
}

func absolute(href string) string {
	if strings.HasPrefix(href, "http") {
		return href
	}
	return "https://hh.ru" + href
}

func idFrom(href string) string {
	i := strings.Index(href, "/vacancy/")
	if i < 0 {
		return ""
	}
	rest := href[i+len("/vacancy/"):]
	if j := strings.IndexAny(rest, "?&#"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func text(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
