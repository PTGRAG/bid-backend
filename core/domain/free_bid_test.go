package domain

import "testing"

func nmBids(pairs ...interface{}) []NearMissBid {
	out := []NearMissBid{}
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, NearMissBid{ParticipantID: pairs[i].(string), BidValue: NewMoney(pairs[i+1].(float64))})
	}
	return out
}

func pickIDs(p []NearMissPick) []string {
	out := make([]string, len(p))
	for i := range p {
		out[i] = p[i].ParticipantID
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSelectNearMisses_RunnerUpsByDefault(t *testing.T) {
	// 1 and 2 are taken twice (knockouts); 3 is the lowest unique (winner w).
	bids := nmBids("d1", 1.0, "d2", 1.0, "w", 3.0, "a", 4.0, "b", 5.0, "x", 6.0, "x2", 6.0, "c", 7.0, "d", 8.0, "e", 9.0)
	got := SelectNearMisses(bids, "w", NewMoney(3), nil, DefaultNearMissSettings())
	if want := []string{"a", "b", "c", "d"}; !eq(pickIDs(got), want) {
		t.Fatalf("picked %v, want %v", pickIDs(got), want)
	}
	for i, wantRank := range []int{2, 3, 4, 5} {
		if got[i].Rank != wantRank || got[i].Reason != NearMissRunnerUp {
			t.Fatalf("pick %d = %+v, want rank %d runner_up", i, got[i], wantRank)
		}
	}
}

func TestSelectNearMisses_SkipsForfeitedAndDuplicateOwners(t *testing.T) {
	// "l" had the lowest unique bid but let the prize lapse; "w" won with 4. "a" owns two unique
	// bids above the winner and gets one free bid only.
	bids := nmBids("l", 2.0, "w", 4.0, "a", 5.0, "a", 6.0, "l", 7.0, "b", 8.0)
	s := DefaultNearMissSettings()
	s.RunnerUps = 3
	got := SelectNearMisses(bids, "w", NewMoney(4), []string{"l"}, s)
	if want := []string{"a", "b"}; !eq(pickIDs(got), want) {
		t.Fatalf("picked %v, want %v", pickIDs(got), want)
	}
}

func TestSelectNearMisses_DuplicateKnockouts(t *testing.T) {
	bids := nmBids("d1", 1.0, "d2", 1.0, "k1", 2.0, "k2", 2.0, "w", 3.0, "a", 4.0)
	s := DefaultNearMissSettings()
	s.RunnerUps = 1
	s.IncludeDuplicates = true
	s.MaxDuplicates = 3
	got := SelectNearMisses(bids, "w", NewMoney(3), nil, s)
	// Runner-up first, then knockouts closest to the winning bid (2 before 1), capped at 3.
	if want := []string{"a", "k1", "k2", "d1"}; !eq(pickIDs(got), want) {
		t.Fatalf("picked %v, want %v", pickIDs(got), want)
	}
	if got[1].Reason != NearMissDuplicateKnockout {
		t.Fatalf("reason = %s", got[1].Reason)
	}

	s.Enabled = false
	if got := SelectNearMisses(bids, "w", NewMoney(3), nil, s); len(got) != 0 {
		t.Fatalf("disabled must pick nobody, got %v", pickIDs(got))
	}
}

func TestNearMissSettingsValidate(t *testing.T) {
	s := DefaultNearMissSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	s.RunnerUps = MaxNearMissRunnerUps + 1
	if s.Validate() == nil {
		t.Fatal("too many runner-ups accepted")
	}
	s = DefaultNearMissSettings()
	s.MaxDuplicates = -1
	if s.Validate() == nil {
		t.Fatal("negative duplicates accepted")
	}
}

func TestCheckoutChargeWithFreeBids(t *testing.T) {
	fee := NewMoney(50)
	pack := []Money{NewMoney(1), NewMoney(2), NewMoney(3)}
	cases := []struct {
		waive bool
		free  int
		want  float64
	}{
		{false, 0, 150}, {false, 1, 100}, {true, 1, 50}, {true, 2, 0}, {false, 9, 0},
	}
	for _, c := range cases {
		if got := CheckoutChargeWithFreeBids(pack, fee, c.waive, c.free); !got.Equal(NewMoney(c.want)) {
			t.Fatalf("waive=%t free=%d: got %s want %v", c.waive, c.free, got, c.want)
		}
	}
}

func TestFreeBidsApplicable(t *testing.T) {
	cases := []struct {
		balance, tickets int
		waive            bool
		want             int
	}{
		{0, 3, false, 0}, {5, 3, false, 3}, {5, 3, true, 2}, {1, 1, true, 0}, {2, 4, false, 2},
	}
	for _, c := range cases {
		if got := FreeBidsApplicable(c.balance, c.tickets, c.waive); got != c.want {
			t.Fatalf("%+v: got %d", c, got)
		}
	}
}

func TestMaskName(t *testing.T) {
	cases := map[string]string{"Abebe Kebede": "Ab*** K.", "Abebe": "Ab***", "Al": "A***", "": "Bidder", "Selam T Bekele": "Se*** B."}
	for in, want := range cases {
		if got := MaskName(in); got != want {
			t.Fatalf("MaskName(%q) = %q, want %q", in, got, want)
		}
	}
}
