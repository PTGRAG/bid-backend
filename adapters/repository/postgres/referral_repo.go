package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/bid-hub/admin-backend/core/ports"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ReferralRepo implements ports.ReferralRepository.
type ReferralRepo struct {
	db *gorm.DB
}

func NewReferralRepository(db *gorm.DB) *ReferralRepo {
	return &ReferralRepo{db: db}
}

var _ ports.ReferralRepository = (*ReferralRepo)(nil)

// paidEntrySQL is the qualifying-ticket rule: a successful ticket checkout that charged money.
const paidEntrySQL = `p.status = 'success' AND p.purpose = 'entry' AND p.amount > 0 AND p.auction_id IS NOT NULL`

func notFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.ErrNotFound
	}
	return err
}

// ---- Programs ----

func (r *ReferralRepo) ListPrograms() ([]domain.ReferralProgram, error) {
	var out []domain.ReferralProgram
	err := r.db.Order("kind, is_default DESC, created_at").Find(&out).Error
	return out, err
}

func (r *ReferralRepo) GetProgram(id uuid.UUID) (*domain.ReferralProgram, error) {
	var p domain.ReferralProgram
	if err := r.db.Where("id = ?", id).First(&p).Error; err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (r *ReferralRepo) GetDefaultProgram(kind string) (*domain.ReferralProgram, error) {
	var p domain.ReferralProgram
	if err := r.db.Where("kind = ? AND is_default", kind).First(&p).Error; err != nil {
		return nil, notFound(err)
	}
	return &p, nil
}

func (r *ReferralRepo) CreateProgram(p *domain.ReferralProgram) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	if err := r.db.Create(p).Error; err != nil {
		if _, ok := pgUniqueViolation(err); ok {
			return domain.NewAppError(409, "There is already a default program for this kind", domain.ErrConflict)
		}
		return err
	}
	return nil
}

func (r *ReferralRepo) UpdateProgram(p *domain.ReferralProgram) error {
	return r.db.Model(&domain.ReferralProgram{}).Where("id = ?", p.ID).Updates(map[string]interface{}{
		"name":                    p.Name,
		"threshold":               p.Threshold,
		"tiers":                   gorm.Expr("?::jsonb", mustJSON(p.Tiers)),
		"net_margin_per_bid":      p.NetMarginPerBid,
		"qualifying_auction_ids":  gorm.Expr("?::jsonb", mustJSON(p.QualifyingAuctionIDs)),
		"attribution_window_days": p.AttributionWindowDays,
		"updated_at":              time.Now().UTC(),
	}).Error
}

func (r *ReferralRepo) AuctionTitles(ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	var rows []struct {
		ID    uuid.UUID
		Title string
	}
	if err := r.db.Table("auctions").Select("id, title").Where("id IN ?", ids).Scan(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[row.ID] = row.Title
	}
	return out, nil
}

// LowestEntranceFee is the cheapest ticket among the given auctions, or among auctions that are
// open or about to open when ids is empty. nil when there are none.
func (r *ReferralRepo) LowestEntranceFee(ids []uuid.UUID) (*domain.Money, error) {
	q := r.db.Table("auctions")
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	} else {
		q = q.Where("status IN ('draft', 'active', 'postponed')")
	}
	var fee *domain.Money
	if err := q.Select("MIN(entrance_fee)").Scan(&fee).Error; err != nil {
		return nil, err
	}
	return fee, nil
}

// ---- Codes ----

func (r *ReferralRepo) CreateCode(c *domain.ReferralCode) error {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if err := r.db.Create(c).Error; err != nil {
		if _, ok := pgUniqueViolation(err); ok {
			return domain.NewAppError(409, "That code is already taken", domain.ErrConflict)
		}
		return err
	}
	return nil
}

func (r *ReferralRepo) UpdateCode(c *domain.ReferralCode) error {
	return r.db.Model(&domain.ReferralCode{}).Where("id = ?", c.ID).Updates(map[string]interface{}{
		"label":      c.Label,
		"status":     c.Status,
		"program_id": c.ProgramID,
		"updated_at": time.Now().UTC(),
	}).Error
}

func (r *ReferralRepo) GetCode(id uuid.UUID) (*domain.ReferralCode, error) {
	var c domain.ReferralCode
	if err := r.db.Where("id = ?", id).First(&c).Error; err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (r *ReferralRepo) GetCodeByCode(code string) (*domain.ReferralCode, error) {
	var c domain.ReferralCode
	if err := r.db.Where("code = ?", domain.NormalizeReferralCode(code)).First(&c).Error; err != nil {
		return nil, notFound(err)
	}
	return &c, nil
}

func (r *ReferralRepo) ListCodesByOwner(ownerID uuid.UUID) ([]domain.ReferralCode, error) {
	var out []domain.ReferralCode
	err := r.db.Where("owner_user_id = ?", ownerID).Order("created_at").Find(&out).Error
	return out, err
}

func (r *ReferralRepo) ListCodes(f ports.ReferralCodeFilter) ([]ports.ReferralCodeRow, int64, error) {
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	q := r.db.Table("referral_codes rc").
		Joins("LEFT JOIN users o ON o.id = rc.owner_user_id")
	if f.Kind != "" {
		q = q.Where("rc.kind = ?", f.Kind)
	}
	if f.Status != "" {
		q = q.Where("rc.status = ?", f.Status)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + strings.ToLower(s) + "%"
		q = q.Where("LOWER(rc.code) LIKE ? OR LOWER(rc.label) LIKE ? OR LOWER(COALESCE(o.full_name, '')) LIKE ? OR COALESCE(o.phone, '') LIKE ?", like, like, like, "%"+s+"%")
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []ports.ReferralCodeRow
	if err := q.Select("rc.*, COALESCE(o.full_name, '') AS owner_name, COALESCE(o.phone, '') AS owner_phone").
		Order("rc.created_at DESC").Offset((f.Page - 1) * f.Limit).Limit(f.Limit).
		Scan(&rows).Error; err != nil {
		return nil, 0, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i := range rows {
		ids[i] = rows[i].ID
	}
	counts, err := r.Counts(ids)
	if err != nil {
		return nil, 0, err
	}
	for i := range rows {
		rows[i].ReferralCounts = counts[rows[i].ID]
	}
	return rows, total, nil
}

func (r *ReferralRepo) Counts(codeIDs []uuid.UUID) (map[uuid.UUID]ports.ReferralCounts, error) {
	out := make(map[uuid.UUID]ports.ReferralCounts, len(codeIDs))
	if len(codeIDs) == 0 {
		return out, nil
	}
	var attr []struct {
		CodeID    uuid.UUID
		Signups   int64
		Pending   int64
		Qualified int64
		Rejected  int64
	}
	if err := r.db.Raw(`SELECT code_id,
			COUNT(*) AS signups,
			COUNT(*) FILTER (WHERE status = 'signed_up') AS pending,
			COUNT(*) FILTER (WHERE status = 'qualified') AS qualified,
			COUNT(*) FILTER (WHERE status = 'rejected') AS rejected
		FROM referral_attributions WHERE code_id IN ? GROUP BY code_id`, codeIDs).Scan(&attr).Error; err != nil {
		return nil, err
	}
	var clicks []struct {
		CodeID   uuid.UUID
		Visitors int64
		Visits   int64
	}
	if err := r.db.Raw(`SELECT code_id, COUNT(*) AS visitors, COALESCE(SUM(hits), 0) AS visits
		FROM referral_clicks WHERE code_id IN ? GROUP BY code_id`, codeIDs).Scan(&clicks).Error; err != nil {
		return nil, err
	}
	var paid []struct {
		CodeID uuid.UUID
		Total  domain.Money
	}
	if err := r.db.Raw(`SELECT code_id, COALESCE(SUM(amount), 0) AS total
		FROM referral_payouts WHERE code_id IN ? GROUP BY code_id`, codeIDs).Scan(&paid).Error; err != nil {
		return nil, err
	}
	for _, id := range codeIDs {
		out[id] = ports.ReferralCounts{PaidOut: domain.Zero()}
	}
	for _, a := range attr {
		c := out[a.CodeID]
		c.Signups, c.Pending, c.Qualified, c.Rejected = a.Signups, a.Pending, a.Qualified, a.Rejected
		out[a.CodeID] = c
	}
	for _, k := range clicks {
		c := out[k.CodeID]
		c.Visitors, c.Visits = k.Visitors, k.Visits
		out[k.CodeID] = c
	}
	for _, p := range paid {
		c := out[p.CodeID]
		c.PaidOut = p.Total
		out[p.CodeID] = c
	}
	return out, nil
}

// Activity returns one row per day (Addis Ababa time) from since to today, zero-filled.
func (r *ReferralRepo) Activity(codeIDs []uuid.UUID, since time.Time) ([]ports.ReferralDailyCount, error) {
	loc := addisLocation()
	start := time.Date(since.In(loc).Year(), since.In(loc).Month(), since.In(loc).Day(), 0, 0, 0, 0, loc)
	today := time.Now().In(loc)
	var days []ports.ReferralDailyCount
	index := map[string]int{}
	for d := start; !d.After(today); d = d.AddDate(0, 0, 1) {
		key := d.Format("2006-01-02")
		index[key] = len(days)
		days = append(days, ports.ReferralDailyCount{Day: key})
	}
	if len(codeIDs) == 0 {
		return days, nil
	}
	type dayCount struct {
		Day string
		N   int64
	}
	run := func(sql string, apply func(*ports.ReferralDailyCount, int64)) error {
		var rows []dayCount
		if err := r.db.Raw(sql, codeIDs, start.UTC()).Scan(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			if i, ok := index[row.Day]; ok {
				apply(&days[i], row.N)
			}
		}
		return nil
	}
	const tz = `'Africa/Addis_Ababa'`
	if err := run(`SELECT to_char(first_seen_at AT TIME ZONE `+tz+`, 'YYYY-MM-DD') AS day, COUNT(*) AS n
		FROM referral_clicks WHERE code_id IN ? AND first_seen_at >= ? GROUP BY 1`,
		func(d *ports.ReferralDailyCount, n int64) { d.Visitors = n }); err != nil {
		return nil, err
	}
	if err := run(`SELECT to_char(signed_up_at AT TIME ZONE `+tz+`, 'YYYY-MM-DD') AS day, COUNT(*) AS n
		FROM referral_attributions WHERE code_id IN ? AND signed_up_at >= ? GROUP BY 1`,
		func(d *ports.ReferralDailyCount, n int64) { d.Signups = n }); err != nil {
		return nil, err
	}
	if err := run(`SELECT to_char(qualified_at AT TIME ZONE `+tz+`, 'YYYY-MM-DD') AS day, COUNT(*) AS n
		FROM referral_attributions WHERE code_id IN ? AND status = 'qualified' AND qualified_at >= ? GROUP BY 1`,
		func(d *ports.ReferralDailyCount, n int64) { d.Qualified = n }); err != nil {
		return nil, err
	}
	return days, nil
}

func addisLocation() *time.Location {
	if loc, err := time.LoadLocation("Africa/Addis_Ababa"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*60*60)
}

// ---- Clicks ----

func (r *ReferralRepo) UpsertClick(c *domain.ReferralClick) error {
	return r.db.Exec(`INSERT INTO referral_clicks (code_id, visitor_id, ip_hash, user_agent, landing_path, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, NOW(), NOW())
		ON CONFLICT (code_id, visitor_id) DO UPDATE SET hits = referral_clicks.hits + 1, last_seen_at = NOW()`,
		c.CodeID, c.VisitorID, c.IPHash, c.UserAgent, c.LandingPath).Error
}

// ---- Attributions ----

func (r *ReferralRepo) CreateAttribution(a *domain.ReferralAttribution) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if err := r.db.Create(a).Error; err != nil {
		if _, ok := pgUniqueViolation(err); ok {
			return domain.ErrConflict
		}
		return err
	}
	return nil
}

func (r *ReferralRepo) GetAttribution(id uuid.UUID) (*domain.ReferralAttribution, error) {
	var a domain.ReferralAttribution
	if err := r.db.Where("id = ?", id).First(&a).Error; err != nil {
		return nil, notFound(err)
	}
	return &a, nil
}

func (r *ReferralRepo) GetAttributionByUser(userID uuid.UUID) (*domain.ReferralAttribution, error) {
	var a domain.ReferralAttribution
	if err := r.db.Where("referred_user_id = ?", userID).First(&a).Error; err != nil {
		return nil, notFound(err)
	}
	return &a, nil
}

func (r *ReferralRepo) ListAttributions(codeIDs []uuid.UUID, f ports.ReferralAttributionFilter) ([]ports.ReferralAttributionRow, int64, error) {
	if len(codeIDs) == 0 {
		return []ports.ReferralAttributionRow{}, 0, nil
	}
	if f.Limit <= 0 || f.Limit > 100 {
		f.Limit = 20
	}
	if f.Page <= 0 {
		f.Page = 1
	}
	q := r.db.Table("referral_attributions ra").
		Joins("JOIN referral_codes rc ON rc.id = ra.code_id").
		Joins("JOIN users u ON u.id = ra.referred_user_id").
		Joins("LEFT JOIN auctions a ON a.id = ra.qualifying_auction_id").
		Where("ra.code_id IN ?", codeIDs)
	if f.Status != "" {
		q = q.Where("ra.status = ?", f.Status)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	var rows []ports.ReferralAttributionRow
	err := q.Select(`ra.*, rc.code AS code, u.full_name AS user_name, COALESCE(u.phone, '') AS user_phone,
			u.created_at AS user_created_at, COALESCE(a.title, '') AS auction_title,
			(SELECT COUNT(*) FROM referral_attributions x
			  WHERE ra.visitor_id <> '' AND x.visitor_id = ra.visitor_id AND x.id <> ra.id) AS same_device`).
		Order("ra.signed_up_at DESC").Offset((f.Page - 1) * f.Limit).Limit(f.Limit).
		Scan(&rows).Error
	return rows, total, err
}

const candidateSelect = `SELECT ra.id AS attribution_id, ra.code_id, ra.referred_user_id, ra.signed_up_at,
		COALESCE(u.phone, '') AS user_phone, rc.owner_user_id, COALESCE(o.phone, '') AS owner_phone,
		rc.kind, rc.program_id
	FROM referral_attributions ra
	JOIN referral_codes rc ON rc.id = ra.code_id
	JOIN users u ON u.id = ra.referred_user_id
	LEFT JOIN users o ON o.id = rc.owner_user_id`

// PendingCandidates returns signed-up attributions whose account has a phone number and bought
// a paid ticket on a qualifying auction inside the program's attribution window, i.e. the ones
// that are ready to qualify or be rejected. The service re-checks every rule.
func (r *ReferralRepo) PendingCandidates(limit int) ([]ports.ReferralCandidate, error) {
	var out []ports.ReferralCandidate
	err := r.db.Raw(candidateSelect+`
		JOIN referral_programs pr ON pr.id = COALESCE(rc.program_id,
			(SELECT d.id FROM referral_programs d WHERE d.kind = rc.kind AND d.is_default))
		WHERE ra.status = 'signed_up' AND u.deleted_at IS NULL AND COALESCE(u.phone, '') <> ''
		  AND EXISTS (SELECT 1 FROM payments p
		              WHERE p.user_id = ra.referred_user_id AND `+paidEntrySQL+`
		                AND p.created_at >= ra.signed_up_at
		                AND p.created_at <= ra.signed_up_at + make_interval(days => pr.attribution_window_days)
		                AND (jsonb_array_length(pr.qualifying_auction_ids) = 0
		                     OR jsonb_exists(pr.qualifying_auction_ids, p.auction_id::text)))
		ORDER BY ra.signed_up_at
		LIMIT ?`, limit).Scan(&out).Error
	return out, err
}

func (r *ReferralRepo) Candidate(attributionID uuid.UUID) (*ports.ReferralCandidate, error) {
	var out []ports.ReferralCandidate
	if err := r.db.Raw(candidateSelect+` WHERE ra.id = ?`, attributionID).Scan(&out).Error; err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, domain.ErrNotFound
	}
	return &out[0], nil
}

func (r *ReferralRepo) FirstQualifyingPayment(userID uuid.UUID, from, to time.Time, auctionIDs []uuid.UUID) (*ports.ReferralQualifyingPayment, error) {
	q := r.db.Table("payments p").
		Select("p.tx_ref, p.auction_id, p.created_at").
		Where("p.user_id = ? AND "+paidEntrySQL, userID).
		Where("p.created_at >= ? AND p.created_at <= ?", from, to)
	if len(auctionIDs) > 0 {
		q = q.Where("p.auction_id IN ?", auctionIDs)
	}
	var rows []ports.ReferralQualifyingPayment
	if err := q.Order("p.created_at").Limit(1).Scan(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// HasPaidEntry reports whether the account ever bought a ticket (any successful ticket checkout,
// free or paid): such an account is an existing customer and cannot be claimed by a code.
func (r *ReferralRepo) HasPaidEntry(userID uuid.UUID) (bool, error) {
	var n int64
	err := r.db.Table("payments").Where("user_id = ? AND status = 'success' AND purpose = 'entry'", userID).Count(&n).Error
	return n > 0, err
}

func (r *ReferralRepo) PhoneQualified(phone string) (bool, error) {
	var n int64
	err := r.db.Model(&domain.ReferralAttribution{}).Where("status = 'qualified' AND qualified_phone = ?", phone).Count(&n).Error
	return n > 0, err
}

func (r *ReferralRepo) AccountQualified(account string) (bool, error) {
	if account == "" {
		return false, nil
	}
	var n int64
	err := r.db.Model(&domain.ReferralAttribution{}).Where("status = 'qualified' AND qualified_account = ?", account).Count(&n).Error
	return n > 0, err
}

func (r *ReferralRepo) MarkQualified(id uuid.UUID, pay ports.ReferralQualifyingPayment, phone, account string, at time.Time) error {
	var acct interface{}
	if account != "" {
		acct = account
	}
	// A savepoint, so a unique violation (phone or account already counted) does not abort the
	// surrounding sweep transaction.
	err := r.db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&domain.ReferralAttribution{}).Where("id = ? AND status = 'signed_up'", id).Updates(map[string]interface{}{
			"status":                domain.ReferralQualified,
			"qualified_at":          at,
			"qualifying_tx_ref":     pay.TxRef,
			"qualifying_auction_id": pay.AuctionID,
			"qualified_phone":       phone,
			"qualified_account":     acct,
			"reject_reason":         "",
			"reject_note":           "",
			"updated_at":            time.Now().UTC(),
		}).Error
	})
	if err != nil {
		if _, ok := pgUniqueViolation(err); ok {
			return domain.ErrConflict
		}
		return err
	}
	return nil
}

func (r *ReferralRepo) MarkRejected(id uuid.UUID, reason, note string) error {
	return r.db.Model(&domain.ReferralAttribution{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":                domain.ReferralRejected,
		"reject_reason":         reason,
		"reject_note":           truncate(note, 255),
		"qualified_at":          nil,
		"qualifying_tx_ref":     "",
		"qualifying_auction_id": nil,
		"qualified_phone":       nil,
		"qualified_account":     nil,
		"updated_at":            time.Now().UTC(),
	}).Error
}

func (r *ReferralRepo) MarkSignedUp(id uuid.UUID) error {
	return r.db.Model(&domain.ReferralAttribution{}).Where("id = ?", id).Updates(map[string]interface{}{
		"status":                domain.ReferralSignedUp,
		"reject_reason":         "",
		"reject_note":           "",
		"qualified_at":          nil,
		"qualifying_tx_ref":     "",
		"qualifying_auction_id": nil,
		"qualified_phone":       nil,
		"qualified_account":     nil,
		"updated_at":            time.Now().UTC(),
	}).Error
}

func (r *ReferralRepo) LapsedQualified(limit int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := r.db.Raw(`SELECT ra.id FROM referral_attributions ra
		WHERE ra.status = 'qualified'
		  AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.tx_ref = ra.qualifying_tx_ref AND `+paidEntrySQL+`)
		LIMIT ?`, limit).Scan(&ids).Error
	return ids, err
}

// sweepLockKey is an arbitrary constant for pg_try_advisory_xact_lock.
const sweepLockKey = 7_301_202_611

func (r *ReferralRepo) WithSweepLock(fn func(repo ports.ReferralRepository) error) (bool, error) {
	ran := false
	err := r.db.Transaction(func(tx *gorm.DB) error {
		var got bool
		if err := tx.Raw("SELECT pg_try_advisory_xact_lock(?)", sweepLockKey).Scan(&got).Error; err != nil {
			return err
		}
		if !got {
			return nil
		}
		ran = true
		return fn(&ReferralRepo{db: tx})
	})
	return ran, err
}

// ---- Payouts ----

func (r *ReferralRepo) CreatePayout(p *domain.ReferralPayout) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	return r.db.Create(p).Error
}

func (r *ReferralRepo) ListPayouts(codeIDs []uuid.UUID) ([]domain.ReferralPayout, error) {
	out := []domain.ReferralPayout{}
	if len(codeIDs) == 0 {
		return out, nil
	}
	err := r.db.Where("code_id IN ?", codeIDs).Order("paid_at DESC").Find(&out).Error
	return out, err
}

// ---- Self-serve agents ----

func (r *ReferralRepo) GetSettings() (*domain.ReferralSettings, error) {
	var st domain.ReferralSettings
	err := r.db.Where("id = 1").First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &domain.ReferralSettings{ID: 1, AgentSignupOpen: true}, nil
	}
	return &st, err
}

func (r *ReferralRepo) UpdateSettings(st *domain.ReferralSettings) error {
	st.ID = 1
	st.UpdatedAt = time.Now().UTC()
	return r.db.Exec(`INSERT INTO referral_settings (id, agent_signup_open, updated_at, updated_by)
		VALUES (1, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET agent_signup_open = EXCLUDED.agent_signup_open,
			updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		st.AgentSignupOpen, st.UpdatedAt, st.UpdatedBy).Error
}

// agentLockKey is an arbitrary constant for pg_advisory_xact_lock.
const agentLockKey = 7_301_202_612

func (r *ReferralRepo) WithAgentLock(fn func(repo ports.ReferralRepository) error) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("SELECT pg_advisory_xact_lock(?)", agentLockKey).Error; err != nil {
			return err
		}
		return fn(&ReferralRepo{db: tx})
	})
}

// phoneDigitsSQL mirrors domain.PhoneDigits for a phone column.
const phoneDigitsSQL = `(CASE
	WHEN regexp_replace(%[1]s, '\D', '', 'g') ~ '^0[0-9]{9}$' THEN '251' || substr(regexp_replace(%[1]s, '\D', '', 'g'), 2)
	WHEN regexp_replace(%[1]s, '\D', '', 'g') ~ '^[79][0-9]{8}$' THEN '251' || regexp_replace(%[1]s, '\D', '', 'g')
	ELSE regexp_replace(%[1]s, '\D', '', 'g') END)`

func (r *ReferralRepo) AgentPhoneTaken(phoneDigits string, exceptUser uuid.UUID) (bool, error) {
	if phoneDigits == "" {
		return false, nil
	}
	var n int64
	err := r.db.Table("referral_codes rc").
		Joins("JOIN users u ON u.id = rc.owner_user_id").
		Where("rc.kind = ? AND u.id <> ? AND COALESCE(u.phone, '') <> ''", domain.ReferralKindAgent, exceptUser).
		Where(fmt.Sprintf(phoneDigitsSQL, "u.phone")+" = ?", phoneDigits).
		Count(&n).Error
	return n > 0, err
}

func (r *ReferralRepo) PayoutAccountTaken(p domain.ReferralPayoutAccount, exceptCode uuid.UUID) (bool, error) {
	var n int64
	err := r.db.Model(&domain.ReferralCode{}).
		Where("payout_method = ? AND payout_bank = ? AND payout_account = ? AND id <> ?", p.Method, p.Bank, p.Account, exceptCode).
		Count(&n).Error
	return n > 0, err
}

func (r *ReferralRepo) SetPayoutAccount(codeID uuid.UUID, p domain.ReferralPayoutAccount) error {
	err := r.db.Model(&domain.ReferralCode{}).Where("id = ?", codeID).Updates(map[string]interface{}{
		"payout_method":  p.Method,
		"payout_bank":    p.Bank,
		"payout_account": p.Account,
		"payout_name":    p.Name,
		"updated_at":     time.Now().UTC(),
	}).Error
	if _, ok := pgUniqueViolation(err); ok {
		return domain.NewAppError(409, "That payout account is already used by another agent", domain.ErrConflict)
	}
	return err
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
