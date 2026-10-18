// Package tito implements ticket-in/ticket-out: issue, validation with a
// short-lived redemption hold, redemption, expiry, void, double-redeem
// prevention and an append-only ledger. It holds no locks; the caller (the
// core) serialises access.
package tito

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/noisyloop/cashino/internal/event"
)

// Status of a ticket.
type Status string

const (
	Issued   Status = "issued"
	Pending  Status = "pending" // validated, awaiting the redeeming ticket_in
	Redeemed Status = "redeemed"
	Expired  Status = "expired"
	Voided   Status = "voided"
)

// Errors returned by validation and redemption.
var (
	ErrMalformed       = errors.New("malformed validation number")
	ErrNotFound        = errors.New("ticket not found")
	ErrAlreadyRedeemed = errors.New("ticket already redeemed")
	ErrPending         = errors.New("ticket redemption already in progress")
	ErrExpired         = errors.New("ticket expired")
	ErrVoided          = errors.New("ticket voided")
	ErrAmount          = errors.New("ticket amount mismatch")
	ErrDuplicateIssue  = errors.New("validation number already issued")
)

// Ticket is one printed voucher.
type Ticket struct {
	Validation    string    `json:"validation"`
	Amount        int64     `json:"amount"`
	Status        Status    `json:"status"`
	IssuedBy      string    `json:"issued_by"`
	IssuedAt      time.Time `json:"issued_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	RedeemedBy    string    `json:"redeemed_by,omitempty"` // machine id or cage user
	RedeemedAt    time.Time `json:"redeemed_at,omitempty"`
	pendingBy     string
	pendingExpiry time.Time
}

// LedgerEntry is one row of the ticket ledger. Issue entries are positive
// liability, redeem/expire/void entries are negative.
type LedgerEntry struct {
	Validation string       `json:"validation"`
	Kind       string       `json:"kind"` // issue | redeem | expire | void
	Amount     int64        `json:"amount"`
	Liability  int64        `json:"liability_delta"`
	At         time.Time    `json:"at"`
	Ref        string       `json:"ref"` // machine or user
	EventID    int64        `json:"event_id"`
	Ev         *event.Event `json:"-"` // source event; its ID is known once persisted
}

// Book is the in-memory ticket store.
type Book struct {
	tickets   map[string]*Ticket
	liability int64
	Expiry    time.Duration // validity window for newly issued tickets
	HoldTTL   time.Duration // how long a validation hold lasts
	dirty     map[string]bool
	ledger    []LedgerEntry // entries not yet persisted
}

// NewBook returns an empty book.
func NewBook(expiry time.Duration) *Book {
	return &Book{tickets: map[string]*Ticket{}, Expiry: expiry, HoldTTL: 30 * time.Second, dirty: map[string]bool{}}
}

// Load inserts a persisted ticket (startup snapshot load).
func (b *Book) Load(t Ticket) {
	tt := t
	b.tickets[t.Validation] = &tt
	if t.Status == Issued || t.Status == Pending {
		b.liability += t.Amount
	}
}

// Get returns a copy of a ticket.
func (b *Book) Get(v string) (Ticket, bool) {
	t, ok := b.tickets[v]
	if !ok {
		return Ticket{}, false
	}
	return *t, true
}

// Liability is the face value of all outstanding (issued, unredeemed,
// unexpired) tickets.
func (b *Book) Liability() int64 { return b.liability }

// Outstanding returns all tickets that are still liabilities.
func (b *Book) Outstanding() []Ticket {
	var out []Ticket
	for _, t := range b.tickets {
		if t.Status == Issued || t.Status == Pending {
			out = append(out, *t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IssuedAt.Before(out[j].IssuedAt) })
	return out
}

func (b *Book) mark(v string, e LedgerEntry) {
	b.dirty[v] = true
	b.ledger = append(b.ledger, e)
}

// Issue records a ticket_out. Re-issuing an existing validation number is an
// error (the event is still kept in the log; the caller raises an alert).
func (b *Book) Issue(e *event.Event) error {
	if !event.ValidTicket(e.Ticket) {
		return ErrMalformed
	}
	if _, ok := b.tickets[e.Ticket]; ok {
		return ErrDuplicateIssue
	}
	exp := e.Expires
	if exp.IsZero() {
		exp = e.TS.Add(b.Expiry)
	}
	t := &Ticket{Validation: e.Ticket, Amount: e.Amount, Status: Issued, IssuedBy: e.Source, IssuedAt: e.TS, ExpiresAt: exp}
	b.tickets[e.Ticket] = t
	b.liability += t.Amount
	b.mark(t.Validation, LedgerEntry{Validation: t.Validation, Kind: "issue", Amount: t.Amount, Liability: t.Amount, At: e.TS, Ref: e.Source, EventID: e.ID, Ev: e})
	return nil
}

// check returns why ticket v cannot be redeemed now, or nil.
func (b *Book) check(v string, now time.Time, by string) (*Ticket, error) {
	if !event.ValidTicket(v) {
		return nil, ErrMalformed
	}
	t, ok := b.tickets[v]
	if !ok {
		return nil, ErrNotFound
	}
	switch t.Status {
	case Redeemed:
		return t, ErrAlreadyRedeemed
	case Voided:
		return t, ErrVoided
	case Expired:
		return t, ErrExpired
	case Pending:
		if now.Before(t.pendingExpiry) && t.pendingBy != by {
			return t, ErrPending
		}
	}
	if !now.Before(t.ExpiresAt) {
		return t, ErrExpired
	}
	return t, nil
}

// Validate is the host side of a machine's "may I accept this ticket?"
// request. On success the ticket is placed on a short redemption hold for
// the requesting machine so that no other machine or the cage can redeem
// it concurrently. The hold is not persisted: if the host restarts the
// ticket simply reverts to issued.
func (b *Book) Validate(v, machine string, now time.Time) (Ticket, error) {
	t, err := b.check(v, now, machine)
	if err != nil {
		if t != nil {
			return *t, err
		}
		return Ticket{}, err
	}
	t.Status = Pending
	t.pendingBy = machine
	t.pendingExpiry = now.Add(b.HoldTTL)
	return *t, nil
}

// Release drops a validation hold (e.g. the machine rejected the ticket).
func (b *Book) Release(v, by string) {
	if t, ok := b.tickets[v]; ok && t.Status == Pending && t.pendingBy == by {
		t.Status = Issued
	}
}

// Reserve is the cage-side equivalent of Validate: it checks the ticket and
// holds it while the redemption event is written.
func (b *Book) Reserve(v, by string, now time.Time) (Ticket, error) {
	return b.Validate(v, by, now)
}

// Redeem applies a ticket_in or ticket_redeemed event. It returns an error
// when the redemption should not have happened (double redeem, unknown or
// expired ticket, amount mismatch). For a ticket_in event the machine has
// already paid the credits, so the caller keeps the event and raises an
// alert; the book never moves a ticket out of a terminal state.
func (b *Book) Redeem(e *event.Event, by string) error {
	t, ok := b.tickets[e.Ticket]
	if !ok {
		return ErrNotFound
	}
	switch t.Status {
	case Redeemed:
		return ErrAlreadyRedeemed
	case Voided:
		return ErrVoided
	case Expired:
		return ErrExpired
	}
	if e.Amount != t.Amount {
		return fmt.Errorf("%w: ticket %d, presented %d", ErrAmount, t.Amount, e.Amount)
	}
	t.Status = Redeemed
	t.RedeemedBy = by
	t.RedeemedAt = e.TS
	t.pendingBy = ""
	b.liability -= t.Amount
	b.mark(t.Validation, LedgerEntry{Validation: t.Validation, Kind: "redeem", Amount: t.Amount, Liability: -t.Amount, At: e.TS, Ref: by, EventID: e.ID, Ev: e})
	return nil
}

// ExpireDue returns validation numbers of issued tickets past expiry at now.
// The caller writes a ticket_expired event for each; Expire applies it.
func (b *Book) ExpireDue(now time.Time) []string {
	var out []string
	for v, t := range b.tickets {
		if t.Status == Issued && !now.Before(t.ExpiresAt) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// Expire applies a ticket_expired event.
func (b *Book) Expire(e *event.Event) error {
	return b.close(e, Expired, "expire")
}

// Void applies a ticket_voided event (manual, audited).
func (b *Book) Void(e *event.Event) error {
	return b.close(e, Voided, "void")
}

func (b *Book) close(e *event.Event, s Status, kind string) error {
	t, ok := b.tickets[e.Ticket]
	if !ok {
		return ErrNotFound
	}
	if t.Status != Issued && t.Status != Pending {
		return fmt.Errorf("cannot %s a %s ticket", kind, t.Status)
	}
	t.Status = s
	b.liability -= t.Amount
	ref := e.Actor
	if ref == "" {
		ref = e.Source
	}
	b.mark(t.Validation, LedgerEntry{Validation: t.Validation, Kind: kind, Amount: t.Amount, Liability: -t.Amount, At: e.TS, Ref: ref, EventID: e.ID, Ev: e})
	return nil
}

// Drain returns and clears the tickets and ledger entries changed since the
// last call, for persistence.
func (b *Book) Drain() ([]Ticket, []LedgerEntry) {
	ts := make([]Ticket, 0, len(b.dirty))
	for v := range b.dirty {
		t := *b.tickets[v]
		if t.Status == Pending {
			t.Status = Issued // holds are never persisted
		}
		ts = append(ts, t)
	}
	l := b.ledger
	b.dirty = map[string]bool{}
	b.ledger = nil
	return ts, l
}

// NewValidation builds an 18-digit validation number from a 17-digit
// payload by appending its Luhn check digit.
func NewValidation(payload17 string) string {
	return payload17 + string(event.LuhnDigit(payload17))
}

// Aging buckets outstanding liability by age at now.
func (b *Book) Aging(now time.Time) map[string]int64 {
	out := map[string]int64{"0-1d": 0, "1-7d": 0, "7-30d": 0, "30d+": 0}
	for _, t := range b.tickets {
		if t.Status != Issued && t.Status != Pending {
			continue
		}
		age := now.Sub(t.IssuedAt)
		switch {
		case age < 24*time.Hour:
			out["0-1d"] += t.Amount
		case age < 7*24*time.Hour:
			out["1-7d"] += t.Amount
		case age < 30*24*time.Hour:
			out["7-30d"] += t.Amount
		default:
			out["30d+"] += t.Amount
		}
	}
	return out
}

// HeldBy returns who holds the validation hold on v ("" if none).
func (b *Book) HeldBy(v string) string {
	if t, ok := b.tickets[v]; ok && t.Status == Pending {
		return t.pendingBy
	}
	return ""
}
