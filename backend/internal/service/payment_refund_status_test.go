//go:build unit

package service

import (
	"testing"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/stretchr/testify/require"
)

// refundedStatusForTotal is the single place that decides whether an order is
// fully or only partly refunded. It previously existed as three open-coded copies
// which drifted — the stuck-refund sweep forgot the fully-refunded case and
// released a fully refunded order as PARTIALLY_REFUNDED, leaving it refundable
// again. These cases pin every boundary so the copies cannot diverge again.
func TestRefundedStatusForTotalBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		amount   float64
		refunded float64
		expected string
	}{
		{"nothing refunded", 100, 0, OrderStatusCompleted},
		{"partial refund", 100, 60, OrderStatusPartiallyRefunded},
		{"exactly fully refunded", 100, 100, OrderStatusRefunded},
		{"fully refunded from two installments", 100, 99.99, OrderStatusRefunded},
		// tolerance is 0.01 for CNY: one minor unit below the amount still counts
		// as fully refunded, matching how provider amounts are compared.
		{"over-refunded is still refunded", 100, 160, OrderStatusRefunded},
		// The boundary that the old `> tolerance` (0.01) check got wrong: a real
		// one-cent refund must not be reported as "nothing refunded".
		{"one cent refund is partial", 100, 0.01, OrderStatusPartiallyRefunded},
		{"one cent order fully refunded", 0.01, 0.01, OrderStatusRefunded},
		{"sub-tolerance partial is partial", 100, 0.001, OrderStatusPartiallyRefunded},
		// Zero-amount orders are rejected at creation (payment_order.go:148), so
		// this is defensive only; a definite refund must still not be erased.
		{"zero amount order with refund", 0, 5, OrderStatusPartiallyRefunded},
		{"zero amount order without refund", 0, 0, OrderStatusCompleted},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			order := &dbent.PaymentOrder{Amount: tc.amount, Currency: "CNY"}
			require.Equal(t, tc.expected, refundedStatusForTotal(order, tc.refunded))
		})
	}
}

// A nil order means the plan carried no snapshot; reporting COMPLETED would make
// the order fully refundable again, so the conservative status is required.
func TestRefundedStatusForTotalNilOrderIsConservative(t *testing.T) {
	require.Equal(t, OrderStatusPartiallyRefunded, refundedStatusForTotal(nil, 0))
	require.Equal(t, OrderStatusPartiallyRefunded, refundedStatusForTotal(nil, 50))
}

// Non-CNY currencies use a currency-specific tolerance. JPY has no minor unit
// (factor 0), so a 1 JPY refund on a 100 JPY order must be partial, and full
// refund detection must not depend on the CNY 0.01 constant.
func TestRefundedStatusForTotalHonoursCurrencyTolerance(t *testing.T) {
	order := &dbent.PaymentOrder{Amount: 100, Currency: "JPY"}
	require.Equal(t, OrderStatusPartiallyRefunded, refundedStatusForTotal(order, 1))
	require.Equal(t, OrderStatusRefunded, refundedStatusForTotal(order, 100))
}
