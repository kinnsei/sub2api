//go:build unit

package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Two partial refunds of the SAME amount on the SAME order are legitimate (e.g.
// refund 30 twice on a 100 order). Each gateway call must therefore carry an
// installment-specific idempotency reference; deriving it from (order, amount)
// makes the second refund a duplicate of the first, so the gateway returns the
// first refund and moves no money while the caller records a second success.
func TestRefundReferencesDistinguishEqualPartialRefunds(t *testing.T) {
	const (
		orderID  = "sub2o123"
		intentID = "pi_abc"
		amount   = "30.00"
		refOne   = "sub2o123r0"
		refTwo   = "sub2o123r1"
	)

	stripeFirst := stripeRefundIdempotencyKey(orderID, 3000, refOne)
	stripeSecond := stripeRefundIdempotencyKey(orderID, 3000, refTwo)
	require.NotEqual(t, stripeFirst, stripeSecond,
		"second equal partial refund must not reuse the first Stripe idempotency key")

	airwallexFirst := airwallexRefundRequestID(intentID, amount, refOne)
	airwallexSecond := airwallexRefundRequestID(intentID, amount, refTwo)
	require.NotEqual(t, airwallexFirst, airwallexSecond,
		"second equal partial refund must not reuse the first Airwallex request id")

	wxpayFirst := wxpayRefundReference(orderID, amount, refOne)
	wxpaySecond := wxpayRefundReference(orderID, amount, refTwo)
	require.NotEqual(t, wxpayFirst, wxpaySecond,
		"second equal partial refund must not reuse the first out_refund_no")
}

// A retry of the SAME installment must produce the SAME reference, or the gateway
// would treat a retry as a brand new refund and pay out twice.
func TestRefundReferencesAreStableAcrossRetries(t *testing.T) {
	const (
		orderID  = "sub2o123"
		intentID = "pi_abc"
		amount   = "30.00"
		ref      = "sub2o123r0"
	)

	require.Equal(t, stripeRefundIdempotencyKey(orderID, 3000, ref), stripeRefundIdempotencyKey(orderID, 3000, ref))
	require.Equal(t, airwallexRefundRequestID(intentID, amount, ref), airwallexRefundRequestID(intentID, amount, ref))
	require.Equal(t, wxpayRefundReference(orderID, amount, ref), wxpayRefundReference(orderID, amount, ref))
}

// Callers that supply no persisted reference must keep the previous behaviour, so
// this change cannot break any existing caller.
func TestRefundReferencesFallBackWithoutRefundNo(t *testing.T) {
	require.Equal(t, "re-sub2o123-3000", stripeRefundIdempotencyKey("sub2o123", 3000, ""))
	require.Equal(t, "re-sub2o123-3000", stripeRefundIdempotencyKey("sub2o123", 3000, "   "))
	require.Equal(t, airwallexRefundRequestID("pi_abc", "30.00", ""), airwallexRefundRequestID("pi_abc", "30.00", "  "))
	require.Equal(t, wxpayRefundID("sub2o123", "30.00"), wxpayRefundReference("sub2o123", "30.00", " "))
}
