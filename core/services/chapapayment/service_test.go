package chapapayment

import (
	"context"
	"errors"
	"testing"

	"github.com/bid-hub/admin-backend/core/domain"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveBidValues_CommaAndUnderscoreSeparatedStringInMeta(t *testing.T) {
	svc := &Service{}
	snap := &VerifiedSnapshot{
		TxRef: "R-test123",
		Meta: map[string]interface{}{
			"bid_values": "1.25_3.50_10.00",
		},
	}

	got := svc.resolveBidValues(context.Background(), "R-test123", snap)
	require.Len(t, got, 3)
	assert.Equal(t, "1.25", got[0].String())
	assert.Equal(t, "3.50", got[1].String())
	assert.Equal(t, "10.00", got[2].String())
}

func TestResolveBidValues_PendingBidsFallback(t *testing.T) {
	svc := &Service{}
	svc.SetPendingBids("R-pack456", []domain.Money{
		domain.NewMoney(2.10),
		domain.NewMoney(5.40),
	})

	got := svc.resolveBidValues(context.Background(), "R-pack456", &VerifiedSnapshot{TxRef: "R-pack456"})
	require.Len(t, got, 2)
	assert.Equal(t, "2.10", got[0].String())
	assert.Equal(t, "5.40", got[1].String())
}

func TestIsChapaUnknownTx(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("chapa verify api error (status 400): {\"message\":\"Invalid transaction reference\",\"status\":\"failed\",\"data\":null}"), true},
		{errors.New("chapa verify: transaction not found for tx_ref \"R-abc\""), true},
		{errors.New("chapa verify request failed: context deadline exceeded"), false},
		{errors.New("chapa verify api error (status 500): internal"), false},
	}
	for _, c := range cases {
		if got := isChapaUnknownTx(c.err); got != c.want {
			t.Errorf("isChapaUnknownTx(%v)=%v want %v", c.err, got, c.want)
		}
	}
}
