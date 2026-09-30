//go:build unit

package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/stretchr/testify/require"
)

// newPaymentOrderFailureTestOrder inserts an order directly in the requested
// status so the provider-failure bookkeeping can be probed without running the
// whole create-order pipeline.
var paymentOrderFailureTestSeq atomic.Int64

func newPaymentOrderFailureTestOrder(t *testing.T, ctx context.Context, client *dbent.Client, status string) *dbent.PaymentOrder {
	t.Helper()

	user, err := client.User.Create().
		SetEmail("provider-failure@example.com").
		SetPasswordHash("hash").
		SetUsername("provider-failure").
		Save(ctx)
	require.NoError(t, err)

	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(100).
		SetPayAmount(100).
		SetFeeRate(0).
		SetRechargeCode("PAY-FAILURE").
		SetOutTradeNo(fmt.Sprintf("sub2_provider_failure_%d", paymentOrderFailureTestSeq.Add(1))).
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(status).
		SetExpiresAt(time.Now().Add(30 * time.Minute)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		Save(ctx)
	require.NoError(t, err)
	return order
}

func TestMarkOrderFailedAfterProviderErrorPreservesFulfilledOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentService{entClient: client}

	// The defect this guards against: an unconditional status write happily
	// downgrades an order that a concurrent notification already fulfilled.
	// (This documents the hazard; the assertions below are the regression.)
	alreadyPaid := newPaymentOrderFailureTestOrder(t, ctx, client, OrderStatusPaid)
	_, err := client.PaymentOrder.UpdateOneID(alreadyPaid.ID).SetStatus(OrderStatusFailed).Save(ctx)
	require.NoError(t, err)
	downgraded, err := client.PaymentOrder.Get(ctx, alreadyPaid.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusFailed, downgraded.Status,
		"an unguarded write proves the downgrade hazard is real on this schema")

	// Every already-fulfilled status must survive the provider-failure path.
	for _, status := range []string{OrderStatusPaid, OrderStatusRecharging, OrderStatusCompleted} {
		status := status
		t.Run(status, func(t *testing.T) {
			order := newPaymentOrderFailureTestOrder(t, ctx, client, status)

			svc.markOrderFailedAfterProviderError(ctx, order.ID, payment.TypeAlipay, context.DeadlineExceeded)

			got, err := client.PaymentOrder.Get(ctx, order.ID)
			require.NoError(t, err)
			require.Equal(t, status, got.Status, "fulfilled order must not be downgraded to FAILED")
			require.Nil(t, got.FailedAt)
			require.Nil(t, got.FailedReason)
		})
	}

	// A still-pending order created by this request is marked FAILED as before.
	pending := newPaymentOrderFailureTestOrder(t, ctx, client, OrderStatusPending)
	svc.markOrderFailedAfterProviderError(ctx, pending.ID, payment.TypeAlipay, context.DeadlineExceeded)
	got, err := client.PaymentOrder.Get(ctx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusFailed, got.Status)
}

func TestMarkOrderFailedAfterProviderErrorToleratesMissingOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	svc := &PaymentService{entClient: client}

	// Must not panic or surface an error: the caller already has a provider
	// error to report, and this write is pure bookkeeping.
	svc.markOrderFailedAfterProviderError(ctx, 424242, payment.TypeAlipay, context.DeadlineExceeded)
}
