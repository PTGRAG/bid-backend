//go:build integration

package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bid-hub/admin-backend/adapters/repository/postgres"
	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/bid-hub/admin-backend/core/services/referral"
	"github.com/bid-hub/admin-backend/platform/database"
	"github.com/bid-hub/admin-backend/testutil"
	"github.com/google/uuid"
)

// Agent referrals: link click -> signup -> qualified bidder (a paid ticket), one referral per
// phone and per payment account, tiered payouts after the threshold.

type refEnv struct {
	t     *testing.T
	s     *testutil.Stack
	repo  *postgres.ReferralRepo
	svc   *referral.Service
	admin *domain.User
	n     int
}

type fakePayer map[string]string // tx_ref -> payer account

func (f fakePayer) PayerAccount(txRef string) (string, error) { return f[txRef], nil }

func newRefEnv(t *testing.T) *refEnv {
	t.Helper()
	s := stack(t)
	database.EnsureSafeSchema(s.PG.DB) // Reset truncated the default programs
	repo := postgres.NewReferralRepository(s.PG.DB)
	svc := referral.NewService(repo, postgres.NewUserRepository(s.PG.DB), nil)
	svc.SetAppURL("https://carra.test")
	return &refEnv{t: t, s: s, repo: repo, svc: svc, admin: testutil.SeedAdminUser(t, s.PG)}
}

// user creates a participant account; created is how long ago it signed up.
func (e *refEnv) user(name, phone string, created time.Duration) *domain.User {
	e.t.Helper()
	e.n++
	u := &domain.User{
		ID: uuid.New(), FullName: name, Email: fmt.Sprintf("ref-%d-%s@test.local", e.n, uuid.NewString()[:6]),
		Password: "x", Phone: phone, Role: domain.RoleParticipant, Status: domain.StatusActive,
		CreatedAt: time.Now().Add(-created), UpdatedAt: time.Now(),
	}
	if err := e.s.PG.DB.Create(u).Error; err != nil {
		e.t.Fatal(err)
	}
	return u
}

func (e *refEnv) pay(u *domain.User, auctionID uuid.UUID, amount float64, status string, at time.Time) string {
	e.t.Helper()
	e.n++
	tx := fmt.Sprintf("TX-REF-%d-%s", e.n, uuid.NewString()[:6])
	p := &domain.Payment{
		TxRef: tx, UserID: &u.ID, AuctionID: &auctionID, Amount: domain.NewMoney(amount),
		Status: status, Purpose: domain.PaymentPurposeEntry, Processed: true,
	}
	if err := e.s.PG.DB.Create(p).Error; err != nil {
		e.t.Fatal(err)
	}
	if err := e.s.PG.DB.Exec("UPDATE payments SET created_at = ? WHERE tx_ref = ?", at, tx).Error; err != nil {
		e.t.Fatal(err)
	}
	return tx
}

func (e *refEnv) agentCode(owner *domain.User) *referral.CodeDetail {
	e.t.Helper()
	d, err := e.svc.CreateCode(context.Background(), e.admin.ID, nil, referral.CodeInput{Kind: domain.ReferralKindAgent, OwnerPhone: owner.Phone})
	if err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *refEnv) claim(u *domain.User, code string) *referral.ClaimResult {
	e.t.Helper()
	r, err := e.svc.Claim(context.Background(), u.ID, code, "visitor-"+u.ID.String()[:8])
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *refEnv) sweep() referral.SweepResult {
	e.t.Helper()
	r, err := e.svc.Sweep(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *refEnv) status(u *domain.User) *domain.ReferralAttribution {
	e.t.Helper()
	a, err := e.repo.GetAttributionByUser(u.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *refEnv) dash(owner *domain.User) referral.CodeSummary {
	e.t.Helper()
	d, err := e.svc.MyDashboard(context.Background(), owner.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(d.Codes) != 1 {
		e.t.Fatalf("want 1 code, got %d", len(d.Codes))
	}
	return d.Codes[0]
}

func TestReferral_FunnelClickSignupQualify(t *testing.T) {
	e := newRefEnv(t)
	ctx := context.Background()
	auction := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive, EntranceFee: m(30)})
	agent := e.user("Abebe Kebede", "+251911100100", 30*24*time.Hour)
	code := e.agentCode(agent)
	if !strings.HasPrefix(code.Code.Link, "https://carra.test/?ref=") || len(code.Code.Code) != 6 {
		t.Fatalf("unexpected link %q", code.Code.Link)
	}

	// Two visits from one browser and one from another: 2 visitors, 3 visits.
	for _, v := range []string{"visitor-aaaaaaaa", "visitor-aaaaaaaa", "visitor-bbbbbbbb"} {
		pc, err := e.svc.RecordClick(ctx, strings.ToLower(code.Code.Code), v, "10.0.0.1", "test", "/")
		if err != nil {
			t.Fatal(err)
		}
		if pc.InvitedBy != "Abebe K." {
			t.Fatalf("invitedBy %q", pc.InvitedBy)
		}
	}

	buyer := e.user("Selam Tesfaye", "0922200200", time.Hour)
	if r := e.claim(buyer, code.Code.Code); !r.Attributed {
		t.Fatalf("claim not attributed: %+v", r)
	}
	if r := e.claim(buyer, code.Code.Code); !r.Attributed {
		t.Fatal("claim should be idempotent")
	}

	// A free ticket (0 ETB) does not qualify; a paid one does.
	e.pay(buyer, auction.ID, 0, domain.PaymentStatusSuccess, time.Now())
	if r := e.sweep(); r.Qualified != 0 {
		t.Fatalf("free ticket qualified: %+v", r)
	}
	e.pay(buyer, auction.ID, 30, domain.PaymentStatusPending, time.Now())
	if r := e.sweep(); r.Qualified != 0 {
		t.Fatalf("pending payment qualified: %+v", r)
	}
	tx := e.pay(buyer, auction.ID, 30, domain.PaymentStatusSuccess, time.Now())
	if r := e.sweep(); r.Qualified != 1 {
		t.Fatalf("want 1 qualified, got %+v", r)
	}
	a := e.status(buyer)
	if a.Status != domain.ReferralQualified || a.QualifyingTxRef != tx || a.QualifiedPhone != "251922200200" {
		t.Fatalf("attribution %+v", a)
	}

	d := e.dash(agent)
	f := d.Funnel
	if f.Visitors != 2 || f.Visits != 3 || f.Signups != 1 || f.Qualified != 1 || f.VisitorToSignupRate != 50 || f.SignupToQualifiedRate != 100 {
		t.Fatalf("funnel %+v", f)
	}
	if d.Earnings.Unlocked || d.Earnings.Remaining != 9 || !d.Earnings.Earned.IsZero() {
		t.Fatalf("earnings %+v", d.Earnings)
	}

	page, err := e.svc.MyReferrals(ctx, agent.ID, nil, ports.ReferralAttributionFilter{})
	if err != nil || page.Total != 1 {
		t.Fatalf("referrals %+v %v", page, err)
	}
	if got := page.Items[0]; got.Name != "Selam T." || got.Phone != "•••• ••00" || got.UserID != nil || got.AuctionTitle != auction.Title {
		t.Fatalf("agent view must be masked: %+v", got)
	}
	days, err := e.svc.MyActivity(ctx, agent.ID, nil, 7)
	if err != nil || len(days) != 7 || days[6].Signups+days[5].Signups != 1 || days[6].Qualified != 1 {
		t.Fatalf("activity %+v %v", days, err)
	}
}

func TestReferral_ClaimRules(t *testing.T) {
	e := newRefEnv(t)
	ctx := context.Background()
	auction := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive})
	agent := e.user("Agent One", "+251911100101", 30*24*time.Hour)
	other := e.user("Agent Two", "+251911100102", 30*24*time.Hour)
	c1, c2 := e.agentCode(agent), e.agentCode(other)

	if _, err := e.svc.Claim(ctx, agent.ID, c1.Code.Code, ""); err == nil {
		t.Fatal("an agent must not claim their own code")
	}
	old := e.user("Old Customer", "+251933300300", 10*24*time.Hour)
	if r := e.claim(old, c1.Code.Code); r.Attributed || r.Reason != "existing_account" {
		t.Fatalf("old account: %+v", r)
	}
	bought := e.user("Fast Buyer", "+251933300301", time.Hour)
	e.pay(bought, auction.ID, 0, domain.PaymentStatusSuccess, time.Now())
	if r := e.claim(bought, c1.Code.Code); r.Attributed || r.Reason != "existing_customer" {
		t.Fatalf("existing customer: %+v", r)
	}
	fresh := e.user("New Person", "+251933300302", time.Hour)
	e.claim(fresh, c1.Code.Code)
	if r := e.claim(fresh, c2.Code.Code); r.Attributed || r.Reason != "already_referred" {
		t.Fatalf("second code: %+v", r)
	}
	if _, err := e.svc.Claim(ctx, fresh.ID, "NOPE99", ""); err == nil {
		t.Fatal("unknown code must fail")
	}
	paused := domain.ReferralCodePaused
	if _, err := e.svc.UpdateCode(ctx, e.admin.ID, nil, c2.Code.ID, referral.CodeInput{Status: &paused}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RecordClick(ctx, c2.Code.Code, "visitor-cccccccc", "", "", "/"); err == nil {
		t.Fatal("a paused code must not take clicks")
	}
}

func TestReferral_AntiFraud(t *testing.T) {
	e := newRefEnv(t)
	auction := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive})
	agent := e.user("Agent", "+251911100103", 30*24*time.Hour)
	c := e.agentCode(agent)
	payer := fakePayer{}
	e.svc.SetPayerAccountResolver(payer)

	first := e.user("First", "+251944400400", time.Hour)
	samePhone := e.user("Second Account", "0944400400", time.Hour) // same number, other format
	self := e.user("Agent Alt Account", "0911100103", time.Hour)   // the agent's own phone
	sharedWallet := e.user("Wallet Sharer", "+251944400401", time.Hour)
	noPhone := e.user("No Phone", "", time.Hour)
	for _, u := range []*domain.User{first, samePhone, self, sharedWallet, noPhone} {
		e.claim(u, c.Code.Code)
	}
	payer[e.pay(first, auction.ID, 10, domain.PaymentStatusSuccess, time.Now().Add(-3*time.Minute))] = "telebirr:251955500500"
	e.pay(samePhone, auction.ID, 10, domain.PaymentStatusSuccess, time.Now().Add(-2*time.Minute))
	e.pay(self, auction.ID, 10, domain.PaymentStatusSuccess, time.Now().Add(-2*time.Minute))
	payer[e.pay(sharedWallet, auction.ID, 10, domain.PaymentStatusSuccess, time.Now().Add(-time.Minute))] = "telebirr:251955500500"
	e.pay(noPhone, auction.ID, 10, domain.PaymentStatusSuccess, time.Now())

	r := e.sweep()
	if r.Qualified != 1 || r.Rejected != 3 {
		t.Fatalf("sweep %+v", r)
	}
	want := map[*domain.User]string{
		first:        "",
		samePhone:    domain.ReferralRejectDuplicatePhone,
		self:         domain.ReferralRejectSelf,
		sharedWallet: domain.ReferralRejectDuplicateAccount,
	}
	for u, reason := range want {
		if a := e.status(u); a.RejectReason != reason {
			t.Errorf("%s: reason %q, want %q (status %s)", u.FullName, a.RejectReason, reason, a.Status)
		}
	}
	if a := e.status(noPhone); a.Status != domain.ReferralSignedUp {
		t.Fatalf("no phone stays pending, got %s", a.Status)
	}
	page, _ := e.svc.MyReferrals(context.Background(), agent.ID, nil, ports.ReferralAttributionFilter{Status: domain.ReferralSignedUp})
	if page.Total != 1 || !page.Items[0].NeedsPhone {
		t.Fatalf("pending list %+v", page)
	}

	// Staff can reject a counted referral and restore it; the rules are re-applied.
	ctx := context.Background()
	a := e.status(first)
	if err := e.svc.RejectReferral(ctx, e.admin.ID, nil, a.ID, "fake"); err != nil {
		t.Fatal(err)
	}
	// With the first one out, the next sweep does not revive the duplicates (rejections are final).
	if r := e.sweep(); r.Qualified != 0 {
		t.Fatalf("rejected referrals must stay rejected: %+v", r)
	}
	if err := e.svc.RestoreReferral(ctx, e.admin.ID, nil, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.status(first); got.Status != domain.ReferralQualified {
		t.Fatalf("restored referral should qualify again, got %s", got.Status)
	}
}

func TestReferral_ProgramRulesAuctionWindowRefund(t *testing.T) {
	e := newRefEnv(t)
	ctx := context.Background()
	powerBank := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive, EntranceFee: m(20)})
	other := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive, EntranceFee: m(50)})
	agent := e.user("Agent", "+251911100104", 30*24*time.Hour)
	c := e.agentCode(agent)

	progs, err := e.svc.ListPrograms(ctx)
	if err != nil || len(progs) != 2 {
		t.Fatalf("programs %v %v", progs, err)
	}
	agentProg := progs[0]
	if agentProg.Kind != domain.ReferralKindAgent {
		agentProg = progs[1]
	}
	ids := []uuid.UUID{powerBank.ID}
	window := 7
	margin := domain.NewMoney(15)
	view, err := e.svc.UpdateProgram(ctx, e.admin.ID, nil, agentProg.ID, referral.ProgramInput{
		QualifyingAuctionIDs: &ids, AttributionWindowDays: &window, NetMarginPerBid: &margin,
		Tiers: []domain.ReferralTier{{From: 11, To: intPtr(25), Amount: m(5)}, {From: 26, Amount: m(10)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(view.QualifyingAuctions) != 1 || view.QualifyingAuctions[0].Title != powerBank.Title || view.LowestEntranceFee == nil || view.LowestEntranceFee.Float64() != 20 {
		t.Fatalf("program view %+v", view)
	}
	// Guardrail: the top tier must stay below the margin.
	tooHigh := domain.NewMoney(10)
	if _, err := e.svc.UpdateProgram(ctx, e.admin.ID, nil, agentProg.ID, referral.ProgramInput{NetMarginPerBid: &tooHigh}); err == nil {
		t.Fatal("guardrail should reject a margin at or below the top tier")
	}
	bad := []uuid.UUID{uuid.New()}
	if _, err := e.svc.UpdateProgram(ctx, e.admin.ID, nil, agentProg.ID, referral.ProgramInput{QualifyingAuctionIDs: &bad}); err == nil {
		t.Fatal("unknown auction must be rejected")
	}

	wrongAuction := e.user("Wrong Auction", "+251966600600", time.Hour)
	late := e.user("Too Late", "+251966600601", 10*24*time.Hour) // claimed late is blocked, so attribute directly
	refunded := e.user("Refunded", "+251966600602", time.Hour)
	e.claim(wrongAuction, c.Code.Code)
	e.claim(refunded, c.Code.Code)
	if err := e.repo.CreateAttribution(&domain.ReferralAttribution{CodeID: c.Code.ID, ReferredUserID: late.ID, Status: domain.ReferralSignedUp, SignedUpAt: late.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	e.pay(wrongAuction, other.ID, 50, domain.PaymentStatusSuccess, time.Now())
	e.pay(late, powerBank.ID, 20, domain.PaymentStatusSuccess, time.Now()) // 10 days after signup, window is 7
	tx := e.pay(refunded, powerBank.ID, 20, domain.PaymentStatusSuccess, time.Now())
	if r := e.sweep(); r.Qualified != 1 {
		t.Fatalf("only the power bank ticket inside the window counts: %+v", r)
	}
	page, _ := e.svc.MyReferrals(ctx, agent.ID, nil, ports.ReferralAttributionFilter{Status: domain.ReferralSignedUp})
	expired := 0
	for _, it := range page.Items {
		if it.Expired {
			expired++
		}
	}
	if page.Total != 2 || expired != 1 {
		t.Fatalf("pending %+v", page.Items)
	}

	// A refund takes the qualified bidder away again.
	if err := e.s.PG.DB.Exec("UPDATE payments SET status = 'refunded' WHERE tx_ref = ?", tx).Error; err != nil {
		t.Fatal(err)
	}
	if r := e.sweep(); r.Lapsed != 1 || r.Qualified != 0 {
		t.Fatalf("refund should lapse the referral: %+v", r)
	}
	if e.status(refunded).Status != domain.ReferralSignedUp {
		t.Fatal("lapsed referral goes back to pending")
	}
}

func intPtr(n int) *int { return &n }

func TestReferral_EarningsAndPayouts(t *testing.T) {
	e := newRefEnv(t)
	ctx := context.Background()
	auction := testutil.SeedAuction(t, e.s.PG, testutil.AuctionOpts{Status: domain.AuctionStatusActive})
	agent := e.user("Agent", "+251911100105", 30*24*time.Hour)
	c := e.agentCode(agent)
	for i := 0; i < 12; i++ {
		u := e.user(fmt.Sprintf("Bidder %d", i), fmt.Sprintf("+2519777%05d", i), time.Hour)
		e.claim(u, c.Code.Code)
		e.pay(u, auction.ID, 10, domain.PaymentStatusSuccess, time.Now())
	}
	e.sweep()
	d := e.dash(agent)
	if d.Funnel.Qualified != 12 || !d.Earnings.Unlocked || d.Earnings.Earned.Float64() != 10 || d.Earnings.Owed.Float64() != 10 {
		t.Fatalf("12 qualified at 5 ETB from bidder 11: %+v", d.Earnings)
	}
	if d.Earnings.NextTier == nil || d.Earnings.NextTier.From != 26 || d.Earnings.NextTier.BiddersNeeded != 13 {
		t.Fatalf("next tier %+v", d.Earnings.NextTier)
	}
	if _, err := e.svc.AddPayout(ctx, e.admin.ID, nil, c.Code.ID, referral.PayoutInput{Amount: m(11), Method: "telebirr"}); err == nil {
		t.Fatal("payout above what is owed must fail")
	}
	if _, err := e.svc.AddPayout(ctx, e.admin.ID, nil, c.Code.ID, referral.PayoutInput{Amount: m(6), Method: "telebirr", Reference: "TB123"}); err != nil {
		t.Fatal(err)
	}
	d = e.dash(agent)
	if d.Earnings.PaidOut.Float64() != 6 || d.Earnings.Owed.Float64() != 4 {
		t.Fatalf("after payout %+v", d.Earnings)
	}
	payouts, err := e.svc.MyPayouts(ctx, agent.ID)
	if err != nil || len(payouts) != 1 || payouts[0].Reference != "TB123" {
		t.Fatalf("payouts %+v %v", payouts, err)
	}
	list, err := e.svc.ListCodes(ctx, ports.ReferralCodeFilter{Search: "agent"})
	if err != nil || list.Total != 1 || list.Items[0].Qualified != 12 || list.Items[0].Owed.Float64() != 4 || list.Items[0].OwnerPhone != agent.Phone {
		t.Fatalf("staff list %+v %v", list, err)
	}

	// Influencer code: no account needed, custom code, case-insensitive.
	inf, err := e.svc.CreateCode(ctx, e.admin.ID, nil, referral.CodeInput{Kind: domain.ReferralKindInfluencer, Code: "selam-tv", Label: strPtr("Selam TV")})
	if err != nil || inf.Code.Code != "SELAM-TV" {
		t.Fatalf("influencer code %+v %v", inf, err)
	}
	if pc, err := e.svc.LookupCode(ctx, "Selam-Tv"); err != nil || pc.InvitedBy != "Selam TV" {
		t.Fatalf("lookup %+v %v", pc, err)
	}
	if _, err := e.svc.CreateCode(ctx, e.admin.ID, nil, referral.CodeInput{Kind: domain.ReferralKindInfluencer, Code: "SELAM-TV", Label: strPtr("dup")}); err == nil {
		t.Fatal("duplicate code must fail")
	}
}

func strPtr(s string) *string { return &s }
