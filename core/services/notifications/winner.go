package notifications

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/google/uuid"
)

// Pay only if you win: the winner is told on every channel we have (in-app, Telegram DM, SMS,
// email). Other participants always get an in-app message, plus an email and/or Telegram DM only
// when they opted in (users.notify_results_*). Losers never get an SMS.

// SetAppURL sets the public site URL used in SMS and email links (e.g. https://carramarket.com).
func (s *Service) SetAppURL(u string) {
	s.appURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

func (s *Service) absoluteURL(path string) string {
	base := s.appURL
	if base == "" {
		base = "https://" + participationSMSURL
	}
	return base + path
}

func winnerPayPath(auctionID uuid.UUID) string {
	return auctionActionURL(auctionID) + "?pay=1"
}

// eat formats a deadline in Ethiopia time (UTC+3, no DST).
func eat(t time.Time) string {
	return t.In(time.FixedZone("EAT", 3*60*60)).Format("Mon 2 Jan, 15:04") + " (EAT)"
}

func hoursLeft(due, now time.Time) string {
	d := due.Sub(now)
	if d <= time.Hour {
		return "less than an hour"
	}
	h := int(d.Round(time.Hour) / time.Hour)
	if h == 1 {
		return "1 hour"
	}
	return fmt.Sprintf("%d hours", h)
}

// ParticipantContact returns a participant's name, phone and email (users first, then the legacy
// participant_users table). Used for staff alerts.
func (s *Service) ParticipantContact(participantID string) (name, phone, email string) {
	uid, err := uuid.Parse(participantID)
	if err != nil {
		return "", "", ""
	}
	if s.userRepo != nil {
		if u, uErr := s.userRepo.GetByID(uid); uErr == nil && u != nil {
			name, phone, email = strings.TrimSpace(u.FullName), strings.TrimSpace(u.Phone), strings.TrimSpace(u.Email)
		}
	}
	if (name == "" || phone == "" || email == "") && s.participantUserRepo != nil {
		if pu, pErr := s.participantUserRepo.GetByID(participantID); pErr == nil && pu != nil {
			if name == "" {
				name = strings.TrimSpace(pu.FullName)
			}
			if phone == "" {
				phone = strings.TrimSpace(pu.Phone)
			}
			if email == "" {
				email = strings.TrimSpace(pu.Email)
			}
		}
	}
	// Telegram sign-ups get a placeholder address (tg_<id>@carramarket.com) that nobody reads.
	if !strings.Contains(email, "@") || strings.HasSuffix(strings.ToLower(email), "@carramarket.com") {
		email = ""
	}
	return name, phone, email
}

func (s *Service) sendParticipantEmail(participantID, subject, heading, body, buttonLabel, buttonURL string) {
	if s.emailSvc == nil {
		return
	}
	_, _, email := s.ParticipantContact(participantID)
	if email == "" {
		s.log.Info("email_skipped_no_address", "participant_id", participantID)
		return
	}
	if err := s.emailSvc.SendEmail([]string{email}, subject, simpleEmailHTML(heading, body, buttonLabel, buttonURL)); err != nil {
		s.log.Warn("email_send_failed", "participant_id", participantID, "error", err)
		return
	}
	s.log.Info("email_sent", "participant_id", participantID, "subject", subject)
}

func simpleEmailHTML(heading, body, buttonLabel, buttonURL string) string {
	paras := strings.Builder{}
	for _, p := range strings.Split(body, "\n") {
		if strings.TrimSpace(p) == "" {
			continue
		}
		paras.WriteString(`<p style="margin:0 0 12px;font-size:15px;line-height:1.5;color:#1f2937">` + html.EscapeString(p) + `</p>`)
	}
	button := ""
	if buttonURL != "" {
		button = `<p style="margin:20px 0"><a href="` + html.EscapeString(buttonURL) +
			`" style="background:#047857;color:#ffffff;padding:12px 20px;border-radius:8px;text-decoration:none;font-weight:600;display:inline-block">` +
			html.EscapeString(buttonLabel) + `</a></p>`
	}
	return `<!doctype html><html><body style="margin:0;padding:24px;background:#f3f4f6;font-family:Arial,Helvetica,sans-serif">` +
		`<div style="max-width:520px;margin:0 auto;background:#ffffff;border-radius:12px;padding:24px">` +
		`<h1 style="margin:0 0 16px;font-size:20px;color:#111827">` + html.EscapeString(heading) + `</h1>` +
		paras.String() + button +
		`<p style="margin:24px 0 0;font-size:12px;color:#6b7280">CarraMarket</p></div></body></html>`
}

func (s *Service) notifyParticipantWithLink(
	ctx context.Context,
	participantID string,
	auctionID uuid.UUID,
	notifType, title, message, priority, actionURL, actionLabel string,
) error {
	uid, err := uuid.Parse(participantID)
	if err != nil {
		return nil
	}
	aid := auctionID
	return s.createAndInvalidate(ctx, &domain.Notification{
		ID:          uuid.New(),
		UserID:      uid,
		UserType:    domain.UserTypeParticipant,
		Type:        notifType,
		Title:       title,
		Message:     message,
		ActionURL:   actionURL,
		ActionLabel: actionLabel,
		AuctionID:   &aid,
		Priority:    priority,
	})
}

// NotifyWinnerPaymentDue tells the winner they won and must pay their bid within the window.
func (s *Service) NotifyWinnerPaymentDue(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, bidValue domain.Money, dueAt time.Time, attempt int) error {
	name := displayName(title)
	lead := fmt.Sprintf("Congratulations! Your bid of %s ETB is the lowest unique bid on '%s'.", bidValue.String(), name)
	if attempt > 1 {
		lead = fmt.Sprintf("Good news! The earlier winner of '%s' did not pay, so the prize passes to you: your bid of %s ETB is now the lowest unique bid.", name, bidValue.String())
	}
	payLine := fmt.Sprintf("To get the product, pay your bid of %s ETB by %s. If you don't pay in time, the prize goes to the next lowest unique bid.", bidValue.String(), eat(dueAt))
	payURL := s.absoluteURL(winnerPayPath(auctionID))

	err := s.notifyParticipantWithLink(ctx, winnerID, auctionID, domain.NotificationTypeAuctionWinner,
		"You won! Pay to claim your prize", lead+" "+payLine, domain.NotificationPriorityUrgent,
		winnerPayPath(auctionID), "Pay now")

	s.sendParticipantTelegram(ctx, winnerID,
		fmt.Sprintf("🏆 You won '%s'!\n\nYour bid: %s ETB\nPay by: %s\n\nIf you don't pay in time, the prize goes to the next lowest unique bid.", name, bidValue.String(), eat(dueAt)),
		"Pay now")
	s.sendParticipantSMS(winnerID, trimSMS(fmt.Sprintf(
		"CarraMarket: You won '%s'! Pay your bid of %s ETB by %s to claim it: %s", name, bidValue.String(), eat(dueAt), payURL), name))
	s.sendParticipantEmail(winnerID,
		fmt.Sprintf("You won %s! Pay by %s", name, eat(dueAt)),
		"You won! 🏆", lead+"\n"+payLine, fmt.Sprintf("Pay %s ETB", bidValue.String()), payURL)
	return err
}

// NotifyWinnerPaymentReminder reminds the winner (1 = about a day left, 2 = final reminder).
func (s *Service) NotifyWinnerPaymentReminder(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, bidValue domain.Money, dueAt time.Time, reminder int) error {
	name := displayName(title)
	left := hoursLeft(dueAt, time.Now())
	heading := "Reminder: pay to claim your prize"
	if reminder >= 2 {
		heading = "Final reminder: your win expires soon"
	}
	msg := fmt.Sprintf("You won '%s'. You have %s left to pay your bid of %s ETB (deadline %s). After that the prize goes to the next lowest unique bid.",
		name, left, bidValue.String(), eat(dueAt))
	payURL := s.absoluteURL(winnerPayPath(auctionID))

	err := s.notifyParticipantWithLink(ctx, winnerID, auctionID, domain.NotificationTypeWinnerPaymentReminder,
		heading, msg, domain.NotificationPriorityUrgent, winnerPayPath(auctionID), "Pay now")
	s.sendParticipantTelegram(ctx, winnerID, "⏰ "+heading+"\n\n"+msg, "Pay now")
	s.sendParticipantSMS(winnerID, trimSMS(fmt.Sprintf(
		"CarraMarket: %s left to pay %s ETB for '%s' (by %s). Pay now: %s", left, bidValue.String(), name, eat(dueAt), payURL), name))
	s.sendParticipantEmail(winnerID, heading+": "+name, heading, msg, fmt.Sprintf("Pay %s ETB", bidValue.String()), payURL)
	return err
}

// NotifyWinnerPaymentExpired tells a winner they missed the payment deadline.
func (s *Service) NotifyWinnerPaymentExpired(ctx context.Context, participantID string, auctionID uuid.UUID, title string) error {
	name := displayName(title)
	msg := fmt.Sprintf("The payment deadline for '%s' has passed, so the prize has gone to the next lowest unique bid. You have not been charged anything more.", name)
	err := s.notifyParticipantWithLink(ctx, participantID, auctionID, domain.NotificationTypeWinnerPaymentExpired,
		"Payment deadline passed", msg, domain.NotificationPriorityHigh, auctionActionURL(auctionID), "View auction")
	s.sendParticipantTelegram(ctx, participantID, "⌛ "+msg, "View auction")
	s.sendParticipantSMS(participantID, trimSMS(fmt.Sprintf(
		"CarraMarket: The payment deadline for '%s' passed and the prize has gone to the next bidder.", name), name))
	return err
}

// NotifyWinnerPaid confirms the winner's payment.
func (s *Service) NotifyWinnerPaid(ctx context.Context, winnerID string, auctionID uuid.UUID, title string, amount domain.Money, txRef string) error {
	name := displayName(title)
	msg := fmt.Sprintf("We received your payment of %s ETB for '%s'. Our team will contact you to arrange delivery.", amount.String(), name)
	err := s.notifyParticipantWithLink(ctx, winnerID, auctionID, domain.NotificationTypeWinnerPaid,
		"Payment received. The prize is yours!", msg, domain.NotificationPriorityHigh, auctionActionURL(auctionID), "View auction")
	tg := "✅ " + msg
	if txRef != "" {
		tg += "\nRef: " + txRef
	}
	s.sendParticipantTelegram(ctx, winnerID, tg, "View auction")
	s.sendParticipantSMS(winnerID, trimSMS(fmt.Sprintf(
		"CarraMarket: Payment of %s ETB for '%s' received. We will contact you to arrange delivery.", amount.String(), name), name))
	s.sendParticipantEmail(winnerID, "Payment received: "+name, "The prize is yours!", msg, "View auction",
		s.absoluteURL(auctionActionURL(auctionID)))
	return err
}

// NotifyAuctionResultLosers tells every participant except the winner that the draw is done.
func (s *Service) NotifyAuctionResultLosers(ctx context.Context, auctionID uuid.UUID, title string, winnerID string) error {
	if s.participationRepo == nil {
		return nil
	}
	name := displayName(title)
	heading := "Draw complete"
	msg := fmt.Sprintf("The draw for '%s' is done and your bids did not win this time. You won't be charged anything more. Open the auction to see every bid and check the result yourself.", name)
	url := auctionActionURL(auctionID)

	offset := 0
	for {
		parts, total, err := s.participationRepo.ListByAuction(auctionID, auctionParticipantPageSize, offset)
		if err != nil {
			return fmt.Errorf("list auction participants: %w", err)
		}
		if len(parts) == 0 {
			break
		}
		batch := make([]*domain.Notification, 0, len(parts))
		optIn := make([]string, 0)
		for _, p := range parts {
			if p.Status != "" && p.Status != domain.ParticipationStatusActive {
				continue
			}
			if strings.EqualFold(p.ParticipantID, winnerID) {
				continue
			}
			uid, err := uuid.Parse(p.ParticipantID)
			if err != nil {
				continue
			}
			aid := auctionID
			batch = append(batch, &domain.Notification{
				ID:          uuid.New(),
				UserID:      uid,
				UserType:    domain.UserTypeParticipant,
				Type:        domain.NotificationTypeAuctionResult,
				Title:       heading,
				Message:     msg,
				ActionURL:   url,
				ActionLabel: "See results",
				AuctionID:   &aid,
				Priority:    domain.NotificationPriorityNormal,
			})
			optIn = append(optIn, p.ParticipantID)
		}
		if err := s.flushBatch(ctx, batch); err != nil {
			return err
		}
		for _, pid := range optIn {
			s.sendOptInResult(ctx, pid, name, msg, auctionID)
		}
		offset += len(parts)
		if int64(offset) >= total {
			break
		}
	}
	return nil
}

func (s *Service) sendOptInResult(ctx context.Context, participantID, name, msg string, auctionID uuid.UUID) {
	if s.userRepo == nil {
		return
	}
	uid, err := uuid.Parse(participantID)
	if err != nil {
		return
	}
	u, err := s.userRepo.GetByID(uid)
	if err != nil || u == nil {
		return
	}
	if u.NotifyResultsTelegram {
		s.sendParticipantTelegram(ctx, participantID, "📣 "+msg, "See results")
	}
	if u.NotifyResultsEmail {
		s.sendParticipantEmail(participantID, "Draw complete: "+name, "Draw complete", msg, "See results",
			s.absoluteURL(auctionActionURL(auctionID)))
	}
}

// trimSMS keeps an SMS under 300 characters by shortening the product name, never the link.
func trimSMS(msg, productName string) string {
	if len(msg) <= 300 {
		return msg
	}
	over := len(msg) - 300 + 3
	if over < len(productName) {
		return strings.Replace(msg, productName, productName[:len(productName)-over]+"...", 1)
	}
	return msg[:297] + "..."
}
