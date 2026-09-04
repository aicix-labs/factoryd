package status

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Server serves one page and one JSON endpoint for any number of
// factories. Handlers only read; there is no POST, and a request method
// other than GET or HEAD is refused so that nothing can be mistaken for a
// control surface.
type Server struct {
	collectors []*Collector
}

// NewServer returns a server over the collectors.
func NewServer(cs []*Collector) (*Server, error) {
	if len(cs) == 0 {
		return nil, ErrNoFactories
	}
	return &Server{collectors: cs}, nil
}

// Snapshots collects every factory, sorted by name.
func (s *Server) Snapshots(ctx context.Context) []Snapshot {
	out := make([]Snapshot, 0, len(s.collectors))
	for _, c := range s.collectors {
		out = append(out, c.Collect(ctx))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Factory < out[j].Factory })
	return out
}

// Handler routes / (HTML) and /status.json.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page)
	mux.HandleFunc("HEAD /{$}", s.page)
	mux.HandleFunc("GET /status.json", s.jsonEndpoint)
	mux.HandleFunc("HEAD /status.json", s.jsonEndpoint)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "status is read-only", http.StatusMethodNotAllowed)
			return
		}
		http.NotFound(w, r)
	})
	return mux
}

func (s *Server) jsonEndpoint(w http.ResponseWriter, r *http.Request) {
	snaps := s.Snapshots(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(snaps)
}

func (s *Server) page(w http.ResponseWriter, r *http.Request) {
	snaps := s.Snapshots(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := pageTmpl.Execute(w, snaps); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// Text renders a snapshot for a terminal, in the page's order of questions.
func Text(s Snapshot) string {
	var sb strings.Builder
	state := "WORKING"
	if !s.Working {
		state = "NOT WORKING"
	}
	fmt.Fprintf(&sb, "%s  %s  (%s -> %s)  at %s\n", s.Factory, state, s.Provider, s.Target, s.At.Format(time.RFC3339))
	if len(s.NeedsMe) > 0 {
		sb.WriteString("needs you:\n")
		for _, n := range s.NeedsMe {
			fmt.Fprintf(&sb, "  ! %s\n", n)
		}
	}
	for _, role := range []string{"producer", "reviewer"} {
		v := s.Roles[role]
		fmt.Fprintf(&sb, "%-9s %s", role, roleLine(v))
		if v.Turn != nil {
			if v.Turn.Running {
				fmt.Fprintf(&sb, "; running %s (%s) for %s", v.Turn.ID, v.Turn.Trigger, v.Turn.AgeText)
			} else {
				fmt.Fprintf(&sb, "; idle, last turn %s exit %s after %s", v.Turn.ID, exitText(v.Turn.Exit), v.Turn.AgeText)
			}
		}
		if v.LastHalt != nil && !v.Halted {
			fmt.Fprintf(&sb, "; recovered from a halt (%s) at %s", v.LastHalt.Reason, v.LastHalt.ClearedAt.Format(time.RFC3339))
		}
		if b := v.Blocked; b != nil {
			fmt.Fprintf(&sb, "; submission %s (%s)", b.Disposition, b.Reason)
		}
		if v.LeftoverTurns > 0 {
			fmt.Fprintf(&sb, "; %d turn(s) left processes behind (killed; the agent's tooling is leaking)", v.LeftoverTurns)
		}
		if len(v.Pending) > 0 {
			sb.WriteString("; waiting on")
			for _, p := range v.Pending {
				fmt.Fprintf(&sb, " %s(%s)", p.Label, p.AgeText)
			}
		}
		sb.WriteString("\n")
	}
	switch {
	case s.Health.Err != "":
		fmt.Fprintf(&sb, "health    UNREADABLE: %s\n", s.Health.Err)
	case !s.Health.Present:
		sb.WriteString("health    none\n")
	default:
		h := "healthy"
		if !s.Health.Healthy {
			h = fmt.Sprintf("%d finding(s)", len(s.Health.Findings))
		}
		if s.Health.Stale {
			h += " STALE"
		}
		fmt.Fprintf(&sb, "health    %s, %s ago\n", h, s.Health.AgeText)
		for _, v := range s.Health.Volumes {
			fmt.Fprintf(&sb, "  volume %-28s %.1f%% free\n", v.Path, v.FreePercent)
		}
	}
	switch {
	case s.Changes.Skipped:
		sb.WriteString("changes   not queried (no provider access)\n")
	case s.Changes.Err != "":
		fmt.Fprintf(&sb, "changes   UNKNOWN: %s (last good %s)\n", s.Changes.Err, s.Changes.AsOf.Format(time.RFC3339))
	default:
		fmt.Fprintf(&sb, "changes   %d open as of %s\n", len(s.Changes.Open), s.Changes.AsOf.Format(time.RFC3339))
		for _, c := range s.Changes.Open {
			d := "ready"
			if c.Draft {
				d = "draft"
			}
			fmt.Fprintf(&sb, "  %-6s %-5s %s -> %s  %s\n", c.ID, d, c.SourceBranch, c.TargetBranch, c.Title)
		}
	}
	if s.Verdict != nil {
		fmt.Fprintf(&sb, "verdict   %s on %s at %s: %s\n", s.Verdict.Kind, s.Verdict.ChangeID, s.Verdict.At.Format(time.RFC3339), s.Verdict.Summary)
	}
	return sb.String()
}

func roleLine(v RoleView) string {
	switch {
	case v.Halted:
		return "HALTED: " + v.HaltReason
	case v.Stopped:
		return "stop sentinel present"
	case v.Supervisor == nil:
		return "never supervised"
	case v.Alive == nil:
		return "liveness unknown"
	case !*v.Alive:
		return "supervisor DEAD"
	default:
		return fmt.Sprintf("supervisor pid %d alive (%s)", v.Supervisor.PID, v.WatchMode)
	}
}

func exitText(e *int) string {
	if e == nil {
		return "?"
	}
	return fmt.Sprint(*e)
}

// need is one "needs you" entry as the page renders it: the first
// paragraph -- by convention the verdict and its reason -- shown, the rest
// behind a disclosure. Authored line structure is kept (pre-wrap): a
// reviewer's summary is written to stand alone and often runs to several
// paragraphs with regexes in it, and collapsing it into one run-on line
// buried the actionable sentence and broke the patterns at arbitrary
// points (#46). Escaping is the template's; this only splits.
type need struct {
	First, Rest string
}

// splitNeed splits at the first blank line. A single paragraph has no Rest.
func splitNeed(s string) need {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\n\n"); i >= 0 {
		return need{First: strings.TrimSpace(s[:i]), Rest: strings.TrimSpace(s[i+2:])}
	}
	return need{First: s}
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"roleLine": roleLine, "exitText": exitText,
	"draft": func(b bool) string {
		if b {
			return "draft"
		}
		return "ready"
	},
	"rfc":   func(t time.Time) string { return t.Format(time.RFC3339) },
	"pct":   func(f float64) string { return fmt.Sprintf("%.1f%%", f) },
	"deref": func(b *bool) bool { return b != nil && *b },
	"split": splitNeed,
}).Parse(`<!doctype html><meta charset="utf-8"><title>factoryd status</title>
<meta http-equiv="refresh" content="15">
<style>
body{font:14px/1.4 system-ui,sans-serif;margin:1.5rem;color:#222;background:#fafafa}
h1{font-size:1.2rem;margin:0 0 .25rem}h2{font-size:1rem;margin:1.25rem 0 .25rem}
.ok{color:#0a7a2f}.bad{color:#b00020}.warn{color:#9a6700}
section{background:#fff;border:1px solid #ddd;border-radius:6px;padding:1rem;margin-bottom:1.25rem}
ul{margin:.25rem 0;padding-left:1.25rem}code{background:#f0f0f0;padding:0 .2em}
table{border-collapse:collapse}td,th{text-align:left;padding:.15rem .6rem .15rem 0;vertical-align:top}
.tree{font-family:ui-monospace,monospace;font-size:12px;white-space:pre}
small{color:#666}
.need{white-space:pre-wrap;overflow-wrap:anywhere;border-left:3px solid #b00020;padding:.4rem .75rem;margin:.5rem 0;background:#fff6f6}
.need summary{cursor:pointer;font-weight:600}
.need .rest{margin-top:.5rem}
</style>
{{range .}}<section>
<h1>{{.Factory}} <span class="{{if .Working}}ok{{else}}bad{{end}}">{{if .Working}}working{{else}}NOT WORKING{{end}}</span>
<small>{{.Provider}} → {{.Target}} · {{rfc .At}}</small></h1>
{{if .NeedsMe}}<h2 class="bad">Needs you</h2>{{range .NeedsMe}}{{$n := split .}}{{if $n.Rest}}<details class="need"><summary>{{$n.First}}</summary><div class="rest">{{$n.Rest}}</div></details>{{else}}<div class="need">{{$n.First}}</div>{{end}}{{end}}{{end}}
<h2>Right now</h2>
{{template "roles" .}}
<h2>Health</h2>
{{if .Health.Err}}<p class="bad">unreadable: {{.Health.Err}}</p>{{else if not .Health.Present}}<p class="bad">no health document</p>{{else}}
<p class="{{if .Health.Healthy}}ok{{else}}bad{{end}}">{{if .Health.Healthy}}healthy{{else}}{{len .Health.Findings}} finding(s){{end}}{{if .Health.Stale}} <span class="warn">STALE</span>{{end}} <small>{{.Health.AgeText}} ago</small></p>
{{if .Health.Findings}}<ul>{{range .Health.Findings}}<li><code>{{.Key}}</code> {{.Summary}}</li>{{end}}</ul>{{end}}
<table>{{range .Health.Volumes}}<tr><td>volume</td><td><code>{{.Path}}</code></td><td>{{pct .FreePercent}} free</td></tr>{{end}}
{{range .Health.Caches}}<tr><td>cache</td><td><code>{{.Path}}</code></td><td>{{.Bytes}} of {{.MaxBytes}} bytes{{if .ReclaimedCount}}; reclaimed {{.ReclaimedCount}}{{end}}{{if .Err}} <span class="bad">{{.Err}}</span>{{end}}</td></tr>{{end}}</table>{{end}}
<h2>Open changes</h2>
{{if .Changes.Skipped}}<p><small>not queried (status has no provider access)</small></p>
{{else if .Changes.Err}}<p class="bad">unknown: {{.Changes.Err}} <small>last good {{rfc .Changes.AsOf}}</small></p>{{end}}
{{if .Changes.Open}}<table>{{range .Changes.Open}}<tr><td>{{.ID}}</td><td>{{draft .Draft}}</td><td><code>{{.SourceBranch}}</code> → <code>{{.TargetBranch}}</code></td><td>{{.Title}}</td><td><small>{{.Author}}</small></td></tr>{{end}}</table>
{{else if not .Changes.Skipped}}<p><small>none open as of {{rfc .Changes.AsOf}}</small></p>{{end}}
{{if .Verdict}}<h2>Last verdict</h2><p><b>{{.Verdict.Kind}}</b> on {{.Verdict.ChangeID}} <small>{{rfc .Verdict.At}}</small><br>{{.Verdict.Summary}}</p>{{end}}
{{if .Errors}}<h2 class="bad">Could not read</h2><ul>{{range .Errors}}<li>{{.}}</li>{{end}}</ul>{{end}}
</section>{{end}}
{{define "roles"}}<table>
{{range $role, $v := .Roles}}<tr><th>{{$role}}</th><td>
<span class="{{if or $v.Halted $v.Stopped}}bad{{else if and $v.Alive (deref $v.Alive)}}ok{{else}}bad{{end}}">{{roleLine $v}}</span>
{{if $v.Turn}}<br>{{if $v.Turn.Running}}running <code>{{$v.Turn.ID}}</code> ({{$v.Turn.Trigger}}) for {{$v.Turn.AgeText}}{{else}}idle; last turn <code>{{$v.Turn.ID}}</code> exit {{exitText $v.Turn.Exit}}, {{$v.Turn.AgeText}} ago{{end}}{{end}}
{{if $v.Pending}}<br>waiting on:{{range $v.Pending}} <code>{{.Label}}</code> ({{.AgeText}}){{end}}{{end}}
{{if or $v.Spin $v.Fails}}<br><small>spin {{$v.Spin}}, fail streak {{$v.Fails}}</small>{{end}}
{{if $v.Tree}}<div class="tree">{{template "tree" $v.Tree}}</div>{{end}}
</td></tr>{{end}}</table>{{end}}
{{define "tree"}}{{.PID}} {{.Label}}{{range .Children}}
  └ {{template "tree" .}}{{end}}{{end}}
`))
