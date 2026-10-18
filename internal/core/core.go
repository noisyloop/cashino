// Package core owns cashino's derived state and is the only place it changes.
//
// Every state change happens inside a single apply loop (see Writer), in
// event-log order: machine events from ingestion, and host commands (cage
// redemption, progressive claims, sweeps, alert acks) which emit system
// events into the same log. Because the loop is the only writer, replaying
// the log from scratch reproduces exactly the same derived state.
package core

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/noisyloop/cashino/internal/accounting"
	"github.com/noisyloop/cashino/internal/alerts"
	"github.com/noisyloop/cashino/internal/bonus"
	"github.com/noisyloop/cashino/internal/event"
	"github.com/noisyloop/cashino/internal/players"
	"github.com/noisyloop/cashino/internal/progressive"
	"github.com/noisyloop/cashino/internal/tito"
)

// Config holds floor-wide settings.
type Config struct {
	Location      *time.Location // casino local time zone
	DayCutoffHour int            // gaming day starts at this local hour
	TicketExpiry  time.Duration
	IdleAfter     time.Duration // long-idle alert threshold (host clock)
	PlayingWindow time.Duration // a machine is "playing" if a game started within this window
	Detector      alerts.Detector
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{Location: time.UTC, DayCutoffHour: 6, TicketExpiry: 30 * 24 * time.Hour,
		IdleAfter: 15 * time.Minute, PlayingWindow: 30 * time.Second, Detector: alerts.DefaultDetector()}
}

// Machine states shown on the floor.
const (
	StatePlaying  = "playing"
	StateIdle     = "idle"
	StateTilt     = "tilt"
	StateDoorOpen = "door_open"
	StateOffline  = "offline"
)

// MachineConfig is the static configuration of a cabinet.
type MachineConfig struct {
	ID        string    `json:"id"`
	Theme     string    `json:"theme"`
	Zone      string    `json:"zone"`
	Bank      string    `json:"bank,omitempty"`
	Denom     int64     `json:"denom"`
	RTP       float64   `json:"rtp"`
	X         int       `json:"x"`
	Y         int       `json:"y"`
	CreatedAt time.Time `json:"created_at"`
}

// Machine is a cabinet's configuration plus its derived state.
type Machine struct {
	MachineConfig
	Meters       accounting.Meters `json:"meters"`
	LastSeq      uint64            `json:"last_seq"`
	LastEventTS  time.Time         `json:"last_event_ts"`
	LastRecv     time.Time         `json:"last_recv"`
	LastGameRecv time.Time         `json:"-"`
	Tilt         string            `json:"tilt,omitempty"`
	Door         string            `json:"door,omitempty"`
	Online       bool              `json:"online"`
	payout       alerts.PayoutStats
	handpays     []time.Time
	lastWager    int64
}

// State computes the floor state at host time now.
func (m *Machine) State(now time.Time, window time.Duration) string {
	switch {
	case !m.Online:
		return StateOffline
	case m.Tilt != "":
		return StateTilt
	case m.Door != "":
		return StateDoorOpen
	case now.Sub(m.LastGameRecv) <= window:
		return StatePlaying
	}
	return StateIdle
}

// Daily is one machine's totals for one gaming day.
type Daily struct {
	Machine    string `json:"machine_id"`
	Day        string `json:"day"`
	CoinIn     int64  `json:"coin_in"`
	CoinOut    int64  `json:"coin_out"`
	Jackpot    int64  `json:"jackpot"`
	Handpay    int64  `json:"handpay"`
	Games      int64  `json:"games"`
	BillsIn    int64  `json:"bills_in"`
	TicketsIn  int64  `json:"tickets_in"`
	TicketsOut int64  `json:"tickets_out"`
	TheoMilli  int64  `json:"theo_milli"`
}

// NetWin is the day's actual win.
func (d *Daily) NetWin() int64 { return d.CoinIn - d.CoinOut - d.Jackpot }

type dayKey struct{ m, d string }

// Reconciliation is the result of comparing a machine's meter report with
// the event-derived meters at the same sequence point.
type Reconciliation struct {
	Machine    string                `json:"machine_id"`
	AsOfSeq    uint64                `json:"as_of_seq"`
	HostSeq    uint64                `json:"host_seq"`
	At         time.Time             `json:"at"`
	OK         bool                  `json:"ok"`
	Mismatches []accounting.Mismatch `json:"mismatches"`
	Reported   event.Meters          `json:"reported"`
	Host       event.Meters          `json:"host"`
	Trigger    string                `json:"trigger"`
}

// Gap is a detected hole in a machine's sequence numbers.
type Gap struct {
	Machine   string    `json:"machine_id"`
	From      uint64    `json:"from_seq"`
	To        uint64    `json:"to_seq"`
	At        time.Time `json:"detected_at"`
	Recovered bool      `json:"recovered"`
}

// ProgressiveHit records one progressive award.
type ProgressiveHit struct {
	Bank    string       `json:"bank"`
	Machine string       `json:"machine_id"`
	Claim   string       `json:"claim"`
	Amount  int64        `json:"amount"`
	At      time.Time    `json:"at"`
	EventID int64        `json:"event_id"`
	Ev      *event.Event `json:"-"`
}

// Changes is everything a batch changed, handed to the persister.
type Changes struct {
	Events   []*event.Event
	Machines []Machine
	Daily    []Daily
	Tickets  []tito.Ticket
	Ledger   []tito.LedgerEntry
	Accounts []players.Account
	Sessions []players.Session
	Banks    []progressive.Bank
	Hits     []ProgressiveHit
	Alerts   []alerts.Alert
	Recons   []Reconciliation
	Gaps     []Gap
	Streams  map[string]uint64
}

// Empty reports whether nothing changed.
func (c *Changes) Empty() bool {
	return len(c.Events) == 0 && len(c.Machines) == 0 && len(c.Alerts) == 0 && len(c.Recons) == 0 &&
		len(c.Gaps) == 0 && len(c.Banks) == 0 && len(c.Accounts) == 0 && len(c.Sessions) == 0 && len(c.Tickets) == 0
}

// Core is the derived-state engine.
type Core struct {
	mu  sync.RWMutex
	cfg Config

	machines map[string]*Machine
	streams  map[string]uint64 // last seq per source (machines and sys:*)
	daily    map[dayKey]*Daily
	dayTotal map[string]*Daily

	Tickets  *tito.Book
	Club     *players.Club
	Bonus    *bonus.Engine
	Prog     *progressive.Controller
	Alerts   *alerts.Manager
	detector alerts.Detector

	floorNow  time.Time
	replaying bool
	nextEvent int64 // only used when replaying without a database id

	// pending changes
	out          []*event.Event
	dirtyMachine map[string]bool
	dirtyDaily   map[dayKey]bool
	recons       []Reconciliation
	gaps         []Gap
	hits         []ProgressiveHit
	dirtyStream  map[string]bool

	// floor deltas for the live dashboard (independent of persistence)
	floorDirty map[string]bool

	// Stats
	Applied    int64
	Rejected   int64
	Duplicates int64

	// Clock is the host wall clock (overridable in tests).
	Clock func() time.Time
}

// New builds an empty core.
func New(cfg Config) *Core {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	eng, _ := bonus.NewEngine(nil)
	c := &Core{cfg: cfg, machines: map[string]*Machine{}, streams: map[string]uint64{},
		daily: map[dayKey]*Daily{}, dayTotal: map[string]*Daily{},
		Tickets: tito.NewBook(cfg.TicketExpiry), Club: players.New(), Bonus: eng,
		Prog: progressive.New(), Alerts: alerts.New(), detector: cfg.Detector,
		dirtyMachine: map[string]bool{}, dirtyDaily: map[dayKey]bool{}, dirtyStream: map[string]bool{},
		floorDirty: map[string]bool{}, Clock: time.Now}
	return c
}

// Config returns the floor configuration.
func (c *Core) Config() Config { return c.cfg }

// Lock exposes the core mutex for loaders and the writer.
func (c *Core) Lock()    { c.mu.Lock() }
func (c *Core) Unlock()  { c.mu.Unlock() }
func (c *Core) RLock()   { c.mu.RLock() }
func (c *Core) RUnlock() { c.mu.RUnlock() }

// GamingDay returns the gaming day (YYYY-MM-DD) for an instant.
func (c *Core) GamingDay(t time.Time) string {
	return GamingDay(t, c.cfg.Location, c.cfg.DayCutoffHour)
}

// GamingDay maps an instant to its gaming day given a zone and cutoff hour.
func GamingDay(t time.Time, loc *time.Location, cutoff int) string {
	return t.In(loc).Add(-time.Duration(cutoff) * time.Hour).Format("2006-01-02")
}

// FloorNow is the floor clock: the latest event time seen, or the host
// clock before any event. Ticket expiry and promotions use floor time so a
// simulator running on a virtual clock behaves consistently.
func (c *Core) FloorNow() time.Time {
	if c.floorNow.IsZero() {
		return c.Clock()
	}
	return c.floorNow
}

// ---------------------------------------------------------------------------
// Loading
// ---------------------------------------------------------------------------

// LoadMachine registers a machine with optional derived state.
func (c *Core) LoadMachine(m Machine) {
	mm := m
	mm.Online = false
	c.machines[m.ID] = &mm
	if m.LastSeq > c.streams[m.ID] {
		c.streams[m.ID] = m.LastSeq
	}
	if m.LastEventTS.After(c.floorNow) {
		c.floorNow = m.LastEventTS
	}
	if m.Bank != "" {
		_ = c.Prog.Link(m.Bank, m.ID)
	}
}

// LoadDaily restores a daily row.
func (c *Core) LoadDaily(d Daily) {
	dd := d
	c.daily[dayKey{d.Machine, d.Day}] = &dd
	t := c.dayTotalFor(d.Day)
	addDaily(t, &d)
}

// LoadStream restores a source's last sequence.
func (c *Core) LoadStream(src string, seq uint64) {
	if seq > c.streams[src] {
		c.streams[src] = seq
	}
}

// SetReplaying toggles replay mode: alerts and promotion emission are
// suppressed because their effects are already in the log.
func (c *Core) SetReplaying(v bool) { c.replaying = v }

func addDaily(t, d *Daily) {
	t.CoinIn += d.CoinIn
	t.CoinOut += d.CoinOut
	t.Jackpot += d.Jackpot
	t.Handpay += d.Handpay
	t.Games += d.Games
	t.BillsIn += d.BillsIn
	t.TicketsIn += d.TicketsIn
	t.TicketsOut += d.TicketsOut
	t.TheoMilli += d.TheoMilli
}

func (c *Core) dayTotalFor(day string) *Daily {
	t, ok := c.dayTotal[day]
	if !ok {
		t = &Daily{Machine: "*", Day: day}
		c.dayTotal[day] = t
	}
	return t
}

// ---------------------------------------------------------------------------
// Registration
// ---------------------------------------------------------------------------

// Register adds a machine (or returns the existing one). Caller holds the lock.
func (c *Core) register(cfg MachineConfig) (*Machine, bool) {
	if m, ok := c.machines[cfg.ID]; ok {
		return m, false
	}
	if cfg.X == 0 && cfg.Y == 0 {
		n := len(c.machines)
		cfg.X, cfg.Y = n%25, n/25
	}
	if cfg.RTP <= 0 || cfg.RTP >= 1 {
		cfg.RTP = 0.92
	}
	if cfg.Denom <= 0 {
		cfg.Denom = 1
	}
	if cfg.CreatedAt.IsZero() {
		cfg.CreatedAt = c.Clock()
	}
	m := &Machine{MachineConfig: cfg}
	c.machines[cfg.ID] = m
	if cfg.Bank != "" {
		if err := c.Prog.Link(cfg.Bank, cfg.ID); err != nil {
			m.Bank = ""
		}
	}
	c.dirtyMachine[cfg.ID] = true
	c.floorDirty[cfg.ID] = true
	return m, true
}

// Register adds a machine under the lock. It returns the machine config,
// whether it is new and the machine's last durable sequence and meters.
func (c *Core) Register(cfg MachineConfig) (MachineConfig, bool, uint64, accounting.Meters) {
	c.mu.Lock()
	defer c.mu.Unlock()
	m, created := c.register(cfg)
	return m.MachineConfig, created, c.streams[m.ID], m.Meters
}

// Known reports whether a machine is registered.
func (c *Core) Known(id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.machines[id]
	return ok
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

func (c *Core) touch(m *Machine) {
	c.dirtyMachine[m.ID] = true
	c.floorDirty[m.ID] = true
}

func (c *Core) dailyFor(machine string, ts time.Time) (*Daily, *Daily) {
	day := c.GamingDay(ts)
	k := dayKey{machine, day}
	d, ok := c.daily[k]
	if !ok {
		d = &Daily{Machine: machine, Day: day}
		c.daily[k] = d
	}
	c.dirtyDaily[k] = true
	return d, c.dayTotalFor(day)
}

func (c *Core) raise(typ, sev, machine, msg string) {
	if c.replaying {
		return
	}
	c.Alerts.Raise(typ, sev, machine, msg, c.Clock())
}

func (c *Core) resolve(typ, machine, note string) {
	if c.replaying {
		return
	}
	c.Alerts.AutoResolve(typ, machine, note, c.Clock())
}

// emit appends a host-originated event to the log and applies it.
func (c *Core) emit(e *event.Event) {
	seq := c.streams[e.Source] + 1
	e.Seq = seq
	if e.TS.IsZero() {
		e.TS = c.FloorNow()
	}
	e.Recv = c.Clock()
	c.applySystem(e)
	c.streams[e.Source] = seq
	c.dirtyStream[e.Source] = true
	c.out = append(c.out, e)
}

// ApplyMachine applies one validated, in-order machine event. It returns an
// error (and changes nothing) if the event is a duplicate or out of order;
// ingestion is expected to have filtered those already.
func (c *Core) ApplyMachine(e *event.Event) error {
	m, ok := c.machines[e.Source]
	if !ok {
		c.Rejected++
		return fmt.Errorf("unknown machine %s", e.Source)
	}
	last := c.streams[e.Source]
	if e.Seq <= last {
		c.Duplicates++
		return fmt.Errorf("duplicate seq %d (last %d)", e.Seq, last)
	}
	if e.Recv.IsZero() {
		e.Recv = c.Clock()
	}
	c.applyMachine(m, e)
	c.streams[e.Source] = e.Seq
	m.LastSeq = e.Seq
	c.dirtyStream[e.Source] = true
	if !c.replaying {
		c.out = append(c.out, e)
	}
	c.Applied++
	return nil
}

func (c *Core) applyMachine(m *Machine, e *event.Event) {
	if e.TS.After(c.floorNow) {
		c.floorNow = e.TS
	}
	m.LastEventTS = e.TS
	m.LastRecv = e.Recv
	m.Online = true
	m.Meters.Apply(e, m.RTP)
	c.touch(m)
	if !c.replaying {
		c.resolve(alerts.Idle, m.ID, "activity resumed")
	}

	switch e.Type {
	case event.GameStart:
		d, t := c.dailyFor(m.ID, e.TS)
		theo := accounting.TheoMilli(e.Wager, m.RTP)
		for _, x := range []*Daily{d, t} {
			x.CoinIn += e.Wager
			x.Games++
			x.TheoMilli += theo
		}
		m.LastGameRecv = e.Recv
		m.lastWager = e.Wager
		if m.Tilt != "" {
			m.Tilt = ""
			c.resolve(alerts.Tilt, m.ID, "cleared: play resumed")
		}
		c.Prog.Contribute(m.ID, e.Wager)
		c.rateWager(m, e, theo)

	case event.GameEnd:
		if e.Win > 0 {
			d, t := c.dailyFor(m.ID, e.TS)
			d.CoinOut += e.Win
			t.CoinOut += e.Win
			c.Club.Win(m.ID, e.Win)
		}
		c.checkPayout(m, e)

	case event.JackpotHit:
		d, t := c.dailyFor(m.ID, e.TS)
		d.Jackpot += e.Amount
		t.Jackpot += e.Amount
		if e.Claim != "" {
			if err := c.Prog.Verify(e.Claim, m.ID, e.Amount); err != nil {
				c.raise(alerts.JackpotFault, alerts.Critical, m.ID, "progressive jackpot not matching a granted claim: "+err.Error())
			}
		}

	case event.Handpay:
		d, t := c.dailyFor(m.ID, e.TS)
		d.Handpay += e.Amount
		t.Handpay += e.Amount
		c.checkHandpays(m, e)

	case event.BillIn:
		d, t := c.dailyFor(m.ID, e.TS)
		d.BillsIn += e.Amount
		t.BillsIn += e.Amount

	case event.TicketOut:
		d, t := c.dailyFor(m.ID, e.TS)
		d.TicketsOut += e.Amount
		t.TicketsOut += e.Amount
		if e.Expires.IsZero() {
			e.Expires = e.TS.Add(c.cfg.TicketExpiry)
		}
		if err := c.Tickets.Issue(e); err != nil {
			c.raise(alerts.TicketFault, alerts.Critical, m.ID, fmt.Sprintf("ticket_out %s: %v", e.Ticket, err))
		}

	case event.TicketIn:
		d, t := c.dailyFor(m.ID, e.TS)
		d.TicketsIn += e.Amount
		t.TicketsIn += e.Amount
		if !c.replaying && c.Tickets.HeldBy(e.Ticket) != m.ID {
			c.raise(alerts.TicketFault, alerts.Warning, m.ID, fmt.Sprintf("ticket_in %s accepted without host validation", e.Ticket))
		}
		if err := c.Tickets.Redeem(e, m.ID); err != nil {
			c.raise(alerts.TicketFault, alerts.Critical, m.ID, fmt.Sprintf("ticket_in %s rejected by ledger: %v", e.Ticket, err))
		}

	case event.Tilt:
		m.Tilt = e.Code
		c.raise(alerts.Tilt, alerts.Warning, m.ID, "machine tilt: "+e.Code)

	case event.DoorOpen:
		door := e.Code
		if door == "" {
			door = "main"
		}
		m.Door = door
		c.raise(alerts.DoorOpen, alerts.Warning, m.ID, door+" door open")

	case event.DoorClose:
		m.Door = ""
		c.resolve(alerts.DoorOpen, m.ID, "door closed")

	case event.CardIn:
		c.Club.CardIn(m.ID, e.Card, e.TS)

	case event.CardOut:
		c.Club.CardOut(m.ID, e.TS)
	}

	if err := m.Meters.BalanceCheck(); err != nil && !c.Alerts.IsActive(alerts.BalanceFault, m.ID) {
		c.raise(alerts.BalanceFault, alerts.Warning, m.ID, err.Error())
	}
}

// rateWager credits the player's session and evaluates promotions.
func (c *Core) rateWager(m *Machine, e *event.Event, theo int64) {
	s, ok := c.Club.OpenSession(m.ID)
	if !ok {
		return
	}
	acct, _ := c.Club.Get(s.PlayerID)
	ctx := bonus.Context{Time: e.TS.In(c.cfg.Location), Tier: acct.Tier, Machine: m.ID, Bank: m.Bank}
	mult, _ := c.Bonus.Multiplier(ctx)
	s, prev, _ := c.Club.Wager(m.ID, e.Wager, theo, mult, e.TS)
	if c.replaying {
		return // awards are already in the log
	}
	already := map[string]bool{}
	for _, r := range s.Awards {
		already[r] = true
	}
	for _, a := range c.Bonus.SessionAwards(ctx, prev, s.CoinIn, already) {
		c.emit(&event.Event{Source: event.SourceBonus, Type: event.BonusAwarded, TS: e.TS,
			Amount: a.Amount, BonusRule: a.RuleID, PlayerID: s.PlayerID, Machine: m.ID, Reason: fmt.Sprintf("session %d", s.ID)})
	}
}

func (c *Core) checkPayout(m *Machine, e *event.Event) {
	if c.replaying || e.Wager <= 0 {
		return
	}
	if c.detector.OutsizedMult > 0 && e.Win > c.detector.OutsizedMult*e.Wager {
		c.raise(alerts.OutsizedWin, alerts.Warning, m.ID, fmt.Sprintf("win of %d on a %d wager (%dx)", e.Win, e.Wager, e.Win/e.Wager))
	}
	m.payout.Add(e.Wager, e.Win)
	if m.payout.N >= c.detector.MinGames && m.payout.N%250 == 0 {
		if z := m.payout.Z(m.RTP); z > c.detector.ZThreshold {
			c.raise(alerts.PayoutAnomaly, alerts.Critical, m.ID,
				fmt.Sprintf("observed return %.1f%% over %d games vs configured %.1f%% (z=%.1f)", m.payout.Mean*100, m.payout.N, m.RTP*100, z))
			m.payout = alerts.PayoutStats{}
		}
	}
}

func (c *Core) checkHandpays(m *Machine, e *event.Event) {
	if c.replaying || c.detector.HandpayBurst <= 0 {
		return
	}
	cut := e.TS.Add(-c.detector.HandpayWin)
	keep := m.handpays[:0]
	for _, t := range m.handpays {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	m.handpays = append(keep, e.TS)
	if len(m.handpays) >= c.detector.HandpayBurst {
		c.raise(alerts.HandpayBurst, alerts.Critical, m.ID, fmt.Sprintf("%d handpays within %s", len(m.handpays), c.detector.HandpayWin))
	}
}

// applySystem applies a host event (live or replayed).
func (c *Core) applySystem(e *event.Event) {
	switch e.Type {
	case event.TicketRedeemed:
		_ = c.Tickets.Redeem(e, e.Actor)
	case event.TicketExpired:
		_ = c.Tickets.Expire(e)
	case event.TicketVoided:
		_ = c.Tickets.Void(e)
	case event.ProgressiveAward:
		cl := progressive.Claim{ID: e.Claim, Bank: e.Bank, Machine: e.Machine, Amount: e.Amount, At: e.TS}
		if err := c.Prog.Award(cl); err == nil {
			c.hits = append(c.hits, ProgressiveHit{Bank: e.Bank, Machine: e.Machine, Claim: e.Claim, Amount: e.Amount, At: e.TS, EventID: e.ID, Ev: e})
		}
	case event.BonusAwarded:
		_ = c.Club.AwardFreePlay(e.PlayerID, e.Amount)
		if s, ok := c.Club.OpenSession(e.Machine); ok && s.PlayerID == e.PlayerID {
			c.Club.MarkAward(s.ID, e.Machine, e.BonusRule)
		}
	case event.CompRedeemed:
		_ = c.Club.RedeemComp(e.PlayerID, e.Amount)
	}
}

// ApplyReplayed applies an event read back from the log during rebuild.
func (c *Core) ApplyReplayed(e *event.Event) error {
	if e.IsSystem() {
		if e.TS.After(c.floorNow) {
			c.floorNow = e.TS
		}
		c.applySystem(e)
		if e.Seq > c.streams[e.Source] {
			c.streams[e.Source] = e.Seq
			c.dirtyStream[e.Source] = true
		}
		return nil
	}
	return c.ApplyMachine(e)
}

// Drain collects every pending change for persistence. Caller holds the lock.
func (c *Core) Drain() *Changes {
	ch := &Changes{Events: c.out, Recons: c.recons, Gaps: c.gaps, Hits: c.hits, Streams: map[string]uint64{}}
	c.out, c.recons, c.gaps, c.hits = nil, nil, nil, nil
	for id := range c.dirtyMachine {
		ch.Machines = append(ch.Machines, *c.machines[id])
	}
	for k := range c.dirtyDaily {
		ch.Daily = append(ch.Daily, *c.daily[k])
	}
	for s := range c.dirtyStream {
		ch.Streams[s] = c.streams[s]
	}
	c.dirtyMachine, c.dirtyDaily, c.dirtyStream = map[string]bool{}, map[dayKey]bool{}, map[string]bool{}
	ch.Tickets, ch.Ledger = c.Tickets.Drain()
	ch.Accounts, ch.Sessions = c.Club.Drain()
	ch.Banks = c.Prog.Drain()
	ch.Alerts = c.Alerts.Drain()
	sort.Slice(ch.Machines, func(i, j int) bool { return ch.Machines[i].ID < ch.Machines[j].ID })
	return ch
}
