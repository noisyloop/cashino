package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/noisyloop/cashino/internal/accounting"
	"github.com/noisyloop/cashino/internal/alerts"
	"github.com/noisyloop/cashino/internal/bonus"
	"github.com/noisyloop/cashino/internal/event"
	"github.com/noisyloop/cashino/internal/progressive"
	"github.com/noisyloop/cashino/internal/tito"
)

// Cmd is a host command executed inside the apply loop with the core lock
// held. It may emit system events via c.emit.
type Cmd func(c *Core) (any, error)

// ErrNotFound is returned for unknown entities.
var ErrNotFound = errors.New("not found")

// ---------------------------------------------------------------------------
// TITO
// ---------------------------------------------------------------------------

// ValidateTicket handles a machine's ticket validation request. It runs
// outside the apply loop (it only places a transient hold).
func (c *Core) ValidateTicket(v, machine string) (tito.Ticket, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Tickets.Validate(v, machine, c.FloorNow())
}

// ReleaseTicket drops a machine's validation hold.
func (c *Core) ReleaseTicket(v, machine string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Tickets.Release(v, machine)
}

// CageRedeem redeems a ticket at the cage. A second redemption of the same
// ticket fails with tito.ErrAlreadyRedeemed.
func CageRedeem(v, user, reason string) Cmd {
	return func(c *Core) (any, error) {
		t, err := c.Tickets.Reserve(v, "cage:"+user, c.FloorNow())
		if err != nil {
			return t, err
		}
		e := &event.Event{Source: event.SourceCage, Type: event.TicketRedeemed, Ticket: v, Amount: t.Amount, Actor: user, Reason: reason, Machine: t.IssuedBy}
		c.emit(e)
		t, _ = c.Tickets.Get(v)
		return t, nil
	}
}

// VoidTicket voids an outstanding ticket (manual, audited).
func VoidTicket(v, user, reason string) Cmd {
	return func(c *Core) (any, error) {
		t, ok := c.Tickets.Get(v)
		if !ok {
			return nil, tito.ErrNotFound
		}
		if t.Status != tito.Issued {
			return t, fmt.Errorf("cannot void a %s ticket", t.Status)
		}
		c.emit(&event.Event{Source: event.SourceCage, Type: event.TicketVoided, Ticket: v, Amount: t.Amount, Actor: user, Reason: reason})
		t, _ = c.Tickets.Get(v)
		return t, nil
	}
}

// SweepExpired expires issued tickets past their expiry on the floor clock.
func SweepExpired() Cmd {
	return func(c *Core) (any, error) {
		due := c.Tickets.ExpireDue(c.FloorNow())
		for _, v := range due {
			t, _ := c.Tickets.Get(v)
			c.emit(&event.Event{Source: event.SourceSweep, Type: event.TicketExpired, Ticket: v, Amount: t.Amount, Actor: "system"})
		}
		return len(due), nil
	}
}

// ---------------------------------------------------------------------------
// Progressives
// ---------------------------------------------------------------------------

// ClaimProgressive grants and resets a bank's jackpot for a machine's hit.
// The machine then reports a jackpot_hit carrying the returned claim.
func ClaimProgressive(bank, machine string) Cmd {
	return func(c *Core) (any, error) {
		b, ok := c.Prog.Get(bank)
		if !ok {
			return nil, progressive.ErrNoBank
		}
		id := fmt.Sprintf("%s-%06d", bank, b.Hits+1)
		cl, err := c.Prog.Grant(bank, machine, id, c.FloorNow())
		if err != nil {
			return nil, err
		}
		c.emit(&event.Event{Source: event.SourceProgressive, Type: event.ProgressiveAward, Bank: bank, Machine: machine, Claim: id, Amount: cl.Amount})
		return cl, nil
	}
}

// UpsertBank adds or reconfigures a progressive bank.
func UpsertBank(b progressive.Bank) Cmd {
	return func(c *Core) (any, error) {
		if err := c.Prog.Upsert(b); err != nil {
			return nil, err
		}
		for _, m := range b.Machines {
			if mm, ok := c.machines[m]; ok {
				mm.Bank = b.ID
				c.touch(mm)
			}
		}
		nb, _ := c.Prog.Get(b.ID)
		return nb, nil
	}
}

// ---------------------------------------------------------------------------
// Players and promotions
// ---------------------------------------------------------------------------

// RedeemComp deducts comp credits from a player (manual, audited).
func RedeemComp(playerID, amount int64, user, reason string) Cmd {
	return func(c *Core) (any, error) {
		if err := c.Club.CanRedeemComp(playerID, amount); err != nil {
			return nil, err
		}
		c.emit(&event.Event{Source: event.SourcePlayers, Type: event.CompRedeemed, PlayerID: playerID, Amount: amount, Actor: user, Reason: reason})
		a, _ := c.Club.Get(playerID)
		return a, nil
	}
}

// SetRules replaces the promotion rule set.
func SetRules(rules []bonus.Rule) Cmd {
	return func(c *Core) (any, error) {
		return nil, c.Bonus.Set(rules)
	}
}

// ---------------------------------------------------------------------------
// Alerts
// ---------------------------------------------------------------------------

// AckAlert acknowledges an alert.
func AckAlert(id int64, user, reason string) Cmd {
	return func(c *Core) (any, error) {
		a, ok := c.Alerts.Ack(id, user, reason, c.Clock())
		if !ok {
			return nil, fmt.Errorf("alert %d is not open", id)
		}
		return a, nil
	}
}

// ResolveAlert resolves an alert. Resolving a tilt also clears the
// machine's tilt state (attendant reset).
func ResolveAlert(id int64, user, note string) Cmd {
	return func(c *Core) (any, error) {
		a, ok := c.Alerts.Resolve(id, user, note, c.Clock())
		if !ok {
			return nil, fmt.Errorf("alert %d is already resolved or unknown", id)
		}
		if a.Type == alerts.Tilt {
			if m, ok := c.machines[a.Machine]; ok {
				m.Tilt = ""
				c.touch(m)
			}
		}
		return a, nil
	}
}

// ---------------------------------------------------------------------------
// Ingestion support
// ---------------------------------------------------------------------------

// RecordGap records a sequence gap reported by ingestion.
func RecordGap(g Gap) Cmd {
	return func(c *Core) (any, error) {
		c.gaps = append(c.gaps, g)
		if !g.Recovered {
			c.raise(alerts.EventGap, alerts.Critical, g.Machine, fmt.Sprintf("events %d..%d lost (not recoverable)", g.From, g.To))
		}
		return nil, nil
	}
}

// SetOnline marks a machine connected or disconnected.
func SetOnline(machine string, online bool) Cmd {
	return func(c *Core) (any, error) {
		m, ok := c.machines[machine]
		if !ok {
			return nil, ErrNotFound
		}
		m.Online = online
		if online {
			m.LastRecv = c.Clock()
			c.resolve(alerts.CommLoss, machine, "reconnected")
		} else {
			c.raise(alerts.CommLoss, alerts.Warning, machine, "machine disconnected")
		}
		c.touch(m)
		return nil, nil
	}
}

// CheckIdle raises long-idle alerts for connected machines with no
// activity within the configured window (host clock).
func CheckIdle() Cmd {
	return func(c *Core) (any, error) {
		now := c.Clock()
		n := 0
		for _, m := range c.machines {
			if !m.Online || m.LastRecv.IsZero() {
				continue
			}
			if now.Sub(m.LastRecv) > c.cfg.IdleAfter && !c.Alerts.IsActive(alerts.Idle, m.ID) {
				c.raise(alerts.Idle, alerts.Info, m.ID, fmt.Sprintf("no activity for %s", now.Sub(m.LastRecv).Round(time.Second)))
				n++
			}
		}
		return n, nil
	}
}

// Reconcile compares a machine's meter report, taken after event asOf,
// with the host meters. Ingestion submits it in stream order, so the host
// meters reflect exactly the events up to asOf.
func Reconcile(machine string, asOf uint64, reported event.Meters, trigger string) Cmd {
	return func(c *Core) (any, error) {
		m, ok := c.machines[machine]
		if !ok {
			return nil, ErrNotFound
		}
		r := Reconciliation{Machine: machine, AsOfSeq: asOf, HostSeq: c.streams[machine], At: c.Clock(),
			Reported: reported, Host: m.Meters.Meters, Trigger: trigger}
		r.Mismatches = accounting.Reconcile(reported, m.Meters.Meters)
		if r.HostSeq != asOf {
			r.Mismatches = append(r.Mismatches, accounting.Mismatch{Meter: "seq", Machine: int64(asOf), Host: int64(r.HostSeq), Delta: int64(asOf) - int64(r.HostSeq)})
		}
		r.OK = len(r.Mismatches) == 0
		c.recons = append(c.recons, r)
		if !r.OK {
			parts := make([]string, 0, len(r.Mismatches))
			for _, x := range r.Mismatches {
				parts = append(parts, x.String())
			}
			c.raise(alerts.MeterMismatch, alerts.Critical, machine, "meter mismatch at seq "+fmt.Sprint(asOf)+": "+strings.Join(parts, "; "))
		}
		return r, nil
	}
}
