package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Near-miss free bids
//
// When an auction's result is locked (the winner paid their bid), the closest losers get one
// free bid each:
//
//   - runners-up: the next lowest unique bids above the winning bid (2nd, 3rd, ... lowest unique);
//   - duplicate knockouts (optional): bidders whose value was below the winning bid but lost
//     only because someone else picked the same number.
//
// A free bid is one free ticket. Under pay-on-win a ticket costs only the entrance fee and only
// the winner pays their bid amount, so a free bid waives the entrance fee of one ticket on the
// customer's next checkout, on any auction. If that ticket wins, the winner still pays the bid
// amount like everyone else: the prize is never given away, only the chance to play.
//
// Each customer gets at most one free bid per auction, enforced by a unique index, so settling an
// auction twice never credits anyone twice.
//
// Spending: a checkout records how many free bids it used on its payments row (free_bids_used).
// The balance is grants minus the free bids of pending or successful checkouts, so a failed,
// expired or refunded checkout gives its free bids back without any extra bookkeeping.

// Near-miss reasons stored on a grant.
const (
	NearMissRunnerUp          = "runner_up"
	NearMissDuplicateKnockout = "duplicate_knockout"
	FreeBidStaffGrant         = "staff_grant"
)

// Defaults and limits for the staff-editable near-miss rules.
const (
	DefaultNearMissRunnerUps     = 4 // 2nd to 5th lowest unique bid
	DefaultNearMissMaxDuplicates = 10
	MaxNearMissRunnerUps         = 50
	MaxNearMissDuplicates        = 500
)

// NearMissSettings are the staff-editable rules. There is one row.
type NearMissSettings struct {
	ID int `gorm:"primaryKey" json:"-"`
	// Enabled turns automatic near-miss credits on or off.
	Enabled bool `gorm:"not null;default:true" json:"enabled"`
	// RunnerUps is how many unique bids above the winner get a free bid (4 = 2nd to 5th lowest).
	RunnerUps int `gorm:"not null" json:"runnerUps"`
	// IncludeDuplicates also credits bidders who bid below the winner but tied with someone else.
	IncludeDuplicates bool `gorm:"not null;default:false" json:"includeDuplicates"`
	// MaxDuplicates caps duplicate knockout credits per auction (closest to the winning bid first).
	MaxDuplicates int        `gorm:"not null" json:"maxDuplicates"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	UpdatedBy     *uuid.UUID `gorm:"type:uuid" json:"updatedBy,omitempty"`
}

func (NearMissSettings) TableName() string { return "near_miss_settings" }

// DefaultNearMissSettings are used until staff change them.
func DefaultNearMissSettings() NearMissSettings {
	return NearMissSettings{
		ID:                1,
		Enabled:           true,
		RunnerUps:         DefaultNearMissRunnerUps,
		IncludeDuplicates: false,
		MaxDuplicates:     DefaultNearMissMaxDuplicates,
	}
}

// Validate checks the ranges staff may set.
func (s NearMissSettings) Validate() error {
	if s.RunnerUps < 0 || s.RunnerUps > MaxNearMissRunnerUps {
		return NewAppError(400, fmt.Sprintf("Runner-ups must be between 0 and %d", MaxNearMissRunnerUps), ErrInvalidInput)
	}
	if s.MaxDuplicates < 0 || s.MaxDuplicates > MaxNearMissDuplicates {
		return NewAppError(400, fmt.Sprintf("Duplicate knockouts must be between 0 and %d", MaxNearMissDuplicates), ErrInvalidInput)
	}
	return nil
}

// FreeBidGrant is one free bid given to a customer.
type FreeBidGrant struct {
	ID            uuid.UUID  `gorm:"type:uuid;primaryKey;default:gen_random_uuid()" json:"id"`
	ParticipantID string     `gorm:"type:uuid;not null" json:"participantId"`
	AuctionID     *uuid.UUID `gorm:"type:uuid" json:"auctionId,omitempty"`
	Reason        string     `gorm:"type:varchar(30);not null" json:"reason"`
	// Rank is the bid's place among the unique bids (2 = second lowest unique) for runner-ups.
	Rank      int        `gorm:"not null;default:0" json:"rank,omitempty"`
	BidValue  *Money     `gorm:"type:decimal(10,2)" json:"bidValue,omitempty"`
	Note      string     `gorm:"type:varchar(255);not null;default:''" json:"note,omitempty"`
	CreatedBy *uuid.UUID `gorm:"type:uuid" json:"createdBy,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`

	// Filled for API responses.
	AuctionTitle    string `gorm:"->;-:migration" json:"auctionTitle,omitempty"`
	ParticipantName string `gorm:"->;-:migration" json:"participantName,omitempty"`
}

func (FreeBidGrant) TableName() string { return "free_bid_grants" }

// NearMissBid is the slice of a bid that near-miss selection needs.
type NearMissBid struct {
	ParticipantID string
	BidValue      Money
}

// NearMissPick is one customer chosen for a free bid.
type NearMissPick struct {
	ParticipantID string
	Reason        string
	Rank          int // place among unique bids (runner-ups only)
	BidValue      Money
}

// SelectNearMisses picks who gets a free bid once the winner is locked.
//
// winnerValue is the locked winning bid; winnerID its owner. forfeited are participants who were
// offered the prize and let it lapse: they never get a near-miss credit for this auction. Each
// participant appears at most once (runner-up beats duplicate knockout). Runner-ups are ranked by
// position among all unique bids, so the winner is rank 1 when it is the lowest unique bid.
func SelectNearMisses(bids []NearMissBid, winnerID string, winnerValue Money, forfeited []string, s NearMissSettings) []NearMissPick {
	if !s.Enabled {
		return nil
	}
	skip := map[string]bool{strings.ToLower(winnerID): true}
	for _, f := range forfeited {
		skip[strings.ToLower(f)] = true
	}

	type group struct {
		value   Money
		bidders []string
	}
	byValue := map[string]*group{}
	order := []*group{}
	for _, b := range bids {
		k := b.BidValue.String()
		g, ok := byValue[k]
		if !ok {
			g = &group{value: b.BidValue}
			byValue[k] = g
			order = append(order, g)
		}
		g.bidders = append(g.bidders, b.ParticipantID)
	}
	sort.Slice(order, func(i, j int) bool { return order[i].value.LessThan(order[j].value) })

	picked := map[string]bool{}
	var out []NearMissPick

	// Runner-ups: unique bids above the winning bid, in order.
	if s.RunnerUps > 0 {
		rank := 0
		taken := 0
		for _, g := range order {
			if len(g.bidders) != 1 {
				continue
			}
			rank++
			if !winnerValue.LessThan(g.value) {
				continue // the winner itself, or a lapsed winner below it
			}
			pid := g.bidders[0]
			key := strings.ToLower(pid)
			if skip[key] || picked[key] {
				continue
			}
			picked[key] = true
			out = append(out, NearMissPick{ParticipantID: pid, Reason: NearMissRunnerUp, Rank: rank, BidValue: g.value})
			taken++
			if taken >= s.RunnerUps {
				break
			}
		}
	}

	// Duplicate knockouts: tied values below the winning bid, closest to it first.
	if s.IncludeDuplicates && s.MaxDuplicates > 0 {
		taken := 0
		for i := len(order) - 1; i >= 0 && taken < s.MaxDuplicates; i-- {
			g := order[i]
			if len(g.bidders) < 2 || !g.value.LessThan(winnerValue) {
				continue
			}
			bidders := append([]string(nil), g.bidders...)
			sort.Strings(bidders) // deterministic when a cap cuts a tie group
			for _, pid := range bidders {
				key := strings.ToLower(pid)
				if skip[key] || picked[key] {
					continue
				}
				picked[key] = true
				out = append(out, NearMissPick{ParticipantID: pid, Reason: NearMissDuplicateKnockout, BidValue: g.value})
				taken++
				if taken >= s.MaxDuplicates {
					break
				}
			}
		}
	}
	return out
}

// CheckoutChargeWithFreeBids is CheckoutCharge with free bids applied: after the optional
// free-entrance waiver on the first ticket, freeBids more tickets cost nothing.
func CheckoutChargeWithFreeBids(bids []Money, entranceFee Money, waiveFirst bool, freeBids int) Money {
	paid := len(bids)
	if waiveFirst && paid > 0 {
		paid--
	}
	if freeBids > paid {
		freeBids = paid
	}
	if freeBids > 0 {
		paid -= freeBids
	}
	total := Zero()
	for i := 0; i < paid; i++ {
		total = total.Add(entranceFee)
	}
	return total
}

// FreeBidsApplicable is how many free bids a checkout of ticketCount tickets can use.
func FreeBidsApplicable(balance, ticketCount int, waiveFirst bool) int {
	payable := ticketCount
	if waiveFirst && payable > 0 {
		payable--
	}
	if balance < payable {
		return balance
	}
	if payable < 0 {
		return 0
	}
	return payable
}

// ErrFreeBidsUnavailable is returned when a customer expected to use free bids that are no
// longer there (used by another checkout meanwhile). 409 so the client refreshes the quote.
var ErrFreeBidsUnavailable = NewAppError(
	409,
	"Your free bids changed since you opened this page. Review the updated total and try again.",
	nil,
)

// FreeBidBalance is a customer's free bids for the API.
type FreeBidBalance struct {
	Available int            `json:"available"`
	Earned    int            `json:"earned"`
	Used      int            `json:"used"`
	Recent    []FreeBidGrant `json:"recent"`
}
