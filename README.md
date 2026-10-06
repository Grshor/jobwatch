# jobwatch

A personal job-feed daemon that does the part of job hunting nobody wants to
do: watching, filtering, and preparing applications. You press one button.

```
hh.ru search pages ──▶ dedup ──▶ local decider gate ──▶ OMP agent ──▶ Telegram card
(hh official API is            (decider-4b on           (reads the vacancy     (traps, advice,
 IP-gated → SSR scrape)         localhost, ~0.1s,        + your real resume,    tailored cover
                                zero cloud)              spots the traps)       letter, URL button)
```

The agent layer is the interesting part. For each vacancy that survives the
cheap gate, a headless `omp --no-session -p` run opens the vacancy page,
reads the candidate's actual resume files, and answers in a strict format:

- **Traps** — screening filters ("6+ years" against your real 5), salary
  below ask hidden behind "competitive compensation", staffing agencies
  posing as product companies, iGaming euphemisms ("entertainment platform",
  "European holding", Cyprus relocation), curated review scores, frozen
  benefits during the probation period;
- **Advice** — which resume bullets to lead with for this specific posting;
- **Cover letter** — 4–6 lines built from the candidate's real metrics, no
  invented experience.

## Progressive autonomy

v0.1 is deliberately semi-automatic: the daemon never applies on your
behalf. Cards arrive in Telegram with a URL button — you decide. The caps
(`max_analyze_per_cycle`, `max_cards_per_day`) keep the pipeline from
spamming either you or the boards. Full-auto would need browser automation
of a logged-in session (fragile, ban-worthy, and a misfired application is
un-recallable) — the architecture leaves room for it as a per-vacancy opt-in
later.

## Cost model

The gate is a local 4B decider (~0.1 s, no cloud) and kills ~90% of the
feed. Only survivors cost an agent run (~1–3 min each, capped per cycle).
Everything is local-first: the decider is a llama.cpp server on localhost.

## Run

```sh
cp config.example.yml config.yml   # set filters, criteria, agent work_dir
export JOBWATCH_TG_TOKEN=...       # from @BotFather
./jobwatch -config config.yml          # daemon loop
./jobwatch -config config.yml -once -dry   # one cycle, cards to stdout
```

Requires: a [localmix](https://github.com/Grshor/localmix) decider on :8095
(`lm start decider`) and the `omp` CLI. Tested e2e against live hh.ru search:
20 candidates → gate → 4 analyzed cards, traps and letters rendered.

## Layout

- `internal/hh` — SSR page scraper; breaks loudly (zero results), never silently wrong
- `internal/gate` — `lm decide` wrapper; degrades to "ungated" (pass-through), never to silence
- `internal/agent` — headless `omp` analysis, markdown-tolerant strict-format parser
- `internal/tg` — Bot API sender, URL-button cards
- `internal/store` — atomic JSON state: dedup set + verdict history, TTL-pruned
- `internal/daemon` — the loop and the budgets

## License

MIT
