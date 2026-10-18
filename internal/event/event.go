// Package event defines the cashino machine event model and wire protocol.
//
// The model is SAS-inspired (sequenced, per-machine, credit-denominated
// events) but is an original design: field names, message kinds and
// semantics are documented in docs/event-schema.md.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Type is the kind of a machine or system event.
type Type string

// Machine-originated event types.
const (
	GameStart  Type = "game_start"
	GameEnd    Type = "game_end"
	BillIn     Type = "bill_in"
	TicketIn   Type = "ticket_in"
	TicketOut  Type = "ticket_out"
	Handpay    Type = "handpay"
	Tilt       Type = "tilt"
	DoorOpen   Type = "door_open"
	DoorClose  Type = "door_close"
	CardIn     Type = "card_in"
	CardOut    Type = "card_out"
	JackpotHit Type = "jackpot_hit"
)

// System-originated event types. These are written by the host itself
// (cage, progressive controller, bonusing, sweeps) into the same log so that
// every state change is replayable from one ordered stream.
const (
	TicketRedeemed   Type = "ticket_redeemed"   // redeemed at the cage / kiosk
	TicketExpired    Type = "ticket_expired"    // expiry sweep
	TicketVoided     Type = "ticket_voided"     // manual void (audited)
	ProgressiveAward Type = "progressive_award" // controller granted a claim and reset
	BonusAwarded     Type = "bonus_awarded"     // promotion engine awarded free play
	CompRedeemed     Type = "comp_redeemed"     // attendant redeemed comp balance
)

// Sources used for host-originated events.
const (
	SourceCage        = "sys:cage"
	SourceProgressive = "sys:progressive"
	SourceBonus       = "sys:bonus"
	SourceSweep       = "sys:sweep"
	SourcePlayers     = "sys:players"
)

var machineTypes = map[Type]bool{
	GameStart: true, GameEnd: true, BillIn: true, TicketIn: true, TicketOut: true,
	Handpay: true, Tilt: true, DoorOpen: true, DoorClose: true, CardIn: true,
	CardOut: true, JackpotHit: true,
}

var systemTypes = map[Type]bool{
	TicketRedeemed: true, TicketExpired: true, TicketVoided: true,
	ProgressiveAward: true, BonusAwarded: true, CompRedeemed: true,
}

// IsMachineType reports whether t may be sent by a gaming machine.
func IsMachineType(t Type) bool { return machineTypes[t] }

// IsSystemType reports whether t is a host-originated type.
func IsSystemType(t Type) bool { return systemTypes[t] }

// MachineTypes returns all machine event types (stable order).
func MachineTypes() []Type {
	return []Type{GameStart, GameEnd, BillIn, TicketIn, TicketOut, Handpay, Tilt,
		DoorOpen, DoorClose, CardIn, CardOut, JackpotHit}
}

// Event is one entry in the append-only event log.
//
// All amounts are integer credits. One credit is the floor's base accounting
// unit (1 credit = $0.01 simulated). Optional fields are zero when unused.
type Event struct {
	ID        int64     `json:"id,omitempty"` // global log position, assigned on write
	Source    string    `json:"machine_id"`   // machine id, or "sys:*" for host events
	Seq       uint64    `json:"seq"`          // per-source monotonically increasing, starts at 1
	Type      Type      `json:"type"`
	TS        time.Time `json:"ts"` // event time as reported by the source
	Recv      time.Time `json:"recv,omitempty"`
	Amount    int64     `json:"amount,omitempty"`    // bill/ticket/handpay/jackpot/bonus credits
	Wager     int64     `json:"wager,omitempty"`     // game_start / game_end
	Win       int64     `json:"win,omitempty"`       // game_end credits won to the credit meter
	Ticket    string    `json:"ticket,omitempty"`    // 18-digit validation number
	Card      string    `json:"card,omitempty"`      // player card number
	Code      string    `json:"code,omitempty"`      // tilt code, door name, handpay reason
	Bank      string    `json:"bank,omitempty"`      // progressive bank id
	Claim     string    `json:"claim,omitempty"`     // progressive claim id
	Machine   string    `json:"machine,omitempty"`   // for system events: related machine
	Actor     string    `json:"actor,omitempty"`     // for system events: user who acted
	Reason    string    `json:"reason,omitempty"`    // for manual system events
	PlayerID  int64     `json:"player_id,omitempty"` // for system events
	Expires   time.Time `json:"expires,omitempty"`   // ticket_out expiry (set by host)
	GameID    string    `json:"game_id,omitempty"`   // free-form round id from the game
	BonusRule string    `json:"bonus_rule,omitempty"`
}

// IsSystem reports whether the event was written by the host.
func (e *Event) IsSystem() bool { return len(e.Source) > 4 && e.Source[:4] == "sys:" }

// Limits used by validation. They are deliberately generous; they exist to
// reject garbage, not to enforce game rules.
const (
	MaxAmount    = 100_000_000 // $1,000,000.00
	MaxWager     = 100_000     // $1,000.00 per game
	MaxFutureLag = 10 * time.Minute
)

var (
	machineIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$`)
	ticketRe    = regexp.MustCompile(`^[0-9]{18}$`)
	cardRe      = regexp.MustCompile(`^[0-9]{6,20}$`)
	codeRe      = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)
	bankRe      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)
	minTS       = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
)

// ValidMachineID reports whether id is an acceptable machine identifier.
func ValidMachineID(id string) bool { return machineIDRe.MatchString(id) }

// ValidTicket reports whether v is a well-formed validation number with a
// correct check digit.
func ValidTicket(v string) bool { return ticketRe.MatchString(v) && LuhnValid(v) }

// ValidationError describes why an event was rejected.
type ValidationError struct{ Msg string }

func (v *ValidationError) Error() string { return "invalid event: " + v.Msg }

func bad(format string, a ...any) error { return &ValidationError{fmt.Sprintf(format, a...)} }

// Validate checks a machine-originated event for structural correctness.
// now is the host clock, used to reject events from the far future.
func (e *Event) Validate(now time.Time) error {
	if !ValidMachineID(e.Source) {
		return bad("machine_id %q is malformed", e.Source)
	}
	if !IsMachineType(e.Type) {
		return bad("unknown event type %q", e.Type)
	}
	if e.Seq == 0 {
		return bad("seq must be >= 1")
	}
	if e.TS.IsZero() || e.TS.Before(minTS) {
		return bad("ts missing or too old")
	}
	if e.TS.After(now.Add(MaxFutureLag)) {
		return bad("ts is in the future")
	}
	if e.Amount < 0 || e.Wager < 0 || e.Win < 0 {
		return bad("amounts must be non-negative")
	}
	if e.Amount > MaxAmount || e.Win > MaxAmount {
		return bad("amount exceeds limit")
	}
	if e.Code != "" && !codeRe.MatchString(e.Code) {
		return bad("code is malformed")
	}
	if e.Bank != "" && !bankRe.MatchString(e.Bank) {
		return bad("bank is malformed")
	}
	if len(e.GameID) > 64 || len(e.Claim) > 64 {
		return bad("id field too long")
	}
	switch e.Type {
	case GameStart:
		if e.Wager <= 0 || e.Wager > MaxWager {
			return bad("game_start requires 0 < wager <= %d", MaxWager)
		}
	case GameEnd:
		if e.Wager <= 0 || e.Wager > MaxWager {
			return bad("game_end requires 0 < wager <= %d", MaxWager)
		}
	case BillIn:
		if e.Amount <= 0 {
			return bad("bill_in requires amount > 0")
		}
	case TicketIn, TicketOut:
		if e.Amount <= 0 {
			return bad("%s requires amount > 0", e.Type)
		}
		if !ValidTicket(e.Ticket) {
			return bad("%s requires a valid 18-digit validation number", e.Type)
		}
	case Handpay:
		if e.Amount <= 0 {
			return bad("handpay requires amount > 0")
		}
	case JackpotHit:
		if e.Amount <= 0 {
			return bad("jackpot_hit requires amount > 0")
		}
	case Tilt:
		if e.Code == "" {
			return bad("tilt requires a code")
		}
	case CardIn, CardOut:
		if !cardRe.MatchString(e.Card) {
			return bad("%s requires a numeric card", e.Type)
		}
	}
	return nil
}

// LuhnValid checks the Luhn (mod 10) check digit of a numeric string.
func LuhnValid(s string) bool {
	if len(s) < 2 {
		return false
	}
	return LuhnDigit(s[:len(s)-1]) == s[len(s)-1]
}

// LuhnDigit returns the Luhn check digit to append to the numeric string s.
func LuhnDigit(s string) byte {
	sum := 0
	double := true // rightmost digit of payload is doubled
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return byte('0' + (10-sum%10)%10)
}

// ---------------------------------------------------------------------------
// Wire protocol
// ---------------------------------------------------------------------------

// Message kinds exchanged between a machine and the host. Every frame is a
// single JSON object (newline-delimited over TCP, one per message over WS).
const (
	KindHello          = "hello"           // machine -> host: identify + authenticate
	KindWelcome        = "welcome"         // host -> machine: accepted, last durable seq
	KindEvent          = "event"           // machine -> host
	KindAck            = "ack"             // host -> machine: cumulative durable seq
	KindNak            = "nak"             // host -> machine: event rejected (validation)
	KindResend         = "resend"          // host -> machine: please resend [from,to]
	KindPoll           = "poll"            // host -> machine: report meters
	KindMeters         = "meters"          // machine -> host: meter report
	KindTicketValidate = "ticket_validate" // machine -> host: may I accept this ticket?
	KindTicketResult   = "ticket_result"   // host -> machine
	KindProgClaim      = "progressive_claim"
	KindProgResult     = "progressive_result"
	KindProgUpdate     = "progressive_update" // host -> machine: current value broadcast
	KindError          = "error"
	KindPing           = "ping"
	KindPong           = "pong"
)

// Hello is sent by a machine when it connects.
type Hello struct {
	Machine string  `json:"machine_id"`
	Token   string  `json:"token"` // hex HMAC-SHA256(machine key, machine_id)
	Theme   string  `json:"theme,omitempty"`
	Denom   int64   `json:"denom,omitempty"` // credits per machine credit
	RTP     float64 `json:"rtp,omitempty"`   // configured return to player, 0..1
	Bank    string  `json:"bank,omitempty"`  // progressive bank membership
	Zone    string  `json:"zone,omitempty"`
}

// Meters is the machine's own (soft) meter set, reported on poll.
type Meters struct {
	CoinIn         int64 `json:"coin_in"`
	CoinOut        int64 `json:"coin_out"`
	Jackpot        int64 `json:"jackpot"`
	Handpay        int64 `json:"handpay"`
	GamesPlayed    int64 `json:"games_played"`
	GamesWon       int64 `json:"games_won"`
	BillsIn        int64 `json:"bills_in"`
	TicketsIn      int64 `json:"tickets_in"`
	TicketsOut     int64 `json:"tickets_out"`
	TicketInCount  int64 `json:"ticket_in_count"`
	TicketOutCount int64 `json:"ticket_out_count"`
	Credits        int64 `json:"credits"` // current credit meter
}

// Msg is the envelope for every frame. Only fields relevant to Kind are set.
type Msg struct {
	Kind    string  `json:"kind"`
	ReqID   string  `json:"req_id,omitempty"`
	Hello   *Hello  `json:"hello,omitempty"`
	Event   *Event  `json:"event,omitempty"`
	Seq     uint64  `json:"seq,omitempty"`  // ack / welcome / meters as_of
	From    uint64  `json:"from,omitempty"` // resend
	To      uint64  `json:"to,omitempty"`
	Meters  *Meters `json:"meters,omitempty"`
	Ticket  string  `json:"ticket,omitempty"`
	Amount  int64   `json:"amount,omitempty"`
	OK      bool    `json:"ok,omitempty"`
	Error   string  `json:"error,omitempty"`
	Bank    string  `json:"bank,omitempty"`
	Claim   string  `json:"claim,omitempty"`
	Machine string  `json:"machine_id,omitempty"`
}

// ErrBadFrame is returned for frames that cannot be decoded.
var ErrBadFrame = errors.New("malformed frame")

// MaxFrameBytes bounds a single inbound frame.
const MaxFrameBytes = 16 * 1024

// Decode parses one frame.
func Decode(b []byte) (*Msg, error) {
	if len(b) > MaxFrameBytes {
		return nil, ErrBadFrame
	}
	var m Msg
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, ErrBadFrame
	}
	if m.Kind == "" {
		return nil, ErrBadFrame
	}
	return &m, nil
}
