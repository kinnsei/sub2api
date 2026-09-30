//go:build unit

package service

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// The currency column is authoritative, but an empty column must fall through to
// the snapshot: NormalizePaymentCurrency("") returns the default, so a naive
// "normalize the column" read would report CNY for a USD order whose row predates
// the backfill. That mismatch would change the amount tolerance applied to
// provider notifications for that order.
func TestPaymentOrderCurrencyPrefersColumnThenSnapshot(t *testing.T) {
	tests := []struct {
		name     string
		order    *dbent.PaymentOrder
		expected string
	}{
		{
			name:     "currency column wins over snapshot",
			order:    &dbent.PaymentOrder{Currency: "USD", ProviderSnapshot: map[string]any{"currency": "CNY"}},
			expected: "USD",
		},
		{
			name:     "column wins for a channel whose snapshot never had a currency",
			order:    &dbent.PaymentOrder{Currency: "HKD"},
			expected: "HKD",
		},
		{
			// The pre-backfill row case: empty column must not be normalized to CNY.
			name:     "empty column falls back to snapshot",
			order:    &dbent.PaymentOrder{Currency: "", ProviderSnapshot: map[string]any{"currency": "USD"}},
			expected: "USD",
		},
		{
			name:     "empty column and no snapshot uses the default",
			order:    &dbent.PaymentOrder{Currency: ""},
			expected: "CNY",
		},
		{
			name:     "invalid column value falls back to snapshot",
			order:    &dbent.PaymentOrder{Currency: "NOT_A_CURRENCY", ProviderSnapshot: map[string]any{"currency": "USD"}},
			expected: "USD",
		},
		{
			name:     "nil order is safe",
			order:    nil,
			expected: "CNY",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, PaymentOrderCurrency(tc.order))
		})
	}
}

// paymentProviderConfigCurrency is what freezes the currency at order creation, so
// it must agree with the read path for the same provider config.
func TestPaymentProviderConfigCurrencyMatchesReadPath(t *testing.T) {
	require.Equal(t, "USD", paymentProviderConfigCurrency("stripe", map[string]string{"currency": "usd"}))
	require.Equal(t, "HKD", paymentProviderConfigCurrency("airwallex", map[string]string{"currency": "HKD"}))
	// Alipay/EasyPay settle in the default currency regardless of config.
	require.Equal(t, "CNY", paymentProviderConfigCurrency("alipay", map[string]string{"currency": "USD"}))
	require.Equal(t, "CNY", paymentProviderConfigCurrency("easypay", nil))
}
