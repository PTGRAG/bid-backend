package domain

import (
	"errors"
	"testing"
)

func TestParseFreeEntranceLimit(t *testing.T) {
	valid := []struct {
		in   string
		want int
	}{
		{"1", 1},
		{"10", 10},
		{"100", 100},
		{" 100 ", 100},
		{"1k", 1000},
		{"1K", 1000},
		{"25k", 25000},
		{"1000", 1000},
		{"1000000", 1000000},
		{"1000k", 1000000},
		{"all", FreeEntranceUnlimited},
		{"ALL", FreeEntranceUnlimited},
		{" All ", FreeEntranceUnlimited},
		{"unlimited", FreeEntranceUnlimited},
		{"007", 7},
	}
	for _, tc := range valid {
		got, err := ParseFreeEntranceLimit(tc.in)
		if err != nil {
			t.Errorf("ParseFreeEntranceLimit(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseFreeEntranceLimit(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	invalid := []string{
		"",
		"   ",
		"0",    // "all" is the only way to say unlimited
		"0k",   // still zero
		"-1",   // negative
		"-5k",  //
		"1.5",  // decimals
		"1.5k", //
		"1e3",  // exponent
		"1m",   // unknown suffix
		"k",    // suffix only
		"1kk",  //
		"abc",  //
		"10 0", // embedded space
		"1,000",
		"+10",
		"1000001",  // over the max
		"1001k",    // over the max via suffix
		"99999999", // too many digits
		"٣",        // non-ASCII digit
		"all ten",  //
		"100; DROP TABLE auctions",
	}
	for _, in := range invalid {
		got, err := ParseFreeEntranceLimit(in)
		if err == nil {
			t.Errorf("ParseFreeEntranceLimit(%q) = %d, want error", in, got)
			continue
		}
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ParseFreeEntranceLimit(%q) error should wrap ErrInvalidInput, got %v", in, err)
		}
	}
}

func TestValidateFreeEntranceLimit(t *testing.T) {
	for _, ok := range []int{0, 1, 100, MaxFreeEntranceLimit} {
		if err := ValidateFreeEntranceLimit(ok); err != nil {
			t.Errorf("ValidateFreeEntranceLimit(%d) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{-1, -100, MaxFreeEntranceLimit + 1} {
		if err := ValidateFreeEntranceLimit(bad); err == nil {
			t.Errorf("ValidateFreeEntranceLimit(%d) = nil, want error", bad)
		}
	}
}

func TestAuctionFreeEntranceAvailability(t *testing.T) {
	cases := []struct {
		name          string
		a             *Auction
		wantAvailable bool
		wantUnlimited bool
		wantRemaining int
	}{
		{"nil", nil, false, false, 0},
		{"disabled", &Auction{FreeEntranceEnabled: false, FreeEntranceLimit: 100}, false, false, 0},
		{"fresh 100", &Auction{FreeEntranceEnabled: true, FreeEntranceLimit: 100}, true, false, 100},
		{"partly claimed", &Auction{FreeEntranceEnabled: true, FreeEntranceLimit: 100, FreeEntranceClaimed: 99}, true, false, 1},
		{"full", &Auction{FreeEntranceEnabled: true, FreeEntranceLimit: 100, FreeEntranceClaimed: 100}, false, false, 0},
		{"over-claimed never negative", &Auction{FreeEntranceEnabled: true, FreeEntranceLimit: 10, FreeEntranceClaimed: 12}, false, false, 0},
		{"all", &Auction{FreeEntranceEnabled: true, FreeEntranceLimit: FreeEntranceUnlimited, FreeEntranceClaimed: 5000}, true, true, 0},
		{"all but disabled", &Auction{FreeEntranceEnabled: false, FreeEntranceLimit: FreeEntranceUnlimited}, false, false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.FreeEntranceAvailable(); got != tc.wantAvailable {
				t.Errorf("FreeEntranceAvailable() = %v, want %v", got, tc.wantAvailable)
			}
			if got := tc.a.FreeEntranceIsUnlimited(); got != tc.wantUnlimited {
				t.Errorf("FreeEntranceIsUnlimited() = %v, want %v", got, tc.wantUnlimited)
			}
			if got := tc.a.FreeSlotsRemaining(); got != tc.wantRemaining {
				t.Errorf("FreeSlotsRemaining() = %d, want %d", got, tc.wantRemaining)
			}
		})
	}
}

func TestCheckoutCharge(t *testing.T) {
	m := func(s string) Money {
		v, err := NewMoneyFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	fee := m("60")
	cases := []struct {
		name  string
		bids  []Money
		waive bool
		want  string
	}{
		{"no promo single", []Money{m("1")}, false, "61.00"},
		{"no promo pack", []Money{m("10.4"), m("11.11")}, false, "141.51"},
		{"promo single is bid only", []Money{m("1")}, true, "1.00"},
		{"promo pack pays fee from 2nd bid", []Money{m("10.4"), m("11.11")}, true, "81.51"},
		{"promo three bids [0,1,2]", []Money{m("1"), m("2"), m("3")}, true, "126.00"},
		{"empty pack", nil, true, "0.00"},
	}
	for _, c := range cases {
		if got := CheckoutCharge(c.bids, fee, c.waive).String(); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}
