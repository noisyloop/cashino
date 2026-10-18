// Package alerts manages operational alerts (tilt, door open, meter
// mismatch, idle, communication loss, event gaps, ticket and jackpot
// exceptions) and detects anomalous payout patterns.
package alerts

import (
	"math"
	"sort"
	"time"
)

// Alert types.
const (
	Tilt          = "tilt"
	DoorOpen      = "door_open"
	MeterMismatch = "meter_mismatch"
	Idle          = "long_idle"
	CommLoss      = "comm_loss"
	EventGap      = "event_gap"
	PayoutAnomaly = "payout_anomaly"
	OutsizedWin   = "outsized_win"
	HandpayBurst  = "handpay_burst"
	TicketFault   = "ticket_exception"
	JackpotFault  = "jackpot_exception"
	BalanceFault  = "credit_balance"
)

// Severity levels.
const (
	Info     = "info"
	Warning  = "warning"
	Critical = "critical"
)

// States.
const (
	Open     = "open"
	Acked    = "acknowledged"
	Resolved = "resolved"
)

// Alert is one alert instance. While open, repeated raises of the same
// (type, machine) increment Count instead of creating new alerts.
type Alert struct {
	ID         int64     `json:"id"`
	Type       string    `json:"type"`
	Severity   string    `json:"severity"`
	Machine    string    `json:"machine_id"`
	Message    string    `json:"message"`
	State      string    `json:"state"`
	Count      int64     `json:"count"`
	OpenedAt   time.Time `json:"opened_at"`
	LastAt     time.Time `json:"last_at"`
	AckedBy    string    `json:"acked_by,omitempty"`
	AckedAt    time.Time `json:"acked_at,omitempty"`
	AckReason  string    `json:"ack_reason,omitempty"`
	ResolvedBy string    `json:"resolved_by,omitempty"`
	ResolvedAt time.Time `json:"resolved_at,omitempty"`
	Note       string    `json:"note,omitempty"`
}

// Manager stores alerts in memory; the core persists changes.
type Manager struct {
	byID   map[int64]*Alert
	active map[string]*Alert // type|machine -> unresolved alert
	nextID int64
	dirty  map[int64]bool
	// Hook is called for every created or changed alert (for live push).
	Hook func(Alert)
}

// New returns an empty manager.
func New() *Manager {
	return &Manager{byID: map[int64]*Alert{}, active: map[string]*Alert{}, nextID: 1, dirty: map[int64]bool{}}
}

func key(t, m string) string { return t + "|" + m }

// Load restores a persisted alert.
func (m *Manager) Load(a Alert) {
	aa := a
	m.byID[a.ID] = &aa
	if a.State != Resolved {
		m.active[key(a.Type, a.Machine)] = &aa
	}
	if a.ID >= m.nextID {
		m.nextID = a.ID + 1
	}
}

func (m *Manager) changed(a *Alert) {
	m.dirty[a.ID] = true
	if m.Hook != nil {
		m.Hook(*a)
	}
}

// Raise opens an alert, or bumps the count of the matching unresolved one.
func (m *Manager) Raise(typ, sev, machine, msg string, at time.Time) *Alert {
	if a, ok := m.active[key(typ, machine)]; ok {
		a.Count++
		a.LastAt = at
		a.Message = msg
		if sevRank(sev) > sevRank(a.Severity) {
			a.Severity = sev
		}
		m.changed(a)
		return a
	}
	a := &Alert{ID: m.nextID, Type: typ, Severity: sev, Machine: machine, Message: msg, State: Open, Count: 1, OpenedAt: at, LastAt: at}
	m.nextID++
	m.byID[a.ID] = a
	m.active[key(typ, machine)] = a
	m.changed(a)
	return a
}

func sevRank(s string) int {
	switch s {
	case Critical:
		return 2
	case Warning:
		return 1
	}
	return 0
}

// AutoResolve resolves the unresolved alert of (type, machine), if any, on
// behalf of the system (e.g. door closed, tilt cleared by play).
func (m *Manager) AutoResolve(typ, machine, note string, at time.Time) bool {
	a, ok := m.active[key(typ, machine)]
	if !ok {
		return false
	}
	m.resolve(a, "system", note, at)
	return true
}

func (m *Manager) resolve(a *Alert, by, note string, at time.Time) {
	a.State = Resolved
	a.ResolvedBy = by
	a.ResolvedAt = at
	if note != "" {
		a.Note = note
	}
	delete(m.active, key(a.Type, a.Machine))
	m.changed(a)
}

// IsActive reports whether an unresolved alert of (type, machine) exists.
func (m *Manager) IsActive(typ, machine string) bool {
	_, ok := m.active[key(typ, machine)]
	return ok
}

// Ack acknowledges an alert. Reason is mandatory (enforced by the API).
func (m *Manager) Ack(id int64, by, reason string, at time.Time) (Alert, bool) {
	a, ok := m.byID[id]
	if !ok || a.State != Open {
		return Alert{}, false
	}
	a.State = Acked
	a.AckedBy, a.AckReason, a.AckedAt = by, reason, at
	m.changed(a)
	return *a, true
}

// Resolve manually resolves an alert.
func (m *Manager) Resolve(id int64, by, note string, at time.Time) (Alert, bool) {
	a, ok := m.byID[id]
	if !ok || a.State == Resolved {
		return Alert{}, false
	}
	m.resolve(a, by, note, at)
	return *a, true
}

// Get returns a copy of an alert.
func (m *Manager) Get(id int64) (Alert, bool) {
	a, ok := m.byID[id]
	if !ok {
		return Alert{}, false
	}
	return *a, true
}

// List returns alerts, unresolved first then newest first. If openOnly,
// resolved alerts are excluded.
func (m *Manager) List(openOnly bool, machine string, limit int) []Alert {
	var out []Alert
	for _, a := range m.byID {
		if openOnly && a.State == Resolved {
			continue
		}
		if machine != "" && a.Machine != machine {
			continue
		}
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i].State == Resolved, out[j].State == Resolved
		if ri != rj {
			return !ri
		}
		return out[i].ID > out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// OpenCount returns the number of unresolved alerts.
func (m *Manager) OpenCount() int { return len(m.active) }

// Prune drops resolved alerts older than cutoff from memory (they remain in
// the database).
func (m *Manager) Prune(cutoff time.Time) {
	for id, a := range m.byID {
		if a.State == Resolved && a.ResolvedAt.Before(cutoff) && !m.dirty[id] {
			delete(m.byID, id)
		}
	}
}

// Drain returns alerts changed since the last call.
func (m *Manager) Drain() []Alert {
	out := make([]Alert, 0, len(m.dirty))
	for id := range m.dirty {
		out = append(out, *m.byID[id])
	}
	m.dirty = map[int64]bool{}
	return out
}

// ---------------------------------------------------------------------------
// Payout anomaly detection
// ---------------------------------------------------------------------------

// PayoutStats accumulates per-game return ratios (win / wager) for one
// machine using Welford's online algorithm, so the detector can test whether
// the observed return is implausibly far above the configured RTP.
type PayoutStats struct {
	N    int64
	Mean float64
	M2   float64
}

// Add folds one game in.
func (p *PayoutStats) Add(wager, win int64) {
	if wager <= 0 {
		return
	}
	x := float64(win) / float64(wager)
	p.N++
	d := x - p.Mean
	p.Mean += d / float64(p.N)
	p.M2 += d * (x - p.Mean)
}

// Z returns the z-score of the observed mean return against rtp.
func (p *PayoutStats) Z(rtp float64) float64 {
	if p.N < 2 {
		return 0
	}
	sd := math.Sqrt(p.M2 / float64(p.N-1))
	if sd == 0 {
		return 0
	}
	return (p.Mean - rtp) / (sd / math.Sqrt(float64(p.N)))
}

// Detector thresholds.
type Detector struct {
	MinGames     int64   // games required before the z-test runs
	ZThreshold   float64 // z above which payout is anomalous
	OutsizedMult int64   // single win > OutsizedMult * wager is flagged
	HandpayBurst int     // handpays per machine within HandpayWindow
	HandpayWin   time.Duration
}

// DefaultDetector returns thresholds tuned for the simulator's pay tables.
func DefaultDetector() Detector {
	return Detector{MinGames: 2000, ZThreshold: 5, OutsizedMult: 2500, HandpayBurst: 4, HandpayWin: time.Hour}
}
