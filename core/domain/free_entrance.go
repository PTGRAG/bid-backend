package domain

import (
	"fmt"
	"strings"
)

// Free-entrance promo
//
// When enabled on an auction, the first N joiners (N = FreeEntranceLimit, or everyone when
// the limit is FreeEntranceUnlimited) skip the entrance fee. They still pay their bid
// amount(s) through Chapa. Each customer may use the promo on one auction only, ever.
const (
	// FreeEntranceUnlimited is the stored limit meaning "all" (no cap on free joiners).
	FreeEntranceUnlimited = 0
	// DefaultFreeEntranceLimit applies when an admin enables the promo without a limit.
	DefaultFreeEntranceLimit = 100
	// MaxFreeEntranceLimit is the highest finite limit an admin may set.
	MaxFreeEntranceLimit = 1_000_000
)

// ErrFreeEntranceUnavailable is returned when a customer expected a free entrance but the
// slot is gone (taken by someone else, promo disabled, already used on another auction).
// It is a 409 so clients refresh the quote instead of silently charging a higher amount.
var ErrFreeEntranceUnavailable = NewAppError(
	409,
	"Free entrance is no longer available for you on this auction. Review the updated total and try again.",
	nil,
)

// ParseFreeEntranceLimit parses the admin "free entrance limit" text box.
//
// Accepted (case-insensitive, surrounding spaces ignored):
//
//	"all", "unlimited"  -> FreeEntranceUnlimited
//	"10", "100", "2500" -> that number
//	"1k", "25k"         -> thousands
//
// Anything else (0, negatives, decimals, other suffixes, > MaxFreeEntranceLimit) is rejected.
func ParseFreeEntranceLimit(raw string) (int, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return 0, fmt.Errorf("free entrance limit is required (use a number like 100, 1k, or \"all\"): %w", ErrInvalidInput)
	}
	if s == "all" || s == "unlimited" {
		return FreeEntranceUnlimited, nil
	}

	multiplier := 1
	digits := s
	if strings.HasSuffix(s, "k") {
		multiplier = 1000
		digits = strings.TrimSuffix(s, "k")
	}
	if digits == "" || len(digits) > 7 {
		return 0, fmt.Errorf("free entrance limit %q is not valid (use a number like 100, 1k, or \"all\"): %w", raw, ErrInvalidInput)
	}
	n := 0
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("free entrance limit %q is not valid (use a number like 100, 1k, or \"all\"): %w", raw, ErrInvalidInput)
		}
		n = n*10 + int(r-'0')
	}
	n *= multiplier
	if err := ValidateFreeEntranceLimit(n); err != nil {
		return 0, err
	}
	if n == FreeEntranceUnlimited {
		return 0, fmt.Errorf("free entrance limit must be at least 1 (use \"all\" for no limit): %w", ErrInvalidInput)
	}
	return n, nil
}

// ValidateFreeEntranceLimit validates a stored limit: 0 (all) or 1..MaxFreeEntranceLimit.
func ValidateFreeEntranceLimit(limit int) error {
	if limit < 0 {
		return fmt.Errorf("free entrance limit cannot be negative: %w", ErrInvalidInput)
	}
	if limit > MaxFreeEntranceLimit {
		return fmt.Errorf("free entrance limit cannot exceed %d: %w", MaxFreeEntranceLimit, ErrInvalidInput)
	}
	return nil
}

// FreeEntranceIsUnlimited reports whether every eligible joiner qualifies (promo on, no cap).
func (a *Auction) FreeEntranceIsUnlimited() bool {
	return a != nil && a.FreeEntranceEnabled && a.FreeEntranceLimit == FreeEntranceUnlimited
}

// FreeEntranceAvailable reports whether the auction can still hand out a free entrance.
func (a *Auction) FreeEntranceAvailable() bool {
	if a == nil || !a.FreeEntranceEnabled {
		return false
	}
	if a.FreeEntranceLimit == FreeEntranceUnlimited {
		return true
	}
	return a.FreeEntranceClaimed < a.FreeEntranceLimit
}

// FreeSlotsRemaining returns how many free spots are left for a finite limit.
// It is 0 when the promo is disabled, full, or unlimited (check FreeEntranceIsUnlimited).
func (a *Auction) FreeSlotsRemaining() int {
	if a == nil || !a.FreeEntranceEnabled || a.FreeEntranceLimit == FreeEntranceUnlimited {
		return 0
	}
	left := a.FreeEntranceLimit - a.FreeEntranceClaimed
	if left < 0 {
		return 0
	}
	return left
}
