// Package accounting implements per-machine slot meters derived from the
// event log, hold calculations and meter reconciliation.
//
// Meter semantics (all in credits, 1 credit = $0.01 simulated):
//
//	CoinIn      total wagered                       (game_start.wager)
//	CoinOut     game wins paid to the credit meter  (game_end.win)
//	Jackpot     jackpot / progressive awards        (jackpot_hit.amount)
//	Handpay     credits paid by an attendant        (handpay.amount)
//	BillsIn     currency accepted                   (bill_in.amount)
//	TicketsIn   ticket value accepted               (ticket_in.amount)
//	TicketsOut  ticket value printed                (ticket_out.amount)
//
// Derived:
//
//	NetWin  (casino win) = CoinIn - CoinOut - Jackpot
//	Hold %               = NetWin / CoinIn
//	Theo win             = sum over games of wager * (1 - RTP)
//	Credits (meter)      = BillsIn + TicketsIn + CoinOut + Jackpot
//	                       - CoinIn - TicketsOut - Handpay
package accounting

import (
	"fmt"
	"math"

	"github.com/noisyloop/cashino/internal/event"
)

// Meters is the host-side (event-derived) meter set for one machine.
type Meters struct {
	event.Meters
	TheoMilli int64 `json:"theo_milli"` // theoretical win, in 1/1000 credit
}

// Apply folds one machine event into the meters. rtp is the machine's
// configured return to player, used for the theoretical win. It returns
// false for events that do not affect meters.
func (m *Meters) Apply(e *event.Event, rtp float64) bool {
	switch e.Type {
	case event.GameStart:
		m.CoinIn += e.Wager
		m.GamesPlayed++
		m.TheoMilli += TheoMilli(e.Wager, rtp)
		m.Credits -= e.Wager
	case event.GameEnd:
		if e.Win > 0 {
			m.CoinOut += e.Win
			m.GamesWon++
			m.Credits += e.Win
		}
	case event.JackpotHit:
		m.Jackpot += e.Amount
		m.Credits += e.Amount
	case event.Handpay:
		m.Handpay += e.Amount
		m.Credits -= e.Amount
	case event.BillIn:
		m.BillsIn += e.Amount
		m.Credits += e.Amount
	case event.TicketIn:
		m.TicketsIn += e.Amount
		m.TicketInCount++
		m.Credits += e.Amount
	case event.TicketOut:
		m.TicketsOut += e.Amount
		m.TicketOutCount++
		m.Credits -= e.Amount
	default:
		return false
	}
	return true
}

// TheoMilli is the theoretical casino win of one wager, in milli-credits.
func TheoMilli(wager int64, rtp float64) int64 {
	return int64(math.Round(float64(wager) * (1 - rtp) * 1000))
}

// NetWin is the actual casino win.
func (m *Meters) NetWin() int64 { return m.CoinIn - m.CoinOut - m.Jackpot }

// Theo returns the theoretical win in whole credits.
func (m *Meters) Theo() float64 { return float64(m.TheoMilli) / 1000 }

// Hold returns actual hold as a fraction (0.08 = 8%). Zero coin-in gives 0.
func Hold(coinIn, netWin int64) float64 {
	if coinIn == 0 {
		return 0
	}
	return float64(netWin) / float64(coinIn)
}

// HoldPct is the machine's actual hold fraction.
func (m *Meters) HoldPct() float64 { return Hold(m.CoinIn, m.NetWin()) }

// TheoHoldPct is the machine's theoretical hold fraction.
func (m *Meters) TheoHoldPct() float64 {
	if m.CoinIn == 0 {
		return 0
	}
	return m.Theo() / float64(m.CoinIn)
}

// Drop is the cash and ticket value inserted.
func (m *Meters) Drop() int64 { return m.BillsIn + m.TicketsIn }

// BalanceCheck verifies the credit-meter identity. A non-nil error means the
// event stream is internally inconsistent (e.g. events were lost).
func (m *Meters) BalanceCheck() error {
	exp := m.BillsIn + m.TicketsIn + m.CoinOut + m.Jackpot - m.CoinIn - m.TicketsOut - m.Handpay
	if exp != m.Credits {
		return fmt.Errorf("credit meter %d != derived balance %d", m.Credits, exp)
	}
	if m.Credits < 0 {
		return fmt.Errorf("negative credit meter %d", m.Credits)
	}
	return nil
}

// Add returns the sum of two meter sets (for bank / floor totals).
func Add(a, b Meters) Meters {
	return Meters{Meters: event.Meters{
		CoinIn: a.CoinIn + b.CoinIn, CoinOut: a.CoinOut + b.CoinOut,
		Jackpot: a.Jackpot + b.Jackpot, Handpay: a.Handpay + b.Handpay,
		GamesPlayed: a.GamesPlayed + b.GamesPlayed, GamesWon: a.GamesWon + b.GamesWon,
		BillsIn: a.BillsIn + b.BillsIn, TicketsIn: a.TicketsIn + b.TicketsIn,
		TicketsOut: a.TicketsOut + b.TicketsOut, TicketInCount: a.TicketInCount + b.TicketInCount,
		TicketOutCount: a.TicketOutCount + b.TicketOutCount, Credits: a.Credits + b.Credits,
	}, TheoMilli: a.TheoMilli + b.TheoMilli}
}

// Mismatch is one meter that disagrees between the machine and the host.
type Mismatch struct {
	Meter   string `json:"meter"`
	Machine int64  `json:"machine"`
	Host    int64  `json:"host"`
	Delta   int64  `json:"delta"` // machine - host
}

func (m Mismatch) String() string {
	return fmt.Sprintf("%s machine=%d host=%d delta=%+d", m.Meter, m.Machine, m.Host, m.Delta)
}

// Reconcile compares a machine-reported meter set to the host's event-derived
// meters at the same sequence point. It returns every disagreeing meter;
// an empty result means the machine and the event log agree exactly.
func Reconcile(machine, host event.Meters) []Mismatch {
	pairs := []struct {
		name string
		a, b int64
	}{
		{"coin_in", machine.CoinIn, host.CoinIn},
		{"coin_out", machine.CoinOut, host.CoinOut},
		{"jackpot", machine.Jackpot, host.Jackpot},
		{"handpay", machine.Handpay, host.Handpay},
		{"games_played", machine.GamesPlayed, host.GamesPlayed},
		{"games_won", machine.GamesWon, host.GamesWon},
		{"bills_in", machine.BillsIn, host.BillsIn},
		{"tickets_in", machine.TicketsIn, host.TicketsIn},
		{"tickets_out", machine.TicketsOut, host.TicketsOut},
		{"ticket_in_count", machine.TicketInCount, host.TicketInCount},
		{"ticket_out_count", machine.TicketOutCount, host.TicketOutCount},
		{"credits", machine.Credits, host.Credits},
	}
	var out []Mismatch
	for _, p := range pairs {
		if p.a != p.b {
			out = append(out, Mismatch{Meter: p.name, Machine: p.a, Host: p.b, Delta: p.a - p.b})
		}
	}
	return out
}
