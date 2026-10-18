// Package progressive implements a linked progressive jackpot controller.
//
// A bank links a set of machines. Every wager on a linked machine increments
// the bank's jackpot by a fixed percentage of coin-in (rate, in basis
// points). When a machine hits, it claims the jackpot: the controller
// freezes the current value as the award, resets the bank to its base value
// and records the claim; the machine then reports a jackpot_hit carrying the
// claim id and awarded amount, which the controller verifies.
//
// Values are tracked in milli-credits so that small wagers on low rates
// still accumulate exactly.
package progressive

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// Bank is one linked progressive.
type Bank struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Base          int64     `json:"base"`    // reset value, credits
	RateBP        int64     `json:"rate_bp"` // contribution, basis points of coin-in (100 = 1%)
	Ceiling       int64     `json:"ceiling"` // max value, credits (0 = none)
	CurrentMilli  int64     `json:"current_milli"`
	Hits          int64     `json:"hits"`
	LastHitAt     time.Time `json:"last_hit_at,omitempty"`
	LastHitAmount int64     `json:"last_hit_amount"`
	LastHitBy     string    `json:"last_hit_by,omitempty"`
	Contributed   int64     `json:"contributed_milli"` // lifetime contributions
	Machines      []string  `json:"machines"`
}

// Value is the current jackpot in whole credits.
func (b *Bank) Value() int64 { return b.CurrentMilli / 1000 }

// Claim is a granted, not-yet-reported jackpot.
type Claim struct {
	ID      string
	Bank    string
	Machine string
	Amount  int64
	At      time.Time
}

// Errors.
var (
	ErrNoBank      = errors.New("unknown progressive bank")
	ErrNotLinked   = errors.New("machine is not linked to this bank")
	ErrNoClaim     = errors.New("unknown progressive claim")
	ErrClaimDiffer = errors.New("jackpot does not match granted claim")
)

// Controller owns all banks.
type Controller struct {
	banks  map[string]*Bank
	member map[string]string // machine -> bank
	claims map[string]Claim
	dirty  map[string]bool
}

// New returns an empty controller.
func New() *Controller {
	return &Controller{banks: map[string]*Bank{}, member: map[string]string{}, claims: map[string]Claim{}, dirty: map[string]bool{}}
}

// Upsert adds or reconfigures a bank (keeping its current value, unless the
// bank is new, in which case it starts at base).
func (c *Controller) Upsert(b Bank) error {
	if b.ID == "" || b.Base < 0 || b.RateBP <= 0 || b.RateBP > 2000 {
		return fmt.Errorf("invalid bank: id required, base >= 0, 0 < rate_bp <= 2000")
	}
	if b.Ceiling != 0 && b.Ceiling < b.Base {
		return fmt.Errorf("ceiling below base")
	}
	if old, ok := c.banks[b.ID]; ok {
		if b.CurrentMilli == 0 {
			b.CurrentMilli = old.CurrentMilli
			b.Hits, b.LastHitAt, b.LastHitAmount, b.LastHitBy, b.Contributed = old.Hits, old.LastHitAt, old.LastHitAmount, old.LastHitBy, old.Contributed
		}
		if b.Machines == nil {
			b.Machines = old.Machines
		}
		for _, m := range old.Machines {
			delete(c.member, m)
		}
	} else if b.CurrentMilli == 0 {
		b.CurrentMilli = b.Base * 1000
	}
	bb := b
	bb.Machines = append([]string(nil), b.Machines...)
	c.banks[b.ID] = &bb
	for _, m := range bb.Machines {
		c.member[m] = b.ID
	}
	c.dirty[b.ID] = true
	return nil
}

// Link adds a machine to a bank.
func (c *Controller) Link(bank, machine string) error {
	b, ok := c.banks[bank]
	if !ok {
		return ErrNoBank
	}
	if c.member[machine] == bank {
		return nil
	}
	if old, ok := c.member[machine]; ok {
		c.unlink(old, machine)
	}
	b.Machines = append(b.Machines, machine)
	sort.Strings(b.Machines)
	c.member[machine] = bank
	c.dirty[bank] = true
	return nil
}

func (c *Controller) unlink(bank, machine string) {
	b := c.banks[bank]
	out := b.Machines[:0]
	for _, m := range b.Machines {
		if m != machine {
			out = append(out, m)
		}
	}
	b.Machines = out
	delete(c.member, machine)
	c.dirty[bank] = true
}

// BankOf returns the bank a machine belongs to ("" if none).
func (c *Controller) BankOf(machine string) string { return c.member[machine] }

// Get returns a copy of a bank.
func (c *Controller) Get(id string) (Bank, bool) {
	b, ok := c.banks[id]
	if !ok {
		return Bank{}, false
	}
	cp := *b
	cp.Machines = append([]string(nil), b.Machines...)
	return cp, true
}

// Banks returns copies of all banks, ordered by id.
func (c *Controller) Banks() []Bank {
	out := make([]Bank, 0, len(c.banks))
	for id := range c.banks {
		b, _ := c.Get(id)
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Contribution returns the milli-credit increment for a wager at rateBP.
func Contribution(wager, rateBP int64) int64 {
	// wager credits * rate/10000 * 1000 milli = wager * rate / 10
	return wager * rateBP / 10
}

// Contribute increments the machine's bank (if any) for a wager and returns
// the bank id touched.
func (c *Controller) Contribute(machine string, wager int64) string {
	id, ok := c.member[machine]
	if !ok {
		return ""
	}
	b := c.banks[id]
	inc := Contribution(wager, b.RateBP)
	b.CurrentMilli += inc
	b.Contributed += inc
	if b.Ceiling > 0 && b.CurrentMilli > b.Ceiling*1000 {
		b.CurrentMilli = b.Ceiling * 1000
	}
	c.dirty[id] = true
	return id
}

// Grant prepares a claim for a machine's hit on bank. It does not change
// the bank; the caller writes a progressive_award event and calls Award.
func (c *Controller) Grant(bank, machine, claimID string, at time.Time) (Claim, error) {
	b, ok := c.banks[bank]
	if !ok {
		return Claim{}, ErrNoBank
	}
	if c.member[machine] != bank {
		return Claim{}, ErrNotLinked
	}
	return Claim{ID: claimID, Bank: bank, Machine: machine, Amount: b.Value(), At: at}, nil
}

// Award applies a progressive_award: resets the bank to base and records
// the claim so the machine's jackpot_hit can be verified.
func (c *Controller) Award(cl Claim) error {
	b, ok := c.banks[cl.Bank]
	if !ok {
		return ErrNoBank
	}
	// Reset to base. Grant and Award run back to back inside the core's
	// single apply loop, so no contribution can land between them.
	b.CurrentMilli = b.Base * 1000
	b.Hits++
	b.LastHitAt = cl.At
	b.LastHitAmount = cl.Amount
	b.LastHitBy = cl.Machine
	c.claims[cl.ID] = cl
	c.dirty[cl.Bank] = true
	return nil
}

// Verify consumes a claim reported by a machine's jackpot_hit.
func (c *Controller) Verify(claimID, machine string, amount int64) error {
	cl, ok := c.claims[claimID]
	if !ok {
		return ErrNoClaim
	}
	delete(c.claims, claimID)
	if cl.Machine != machine || cl.Amount != amount {
		return fmt.Errorf("%w: granted %d to %s, reported %d by %s", ErrClaimDiffer, cl.Amount, cl.Machine, amount, machine)
	}
	return nil
}

// Drain returns banks changed since the last call.
func (c *Controller) Drain() []Bank {
	out := make([]Bank, 0, len(c.dirty))
	for id := range c.dirty {
		b, _ := c.Get(id)
		out = append(out, b)
	}
	c.dirty = map[string]bool{}
	return out
}
