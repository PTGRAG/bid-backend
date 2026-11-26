package domain

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func intp(n int) *int { return &n }

func TestReferralDefaultsAreValid(t *testing.T) {
	for _, kind := range []string{ReferralKindAgent, ReferralKindInfluencer} {
		p := NewDefaultReferralProgram(kind)
		if err := p.Validate(); err != nil {
			t.Fatalf("%s defaults invalid: %v", kind, err)
		}
		if !p.TopTierAmount().LessThan(p.NetMarginPerBid) {
			t.Fatalf("%s top tier must stay below the net margin", kind)
		}
	}
}

func TestReferralEarnings(t *testing.T) {
	p := NewDefaultReferralProgram(ReferralKindAgent) // 10 threshold; 11-25: 5, 26-50: 10, 51+: 20
	cases := []struct {
		qualified int
		earned    float64
		unlocked  bool
		remaining int
		rate      float64
		nextFrom  int // 0 = no next tier
		nextNeed  int
	}{
		{0, 0, false, 10, 0, 11, 10},
		{9, 0, false, 1, 0, 11, 1},
		{10, 0, true, 0, 5, 26, 15},
		{11, 5, true, 0, 5, 26, 14},
		{25, 75, true, 0, 10, 51, 25},
		{26, 85, true, 0, 10, 51, 24},
		{50, 325, true, 0, 20, 0, 0},
		{60, 525, true, 0, 20, 0, 0},
	}
	for _, c := range cases {
		e := p.Earnings(c.qualified)
		if e.Earned.Float64() != c.earned || e.Unlocked != c.unlocked || e.Remaining != c.remaining || e.CurrentRate.Float64() != c.rate {
			t.Errorf("q=%d: got earned=%v unlocked=%v remaining=%d rate=%v", c.qualified, e.Earned, e.Unlocked, e.Remaining, e.CurrentRate)
		}
		switch {
		case c.nextFrom == 0 && e.NextTier != nil:
			t.Errorf("q=%d: want no next tier, got %+v", c.qualified, *e.NextTier)
		case c.nextFrom != 0 && (e.NextTier == nil || e.NextTier.From != c.nextFrom || e.NextTier.BiddersNeeded != c.nextNeed):
			t.Errorf("q=%d: want next tier from %d needing %d, got %+v", c.qualified, c.nextFrom, c.nextNeed, e.NextTier)
		}
	}
	// Subtotals add up to the total.
	e := p.Earnings(60)
	sum := Zero()
	for _, row := range e.Tiers {
		sum = sum.Add(row.Subtotal)
	}
	if !sum.Equal(e.Earned) || e.Tiers[2].Bidders != 10 {
		t.Fatalf("breakdown mismatch: %+v", e.Tiers)
	}
}

func TestReferralZeroThresholdPaysFromFirstBidder(t *testing.T) {
	p := NewDefaultReferralProgram(ReferralKindInfluencer)
	p.Threshold = 0
	p.Tiers = []ReferralTier{{From: 1, Amount: NewMoney(3)}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := p.Earnings(4).Earned.Float64(); got != 12 {
		t.Fatalf("earned %v, want 12", got)
	}
}

func TestReferralProgramValidate(t *testing.T) {
	base := func() ReferralProgram { return NewDefaultReferralProgram(ReferralKindAgent) }
	cases := map[string]func(p *ReferralProgram){
		"guardrail":          func(p *ReferralProgram) { p.Tiers[2].Amount = NewMoney(25) },
		"gap":                func(p *ReferralProgram) { p.Tiers[1].From = 27 },
		"starts late":        func(p *ReferralProgram) { p.Tiers[0].From = 12 },
		"falling rate":       func(p *ReferralProgram) { p.Tiers[1].Amount = NewMoney(4) },
		"closed last tier":   func(p *ReferralProgram) { p.Tiers[2].To = intp(80) },
		"open middle tier":   func(p *ReferralProgram) { p.Tiers[1].To = nil },
		"no tiers":           func(p *ReferralProgram) { p.Tiers = nil },
		"zero margin":        func(p *ReferralProgram) { p.NetMarginPerBid = Zero() },
		"bad window":         func(p *ReferralProgram) { p.AttributionWindowDays = 0 },
		"bad kind":           func(p *ReferralProgram) { p.Kind = "partner" },
		"negative threshold": func(p *ReferralProgram) { p.Threshold = -1 },
	}
	for name, mutate := range cases {
		p := base()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestReferralQualifiesAuction(t *testing.T) {
	p := NewDefaultReferralProgram(ReferralKindAgent)
	a, b := uuid.New(), uuid.New()
	if !p.QualifiesAuction(a) {
		t.Fatal("empty list means any auction")
	}
	p.QualifyingAuctionIDs = []uuid.UUID{a}
	if !p.QualifiesAuction(a) || p.QualifiesAuction(b) {
		t.Fatal("only listed auctions qualify")
	}
}

func TestReferralCodes(t *testing.T) {
	for i := 0; i < 50; i++ {
		c, err := GenerateReferralCode()
		if err != nil || len(c) != 6 || strings.ContainsAny(c, "01OI") || ValidateReferralCode(c) != nil {
			t.Fatalf("bad generated code %q (%v)", c, err)
		}
	}
	if NormalizeReferralCode("  selam-1 ") != "SELAM-1" || ValidateReferralCode("SELAM-1") != nil {
		t.Fatal("custom code should normalize and validate")
	}
	for _, bad := range []string{"AB", "-ABCD", "AB CD", "ABCDEFGHIJKLMNOPQRSTU", "ÄBCD"} {
		if ValidateReferralCode(bad) == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPhoneDigitsAndMasks(t *testing.T) {
	for _, in := range []string{"+251 911 223 344", "0911223344", "251911223344", "911223344"} {
		if got := PhoneDigits(in); got != "251911223344" {
			t.Errorf("PhoneDigits(%q) = %q", in, got)
		}
	}
	if MaskPhone("0911223344") != "•••• ••44" || MaskName("Abebe Kebede") != "Abebe K." || MaskName("Selam") != "Selam" {
		t.Fatal("masking changed")
	}
}
