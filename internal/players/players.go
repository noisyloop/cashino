// Package players implements player accounts, card sessions, points, tiers
// and comps. Everything is derived from card_in/card_out/game events plus
// host events (bonus awards, comp redemptions), so it is fully replayable.
package players

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"
	"time"
)

// Tier thresholds on lifetime tier points.
var tiers = []struct {
	Name     string
	Min      int64
	CompRate float64 // fraction of theoretical win returned as comps
}{
	{"bronze", 0, 0.10},
	{"silver", 5_000, 0.15},
	{"gold", 25_000, 0.20},
	{"platinum", 100_000, 0.25},
}

// TierFor returns the tier for a lifetime tier-point total.
func TierFor(points int64) string {
	t := tiers[0].Name
	for _, x := range tiers {
		if points >= x.Min {
			t = x.Name
		}
	}
	return t
}

// CompRate returns the comp accrual rate for a tier.
func CompRate(tier string) float64 {
	for _, x := range tiers {
		if x.Name == tier {
			return x.CompRate
		}
	}
	return tiers[0].CompRate
}

// PointsPerCredit: one point per 100 credits ($1.00) wagered.
const creditsPerPoint = 100

// PointsMilli returns milli-points earned for a wager at a multiplier.
func PointsMilli(wager int64, mult float64) int64 {
	return int64(math.Round(float64(wager) * 1000 / creditsPerPoint * mult))
}

// Account is a player's club account. Names are generated, never real.
type Account struct {
	ID          int64     `json:"id"`
	Card        string    `json:"card"`
	Name        string    `json:"name"`
	Tier        string    `json:"tier"`
	TierMilli   int64     `json:"tier_points_milli"` // lifetime, drives tier
	PointsMilli int64     `json:"points_milli"`      // redeemable balance
	CompMilli   int64     `json:"comp_milli"`        // comp balance, milli-credits
	FreePlay    int64     `json:"freeplay"`          // free-play credits awarded, unredeemed
	CreatedAt   time.Time `json:"created_at"`
	LastSeen    time.Time `json:"last_seen"`
}

// Session is one card-in to card-out period on a machine.
type Session struct {
	ID          int64     `json:"id"`
	PlayerID    int64     `json:"player_id"`
	Machine     string    `json:"machine_id"`
	Start       time.Time `json:"started_at"`
	End         time.Time `json:"ended_at,omitempty"`
	CoinIn      int64     `json:"coin_in"`
	CoinOut     int64     `json:"coin_out"`
	Games       int64     `json:"games"`
	TheoMilli   int64     `json:"theo_milli"`
	PointsMilli int64     `json:"points_milli"`
	Awards      []string  `json:"awards"` // bonus rule ids fired this session
	Closed      string    `json:"closed_reason,omitempty"`
}

// Open reports whether the session is still active.
func (s *Session) Open() bool { return s.End.IsZero() }

// Errors.
var (
	ErrNoPlayer     = errors.New("player not found")
	ErrInsufficient = errors.New("insufficient comp balance")
)

// Club is the in-memory player store.
type Club struct {
	byID      map[int64]*Account
	byCard    map[string]*Account
	open      map[string]*Session // machine -> open session
	nextAcct  int64
	nextSess  int64
	dirtyAcct map[int64]bool
	dirtySess map[int64]*Session
}

// New returns an empty club.
func New() *Club {
	return &Club{byID: map[int64]*Account{}, byCard: map[string]*Account{}, open: map[string]*Session{},
		nextAcct: 1, nextSess: 1, dirtyAcct: map[int64]bool{}, dirtySess: map[int64]*Session{}}
}

// LoadAccount restores a persisted account.
func (c *Club) LoadAccount(a Account) {
	aa := a
	c.byID[a.ID] = &aa
	c.byCard[a.Card] = &aa
	if a.ID >= c.nextAcct {
		c.nextAcct = a.ID + 1
	}
}

// LoadSession restores a persisted session (open sessions become active).
func (c *Club) LoadSession(s Session) {
	if s.ID >= c.nextSess {
		c.nextSess = s.ID + 1
	}
	if s.Open() {
		ss := s
		c.open[s.Machine] = &ss
	}
}

// SetNextSession ensures new session ids start above n (closed sessions are
// not loaded into memory).
func (c *Club) SetNextSession(n int64) {
	if n > c.nextSess {
		c.nextSess = n
	}
}

// Get returns a copy of an account.
func (c *Club) Get(id int64) (Account, bool) {
	a, ok := c.byID[id]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// ByCard returns a copy of the account for a card.
func (c *Club) ByCard(card string) (Account, bool) {
	a, ok := c.byCard[card]
	if !ok {
		return Account{}, false
	}
	return *a, true
}

// Search finds accounts by id, card prefix or name substring.
func (c *Club) Search(q string, limit int) []Account {
	q = strings.ToLower(strings.TrimSpace(q))
	var out []Account
	for _, a := range c.byID {
		if q == "" || strings.HasPrefix(a.Card, q) || strings.Contains(strings.ToLower(a.Name), q) || fmt.Sprint(a.ID) == q {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Count returns the number of accounts.
func (c *Club) Count() int { return len(c.byID) }

// OpenSession returns the open session on a machine.
func (c *Club) OpenSession(machine string) (*Session, bool) {
	s, ok := c.open[machine]
	return s, ok
}

// ActiveSessions returns copies of all open sessions.
func (c *Club) ActiveSessions() []Session {
	out := make([]Session, 0, len(c.open))
	for _, s := range c.open {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

func (c *Club) touchSess(s *Session) { c.dirtySess[s.ID] = s }

// fakeName deterministically generates an obviously fictional name.
func fakeName(card string) string {
	first := []string{"Ada", "Bo", "Cy", "Dee", "Eli", "Fay", "Gus", "Hal", "Ivy", "Jo", "Kit", "Lux", "Max", "Nia", "Oz", "Pip", "Quin", "Rex", "Sky", "Tam"}
	last := []string{"Reelwright", "Spinwell", "Paytable", "Lucky", "Cherry", "Sevens", "Barline", "Jackson", "Wilde", "Scatter"}
	h := fnv.New32a()
	h.Write([]byte(card))
	v := h.Sum32()
	return first[v%uint32(len(first))] + " " + last[(v/7)%uint32(len(last))]
}

// CardIn starts a session. Unknown cards are auto-enrolled (as a kiosk
// would). Any session already open on the machine or for the same player is
// closed first. It returns the new session and whether an account was created.
func (c *Club) CardIn(machine, card string, at time.Time) (*Session, bool) {
	created := false
	a, ok := c.byCard[card]
	if !ok {
		a = &Account{ID: c.nextAcct, Card: card, Name: fakeName(card), Tier: TierFor(0), CreatedAt: at}
		c.nextAcct++
		c.byID[a.ID] = a
		c.byCard[card] = a
		created = true
	}
	if s, ok := c.open[machine]; ok {
		c.close(s, at, "replaced")
	}
	for _, s := range c.open {
		if s.PlayerID == a.ID {
			c.close(s, at, "abandoned")
			break
		}
	}
	a.LastSeen = at
	c.dirtyAcct[a.ID] = true
	s := &Session{ID: c.nextSess, PlayerID: a.ID, Machine: machine, Start: at, Awards: []string{}}
	c.nextSess++
	c.open[machine] = s
	c.touchSess(s)
	return s, created
}

func (c *Club) close(s *Session, at time.Time, why string) {
	s.End = at
	s.Closed = why
	delete(c.open, s.Machine)
	c.touchSess(s)
}

// CardOut closes the machine's session.
func (c *Club) CardOut(machine string, at time.Time) (*Session, bool) {
	s, ok := c.open[machine]
	if !ok {
		return nil, false
	}
	c.close(s, at, "card_out")
	return s, true
}

// Wager credits a rated wager to the machine's open session (if any). It
// returns the session and its coin-in before this wager.
func (c *Club) Wager(machine string, wager, theoMilli int64, mult float64, at time.Time) (*Session, int64, bool) {
	s, ok := c.open[machine]
	if !ok {
		return nil, 0, false
	}
	prev := s.CoinIn
	s.CoinIn += wager
	s.Games++
	s.TheoMilli += theoMilli
	pts := PointsMilli(wager, mult)
	s.PointsMilli += pts
	a := c.byID[s.PlayerID]
	a.PointsMilli += pts
	a.TierMilli += PointsMilli(wager, 1) // tier credit is never multiplied
	a.Tier = TierFor(a.TierMilli / 1000)
	a.CompMilli += int64(math.Round(float64(theoMilli) * CompRate(a.Tier)))
	a.LastSeen = at
	c.dirtyAcct[a.ID] = true
	c.touchSess(s)
	return s, prev, true
}

// Win credits a game win to the machine's open session.
func (c *Club) Win(machine string, win int64) {
	if s, ok := c.open[machine]; ok && win > 0 {
		s.CoinOut += win
		c.touchSess(s)
	}
}

// MarkAward records that a bonus rule fired in the session.
func (c *Club) MarkAward(sessionID int64, machine, rule string) {
	if s, ok := c.open[machine]; ok && s.ID == sessionID {
		s.Awards = append(s.Awards, rule)
		c.touchSess(s)
	}
}

// AwardFreePlay adds free-play credits to an account.
func (c *Club) AwardFreePlay(playerID, amount int64) error {
	a, ok := c.byID[playerID]
	if !ok {
		return ErrNoPlayer
	}
	a.FreePlay += amount
	c.dirtyAcct[a.ID] = true
	return nil
}

// CanRedeemComp checks a comp redemption without applying it.
func (c *Club) CanRedeemComp(playerID, amount int64) error {
	a, ok := c.byID[playerID]
	if !ok {
		return ErrNoPlayer
	}
	if amount <= 0 || a.CompMilli < amount*1000 {
		return ErrInsufficient
	}
	return nil
}

// RedeemComp deducts comp credits.
func (c *Club) RedeemComp(playerID, amount int64) error {
	if err := c.CanRedeemComp(playerID, amount); err != nil {
		return err
	}
	a := c.byID[playerID]
	a.CompMilli -= amount * 1000
	c.dirtyAcct[a.ID] = true
	return nil
}

// Drain returns accounts and sessions changed since the last call.
func (c *Club) Drain() ([]Account, []Session) {
	as := make([]Account, 0, len(c.dirtyAcct))
	for id := range c.dirtyAcct {
		as = append(as, *c.byID[id])
	}
	ss := make([]Session, 0, len(c.dirtySess))
	for _, s := range c.dirtySess {
		cp := *s
		cp.Awards = append([]string{}, s.Awards...)
		ss = append(ss, cp)
	}
	c.dirtyAcct = map[int64]bool{}
	c.dirtySess = map[int64]*Session{}
	return as, ss
}
