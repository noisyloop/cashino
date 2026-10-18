// Package bonus is a small, declarative promotions rule engine.
//
// Two rule kinds are supported:
//
//	points_multiplier  multiply points earned per wager while the rule's
//	                   conditions hold, e.g. "2x points on weekdays 2-6pm".
//	                   Multipliers do not stack: the highest applicable wins.
//	session_freeplay   award free-play credits once per card session when the
//	                   session's coin-in crosses a threshold, e.g. "500 free
//	                   play credits when a session crosses $100 coin-in".
//
// Rules are JSON documents (see docs/bonusing.md) stored in the database and
// edited by admins; every edit is audited.
package bonus

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Kinds of rule.
const (
	PointsMultiplier = "points_multiplier"
	SessionFreeplay  = "session_freeplay"
)

// Rule is one promotion.
type Rule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`

	// Conditions. Empty means "any".
	Days      []int    `json:"days,omitempty"`       // 0=Sunday .. 6=Saturday
	StartHour int      `json:"start_hour,omitempty"` // local hour, inclusive
	EndHour   int      `json:"end_hour,omitempty"`   // local hour, exclusive; 0 with StartHour 0 = all day
	Tiers     []string `json:"tiers,omitempty"`
	Banks     []string `json:"banks,omitempty"`
	Machines  []string `json:"machines,omitempty"`
	From      string   `json:"from,omitempty"` // YYYY-MM-DD inclusive
	Until     string   `json:"until,omitempty"`

	// Actions.
	Multiplier float64 `json:"multiplier,omitempty"` // points_multiplier
	Threshold  int64   `json:"threshold,omitempty"`  // session_freeplay: coin-in credits
	Award      int64   `json:"award,omitempty"`      // session_freeplay: free-play credits
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// Validate checks a rule definition.
func (r *Rule) Validate() error {
	if !idRe.MatchString(r.ID) {
		return fmt.Errorf("id must match %s", idRe)
	}
	if r.Name == "" || len(r.Name) > 100 {
		return fmt.Errorf("name is required (max 100 chars)")
	}
	for _, d := range r.Days {
		if d < 0 || d > 6 {
			return fmt.Errorf("days must be 0..6")
		}
	}
	if r.StartHour < 0 || r.StartHour > 23 || r.EndHour < 0 || r.EndHour > 24 {
		return fmt.Errorf("hours must be within 0..24")
	}
	if r.EndHour != 0 && r.EndHour <= r.StartHour {
		return fmt.Errorf("end_hour must be after start_hour")
	}
	for _, s := range []string{r.From, r.Until} {
		if s != "" {
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return fmt.Errorf("dates must be YYYY-MM-DD")
			}
		}
	}
	switch r.Kind {
	case PointsMultiplier:
		if r.Multiplier <= 1 || r.Multiplier > 10 {
			return fmt.Errorf("multiplier must be in (1, 10]")
		}
	case SessionFreeplay:
		if r.Threshold <= 0 || r.Award <= 0 || r.Award > 1_000_000 {
			return fmt.Errorf("threshold and award must be positive (award <= 1,000,000)")
		}
	default:
		return fmt.Errorf("unknown kind %q", r.Kind)
	}
	return nil
}

// Context describes the wager or session being evaluated. Time must already
// be in the casino's local time zone.
type Context struct {
	Time    time.Time
	Tier    string
	Machine string
	Bank    string
}

func contains[T comparable](xs []T, x T) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// Matches reports whether the rule's conditions hold for ctx.
func (r *Rule) Matches(ctx Context) bool {
	if !r.Enabled {
		return false
	}
	if len(r.Days) > 0 && !contains(r.Days, int(ctx.Time.Weekday())) {
		return false
	}
	if r.StartHour != 0 || r.EndHour != 0 {
		h := ctx.Time.Hour()
		end := r.EndHour
		if end == 0 {
			end = 24
		}
		if h < r.StartHour || h >= end {
			return false
		}
	}
	day := ctx.Time.Format("2006-01-02")
	if r.From != "" && day < r.From {
		return false
	}
	if r.Until != "" && day > r.Until {
		return false
	}
	if len(r.Tiers) > 0 && !contains(r.Tiers, ctx.Tier) {
		return false
	}
	if len(r.Banks) > 0 && !contains(r.Banks, ctx.Bank) {
		return false
	}
	if len(r.Machines) > 0 && !contains(r.Machines, ctx.Machine) {
		return false
	}
	return true
}

// Engine evaluates a set of rules.
type Engine struct {
	rules []Rule
}

// NewEngine builds an engine; invalid rules are rejected.
func NewEngine(rules []Rule) (*Engine, error) {
	e := &Engine{}
	return e, e.Set(rules)
}

// Set replaces the rule set atomically.
func (e *Engine) Set(rules []Rule) error {
	seen := map[string]bool{}
	for i := range rules {
		if err := rules[i].Validate(); err != nil {
			return fmt.Errorf("rule %q: %w", rules[i].ID, err)
		}
		if seen[rules[i].ID] {
			return fmt.Errorf("duplicate rule id %q", rules[i].ID)
		}
		seen[rules[i].ID] = true
	}
	cp := append([]Rule(nil), rules...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	e.rules = cp
	return nil
}

// Rules returns a copy of the rule set.
func (e *Engine) Rules() []Rule { return append([]Rule(nil), e.rules...) }

// Multiplier returns the points multiplier for a wager (1 when no rule
// applies) and the id of the rule that supplied it.
func (e *Engine) Multiplier(ctx Context) (float64, string) {
	best, id := 1.0, ""
	for i := range e.rules {
		r := &e.rules[i]
		if r.Kind == PointsMultiplier && r.Matches(ctx) && r.Multiplier > best {
			best, id = r.Multiplier, r.ID
		}
	}
	return best, id
}

// Award is a free-play grant produced by a session_freeplay rule.
type Award struct {
	RuleID string
	Amount int64
}

// SessionAwards returns the free-play awards earned when a session's coin-in
// moves from prev to next. A rule fires at most once per session; already
// lists rule ids that have fired in this session.
func (e *Engine) SessionAwards(ctx Context, prev, next int64, already map[string]bool) []Award {
	var out []Award
	for i := range e.rules {
		r := &e.rules[i]
		if r.Kind != SessionFreeplay || already[r.ID] || !r.Matches(ctx) {
			continue
		}
		if prev < r.Threshold && next >= r.Threshold {
			out = append(out, Award{RuleID: r.ID, Amount: r.Award})
		}
	}
	return out
}

// ParseRules decodes a JSON array of rules.
func ParseRules(b []byte) ([]Rule, error) {
	var rs []Rule
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

// DefaultRules is the demo promotion set seeded into a fresh database.
func DefaultRules() []Rule {
	return []Rule{
		{ID: "weekday-happy-hour", Name: "2x points weekdays 2-6pm", Enabled: true, Kind: PointsMultiplier,
			Days: []int{1, 2, 3, 4, 5}, StartHour: 14, EndHour: 18, Multiplier: 2},
		{ID: "platinum-1-5x", Name: "Platinum 1.5x points", Enabled: true, Kind: PointsMultiplier,
			Tiers: []string{"platinum"}, Multiplier: 1.5},
		{ID: "session-100", Name: "$5 free play at $100 session coin-in", Enabled: true, Kind: SessionFreeplay,
			Threshold: 10_000, Award: 500},
	}
}
