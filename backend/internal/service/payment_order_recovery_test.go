//go:build unit

package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Tests for the payment recovery paths: cancelling/expiring only when the
// upstream state is verified, reconciling EasyPay orders, fulfilling payments
// that arrive after expiry, and auto-retrying stuck fulfillments.

func newPaymentRecoveryTestUser(t *testing.T, ctx context.Context, client *dbent.Client, suffix string) *dbent.User {
	t.Helper()
	user, err := client.User.Create().
		SetEmail(suffix + "@example.com").
		SetPasswordHash("hash").
		SetUsername(suffix).
		Save(ctx)
	require.NoError(t, err)
	return user
}

func newPaymentRecoveryTestOrder(client *dbent.Client, user *dbent.User, suffix, paymentType, status string) *dbent.PaymentOrderCreate {
	return client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(100).
		SetPayAmount(100).
		SetFeeRate(0).
		SetRechargeCode("RECOVERY-" + suffix).
		SetOutTradeNo("sub2_" + suffix).
		SetPaymentType(paymentType).
		SetPaymentTradeNo("").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(status).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com")
}

func newPaymentRecoveryTestService(
	t *testing.T,
	client *dbent.Client,
	prov payment.Provider,
) (*PaymentService, *mockUserRepo, *paymentOrderLifecycleRedeemRepo) {
	t.Helper()

	userRepo := &mockUserRepo{}
	userRepo.updateBalanceFn = func(_ context.Context, id int64, amount float64) error {
		if userRepo.getByIDUser != nil && userRepo.getByIDUser.ID == id {
			userRepo.getByIDUser.Balance += amount
		}
		return nil
	}
	redeemRepo := &paymentOrderLifecycleRedeemRepo{codesByCode: map[string]*RedeemCode{}}
	registry := payment.NewRegistry()
	if prov != nil {
		registry.Register(prov)
	}
	svc := &PaymentService{
		entClient:       client,
		registry:        registry,
		redeemService:   NewRedeemService(redeemRepo, userRepo, nil, nil, nil, client, nil, nil),
		userRepo:        userRepo,
		providersLoaded: true,
	}
	return svc, userRepo, redeemRepo
}

// setTestUserBalance wires mockUserRepo to a user row so fulfillment can credit it.
func setTestUserBalance(userRepo *mockUserRepo, user *dbent.User) {
	userRepo.getByIDUser = &User{
		ID:       user.ID,
		Email:    user.Email,
		Username: user.Username,
		Balance:  0,
	}
}

func TestCancelOrderRefusesCancelWhenUpstreamStatusUnverified(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "cancel-unverified")

	order, err := newPaymentRecoveryTestOrder(client, user, "cancel_unverified", payment.TypeAlipay, OrderStatusPending).
		Save(ctx)
	require.NoError(t, err)

	registry := payment.NewRegistry()
	provider := &paymentOrderLifecycleQueryProvider{queryErr: errors.New("upstream timeout")}
	registry.Register(provider)

	svc := &PaymentService{entClient: client, registry: registry, providersLoaded: true}

	_, err = svc.CancelOrder(ctx, order.ID, user.ID)
	require.Error(t, err)
	require.Equal(t, "PAYMENT_STATUS_UNVERIFIED", infraerrors.Reason(err))
	require.Zero(t, provider.cancelCalls)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, reloaded.Status)
}

func TestExpireTimedOutOrdersSkipsUnverifiedOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "expire-unverified")

	order, err := newPaymentRecoveryTestOrder(client, user, "expire_unverified", payment.TypeAlipay, OrderStatusPending).
		SetExpiresAt(time.Now().Add(-time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	registry := payment.NewRegistry()
	provider := &paymentOrderLifecycleQueryProvider{queryErr: errors.New("upstream unavailable")}
	registry.Register(provider)

	svc := &PaymentService{entClient: client, registry: registry, providersLoaded: true}

	expired, err := svc.ExpireTimedOutOrders(ctx)
	require.NoError(t, err)
	require.Zero(t, expired)
	require.Zero(t, provider.cancelCalls)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPending, reloaded.Status, "order must stay pending until the upstream state is verified")
}

func TestExpireTimedOutOrdersExpiresUnpaidOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "expire-unpaid")

	order, err := newPaymentRecoveryTestOrder(client, user, "expire_unpaid", payment.TypeAlipay, OrderStatusPending).
		SetExpiresAt(time.Now().Add(-time.Minute)).
		Save(ctx)
	require.NoError(t, err)

	registry := payment.NewRegistry()
	provider := &paymentOrderLifecycleQueryProvider{
		resp: &payment.QueryOrderResponse{Status: payment.ProviderStatusPending},
	}
	registry.Register(provider)

	svc := &PaymentService{entClient: client, registry: registry, providersLoaded: true}

	expired, err := svc.ExpireTimedOutOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, expired)
	require.Equal(t, 1, provider.cancelCalls)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusExpired, reloaded.Status)
}

func TestReconcilePendingPaymentOrdersIncludesEasyPayProviderOrders(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "easypay-reconcile")

	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeEasyPay).
		SetName("easypay-reconcile-instance").
		SetConfig(encryptWebhookProviderConfig(t, map[string]string{
			"pid":       "1001",
			"pkey":      "easypay-key",
			"apiBase":   "https://easypay.example.com",
			"notifyUrl": "https://api.example.com/api/v1/payment/webhook/easypay",
			"returnUrl": "https://example.com/payment/result",
		})).
		SetSupportedTypes(payment.TypeAlipay).
		SetEnabled(true).
		SetRefundEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	order, err := newPaymentRecoveryTestOrder(client, user, "easypay_reconcile", payment.TypeAlipay, OrderStatusPending).
		SetProviderInstanceID(strconv.FormatInt(inst.ID, 10)).
		SetProviderKey(payment.TypeEasyPay).
		Save(ctx)
	require.NoError(t, err)

	prov := &paymentOrderLifecycleQueryProvider{
		key: payment.TypeEasyPay,
		resp: &payment.QueryOrderResponse{
			TradeNo: "easypay-upstream-trade",
			Status:  payment.ProviderStatusPaid,
			Amount:  100,
		},
	}
	svc, userRepo, redeemRepo := newPaymentRecoveryTestService(t, client, prov)
	svc.loadBalancer = newWebhookProviderTestLoadBalancer(client)
	setTestUserBalance(userRepo, user)
	redeemRepo.codesByCode[order.RechargeCode] = &RedeemCode{
		ID:     1,
		Code:   order.RechargeCode,
		Type:   RedeemTypeBalance,
		Value:  order.Amount,
		Status: StatusUnused,
	}
	restoreFactory := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restoreFactory)

	recovered, err := svc.ReconcilePendingPaymentOrders(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, recovered)
	require.Equal(t, order.OutTradeNo, prov.lastQueryTradeNo)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, reloaded.Status)
	require.Equal(t, 100.0, userRepo.getByIDUser.Balance)
}

func TestPaidNotificationRecoversOrderExpiredBeyondGrace(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "late-payment")

	order, err := newPaymentRecoveryTestOrder(client, user, "late_payment", payment.TypeAlipay, OrderStatusExpired).
		SetUpdatedAt(time.Now().Add(-6 * time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	svc, userRepo, redeemRepo := newPaymentRecoveryTestService(t, client, nil)
	setTestUserBalance(userRepo, user)
	redeemRepo.codesByCode[order.RechargeCode] = &RedeemCode{
		ID:     1,
		Code:   order.RechargeCode,
		Type:   RedeemTypeBalance,
		Value:  order.Amount,
		Status: StatusUnused,
	}

	err = svc.HandlePaymentNotification(ctx, &payment.PaymentNotification{
		TradeNo: "alipay-late-trade",
		OrderID: order.OutTradeNo,
		Amount:  order.PayAmount,
		Status:  payment.NotificationStatusSuccess,
	}, payment.TypeAlipay)
	require.NoError(t, err)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, reloaded.Status, "a provider-confirmed payment must be fulfilled even after expiry")
	require.Equal(t, "alipay-late-trade", reloaded.PaymentTradeNo)
	require.Equal(t, 100.0, userRepo.getByIDUser.Balance)

	recoveredAudits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("ORDER_RECOVERED")).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, recoveredAudits, 1)
}

func TestRetryStuckFulfillmentsRecoversPaidOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "stuck-paid")

	order, err := newPaymentRecoveryTestOrder(client, user, "stuck_paid", payment.TypeAlipay, OrderStatusPaid).
		SetPaymentTradeNo("alipay-stuck-trade").
		SetPaidAt(time.Now().Add(-time.Hour)).
		SetUpdatedAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	svc, userRepo, redeemRepo := newPaymentRecoveryTestService(t, client, nil)
	setTestUserBalance(userRepo, user)
	redeemRepo.codesByCode[order.RechargeCode] = &RedeemCode{
		ID:     1,
		Code:   order.RechargeCode,
		Type:   RedeemTypeBalance,
		Value:  order.Amount,
		Status: StatusUnused,
	}

	retried, err := svc.RetryStuckFulfillments(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, retried)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, reloaded.Status)
	require.Equal(t, 100.0, userRepo.getByIDUser.Balance)
}

func TestRetryStuckFulfillmentsStopsAfterAttemptBudget(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "stuck-budget")

	order, err := newPaymentRecoveryTestOrder(client, user, "stuck_budget", payment.TypeAlipay, OrderStatusFailed).
		SetPaymentTradeNo("alipay-stuck-budget-trade").
		SetPaidAt(time.Now().Add(-time.Hour)).
		SetUpdatedAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	// The retry budget is a persisted counter, not a count of FULFILLMENT_FAILED
	// audit rows: payment_audit_logs is capped at one row per (order_id, action) by
	// a unique index, so an audit-based count could never reach the budget and the
	// order would be retried forever. This test used to insert 5 audit rows, which
	// the production schema makes impossible.
	_, err = client.PaymentOrder.UpdateOneID(order.ID).
		SetFulfillmentAttempts(paymentFulfillmentMaxAutoAttempts).
		Save(ctx)
	require.NoError(t, err)

	svc, _, redeemRepo := newPaymentRecoveryTestService(t, client, nil)
	redeemRepo.codesByCode[order.RechargeCode] = &RedeemCode{
		ID:     1,
		Code:   order.RechargeCode,
		Type:   RedeemTypeBalance,
		Value:  order.Amount,
		Status: StatusUnused,
	}

	retried, err := svc.RetryStuckFulfillments(ctx)
	require.NoError(t, err)
	require.Zero(t, retried)

	autoRetries, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("FULFILLMENT_AUTO_RETRY")).
		All(ctx)
	require.NoError(t, err)
	require.Empty(t, autoRetries, "exhausted orders must be left for manual admin review")

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusFailed, reloaded.Status)
}

// TestFulfillmentRetryBudgetIsReachableUnderProductionAuditConstraint is the
// regression test for the infinite-retry defect.
//
// The retry budget used to be derived from the number of FULFILLMENT_FAILED audit
// rows. payment_audit_logs has a unique index on (order_id, action) (migration 131),
// so at most one such row can exist and the count could never reach the budget of
// paymentFulfillmentMaxAutoAttempts. A permanently failing order was therefore
// retried on every sweep forever.
//
// This test drives the real failure path (markFailed over the lease) the maximum
// number of times and asserts the order is subsequently excluded from the sweep.
func TestFulfillmentRetryBudgetIsReachableUnderProductionAuditConstraint(t *testing.T) {
	ctx := context.Background()
	client := newPaymentOrderLifecycleTestClient(t)
	user := newPaymentRecoveryTestUser(t, ctx, client, "budget-reachable")

	order, err := newPaymentRecoveryTestOrder(client, user, "budget_reachable", payment.TypeAlipay, OrderStatusRecharging).
		SetPaymentTradeNo("alipay-budget-reachable-trade").
		SetPaidAt(time.Now().Add(-time.Hour)).
		SetUpdatedAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}

	// Drive the real markFailed path once per attempt budget. Each call consumes one
	// attempt via the lease-conditional update.
	for i := 0; i < paymentFulfillmentMaxAutoAttempts; i++ {
		current, err := client.PaymentOrder.Get(ctx, order.ID)
		require.NoError(t, err)
		lease := &paymentFulfillmentLease{version: current.UpdatedAt}
		// markFailed only applies while the order is RECHARGING at the lease version.
		_, err = client.PaymentOrder.UpdateOneID(order.ID).
			SetStatus(OrderStatusRecharging).
			SetUpdatedAt(current.UpdatedAt).
			Save(ctx)
		require.NoError(t, err)

		svc.markFailed(ctx, order.ID, lease, errors.New("boom"))

		reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
		require.NoError(t, err)
		require.Equal(t, i+1, reloaded.FulfillmentAttempts, "each real failure must consume exactly one attempt")
	}

	// At most one audit row can exist despite 5 failures — this is the constraint
	// that broke the old audit-counting implementation.
	failAudits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("FULFILLMENT_FAILED")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, failAudits, "the audit index caps FULFILLMENT_FAILED at one row")

	require.Equal(t, paymentFulfillmentMaxAutoAttempts, svc.fulfillmentFailureCount(ctx, order.ID),
		"the budget must be reachable; an audit-based count would be stuck at 1")
}
