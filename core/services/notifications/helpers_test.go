package notifications

import (
	"strings"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
)

func TestParticipationSMSCopy_NoRefAndHasLink(t *testing.T) {
	msg := participationSMSCopy("iPhone 17 Pro Max", domain.NewMoney(70), "R-41c1a1d53de14749bf60")
	if strings.Contains(msg, "R-41c1") || strings.Contains(strings.ToLower(msg), "ref") {
		t.Errorf("SMS must not contain the tx ref: %q", msg)
	}
	if !strings.HasSuffix(msg, "carramarket.com") {
		t.Errorf("SMS must end with the website link: %q", msg)
	}
}

func TestParticipationSMSCopy_LongNameKeepsLink(t *testing.T) {
	msg := participationSMSCopy(strings.Repeat("x", 500), domain.NewMoney(70), "R-1")
	if len(msg) > 300 || !strings.HasSuffix(msg, "carramarket.com") {
		t.Errorf("len=%d, must stay <=300 and keep the link: %q", len(msg), msg)
	}
}
