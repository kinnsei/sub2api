//go:build unit

package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/ent/paymentrefund"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// Tests for repeated (partial) refunds and for refusing refunds that cannot be
// sent to the gateway.

func createRefundableOrder(t *testing.T, ctx context.Context, client *dbent.Client, suffix string, amount float64, status string) (*dbent.User, *dbent.PaymentOrder) {
	t.Helper()

	user, err := client.User.Create().
		SetEmail(suffix + "@example.com").
		SetPasswordHash("hash").
		SetUsername(suffix).
		SetBalance(amount).
		Save(ctx)
	require.NoError(t, err)

	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeStripe).
		SetName(suffix + "-provider").
		SetConfig("{}").
		SetSupportedTypes(payment.TypeStripe).
		SetEnabled(true).
		SetRefundEnabled(true).
		SetAllowUserRefund(true).
		Save(ctx)
	require.NoError(t, err)

	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(amount).
		SetPayAmount(amount).
		SetFeeRate(0).
		SetRechargeCode("REFUND-REPEAT-" + suffix).
		SetOutTradeNo("sub2_refund_repeat_" + suffix).
		SetPaymentType(payment.TypeStripe).
		SetPaymentTradeNo("pi_refund_repeat_" + suffix).
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(status).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		SetProviderInstanceID(strconv.FormatInt(inst.ID, 10)).
		Save(ctx)
	require.NoError(t, err)
	return user, order
}

func recordRefundSuccessAudit(t *testing.T, ctx context.Context, client *dbent.Client, orderID int64, amount float64) {
	t.Helper()
	_, err := client.PaymentAuditLog.Create().
		SetOrderID(strconv.FormatInt(orderID, 10)).
		SetAction("REFUND_SUCCESS").
		SetOperator("admin").
		SetDetail(`{"refundAmount":` + strconv.FormatFloat(amount, 'f', -1, 64) + `,"reason":"previous refund"}`).
		Save(ctx)
	require.NoError(t, err)
}

func TestPrepareRefundUsesRemainingAmountAfterPartialRefund(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "repeat-remaining", 100, OrderStatusPartiallyRefunded)
	recordRefundSuccessAudit(t, ctx, client, order.ID, 30)

	svc := &PaymentService{entClient: client}

	plan, result, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.NoError(t, err)
	require.Nil(t, result)
	require.NotNil(t, plan)
	require.Equal(t, 70.0, plan.RefundAmount, "an empty amount must default to the refundable remainder")
	require.Equal(t, 30.0, plan.RefundedBefore)

	_, _, err = svc.PrepareRefund(ctx, order.ID, 80, "", false, false)
	require.Error(t, err)
	require.Equal(t, "REFUND_AMOUNT_EXCEEDED", infraerrors.Reason(err))
}

func TestPrepareRefundRejectsFullyRefundedOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "repeat-exhausted", 100, OrderStatusPartiallyRefunded)
	recordRefundSuccessAudit(t, ctx, client, order.ID, 100)

	svc := &PaymentService{entClient: client}

	_, _, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.Error(t, err)
	require.Equal(t, "REFUND_ALREADY_COMPLETED", infraerrors.Reason(err))
}

func TestPrepareRefundRejectsTerminalRefundedOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "repeat-terminal", 100, OrderStatusRefunded)
	recordRefundSuccessAudit(t, ctx, client, order.ID, 100)

	svc := &PaymentService{entClient: client}

	_, _, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.Error(t, err)
	require.Equal(t, "INVALID_STATUS", infraerrors.Reason(err))
}

func TestPrepareRefundAllowsPartiallyRefundedOrder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "repeat-allowed", 100, OrderStatusPartiallyRefunded)
	recordRefundSuccessAudit(t, ctx, client, order.ID, 40)

	svc := &PaymentService{entClient: client}

	plan, _, err := svc.PrepareRefund(ctx, order.ID, 60, "", false, false)
	require.NoError(t, err)
	require.Equal(t, 60.0, plan.RefundAmount)
}

func TestGwRefundRefusesTradeNoProviderWithoutTradeNo(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "repeat-no-trade-no", 100, OrderStatusCompleted)
	_, err := client.PaymentOrder.UpdateOneID(order.ID).SetPaymentTradeNo("").Save(ctx)
	require.NoError(t, err)
	order, err = client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)

	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{entClient: client, loadBalancer: newWebhookProviderTestLoadBalancer(client)}

	_, err = svc.gwRefund(ctx, &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 100, GatewayAmount: 100, Reason: "no trade no"})
	require.Error(t, err)
	require.Equal(t, "REFUND_NO_TRADE_NO", infraerrors.Reason(err))
	require.Zero(t, prov.refundCalls, "the gateway must not be called without a trade number")

	audits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_NO_TRADE_NO")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, audits)
}

func TestGwRefundAllowsOutTradeNoProviderWithoutTradeNo(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)

	user, err := client.User.Create().
		SetEmail("refund-out-trade-no@example.com").
		SetPasswordHash("hash").
		SetUsername("refund-out-trade-no").
		Save(ctx)
	require.NoError(t, err)

	inst, err := client.PaymentProviderInstance.Create().
		SetProviderKey(payment.TypeAlipay).
		SetName("refund-out-trade-no-provider").
		SetConfig("{}").
		SetSupportedTypes(payment.TypeAlipay).
		SetEnabled(true).
		SetRefundEnabled(true).
		Save(ctx)
	require.NoError(t, err)

	order, err := client.PaymentOrder.Create().
		SetUserID(user.ID).
		SetUserEmail(user.Email).
		SetUserName(user.Username).
		SetAmount(100).
		SetPayAmount(100).
		SetFeeRate(0).
		SetRechargeCode("REFUND-OUT-TRADE-NO").
		SetOutTradeNo("sub2_refund_out_trade_no").
		SetPaymentType(payment.TypeAlipay).
		SetPaymentTradeNo("").
		SetOrderType(payment.OrderTypeBalance).
		SetStatus(OrderStatusCompleted).
		SetExpiresAt(time.Now().Add(time.Hour)).
		SetPaidAt(time.Now()).
		SetClientIP("127.0.0.1").
		SetSrcHost("api.example.com").
		SetProviderInstanceID(strconv.FormatInt(inst.ID, 10)).
		Save(ctx)
	require.NoError(t, err)

	prov := &alipayOutTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "refund-1"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{entClient: client, loadBalancer: newWebhookProviderTestLoadBalancer(client)}

	resp, err := svc.gwRefund(ctx, &RefundPlan{OrderID: order.ID, Order: order, RefundAmount: 100, GatewayAmount: 100, Reason: "out trade no refund"})
	require.NoError(t, err)
	require.Equal(t, payment.ProviderStatusSuccess, resp.Status)
	require.Equal(t, 1, prov.refundCalls)
	require.Equal(t, order.OutTradeNo, prov.lastOrderID)
}

func TestRefundResultStatusUsesCumulativeRefunds(t *testing.T) {
	order := &dbent.PaymentOrder{Amount: 100}

	require.Equal(t, OrderStatusPartiallyRefunded, refundResultStatus(&RefundPlan{
		Order: order, RefundAmount: 40, RefundedBefore: 0,
	}))
	require.Equal(t, OrderStatusPartiallyRefunded, refundResultStatus(&RefundPlan{
		Order: order, RefundAmount: 40, RefundedBefore: 30,
	}))
	require.Equal(t, OrderStatusRefunded, refundResultStatus(&RefundPlan{
		Order: order, RefundAmount: 70, RefundedBefore: 30,
	}))
	require.Equal(t, OrderStatusRefunded, refundResultStatus(&RefundPlan{
		Order: order, RefundAmount: 100, RefundedBefore: 0,
	}))
}

func TestExecuteRefundTwiceMarksPartiallyThenFullyRefunded(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	user, order := createRefundableOrder(t, ctx, client, "repeat-e2e", 100, OrderStatusCompleted)

	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_1"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: user.ID, Email: user.Email, Username: user.Username, Balance: 100},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}

	first, _, err := svc.PrepareRefund(ctx, order.ID, 40, "partial", false, true)
	require.NoError(t, err)
	result, err := svc.ExecuteRefund(ctx, first)
	require.NoError(t, err)
	require.True(t, result.Success)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPartiallyRefunded, reloaded.Status)
	require.Equal(t, 40.0, reloaded.RefundAmount, "refund_amount is the cumulative refunded total")

	second, _, err := svc.PrepareRefund(ctx, order.ID, 0, "rest", false, true)
	require.NoError(t, err)
	require.Equal(t, 60.0, second.RefundAmount)
	result, err = svc.ExecuteRefund(ctx, second)
	require.NoError(t, err)
	require.True(t, result.Success)

	reloaded, err = client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefunded, reloaded.Status)
	require.Equal(t, 100.0, reloaded.RefundAmount)

	// The refunded total lives in the payment_refunds ledger: two installments,
	// summing to the full order amount.
	ledgerRows, err := client.PaymentRefund.Query().
		Where(paymentrefund.OrderIDEQ(order.ID)).
		Order(dbent.Asc(paymentrefund.FieldID)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, ledgerRows, 2, "each refund installment gets its own ledger row")
	require.Equal(t, 40.0, ledgerRows[0].Amount)
	require.Equal(t, 60.0, ledgerRows[1].Amount)
	for _, row := range ledgerRows {
		require.Equal(t, RefundLedgerStatusSucceeded, row.Status)
	}
	require.NotEqual(t, ledgerRows[0].RefundNo, ledgerRows[1].RefundNo, "each installment has its own idempotency key")

	// payment_audit_logs is capped at one row per (order_id, action) by a unique
	// index (migration 131), which is exactly why it can no longer be the source of
	// truth for the refunded total. The audit row is retained only as a readable
	// trace, so its cap must not affect refund correctness.
	successAudits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_SUCCESS")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, successAudits, "audit rows are capped and must not be used as the refund ledger")

	_, _, err = svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.Error(t, err)
	require.Equal(t, "INVALID_STATUS", infraerrors.Reason(err), "a fully refunded order is terminal")
}

// stripeTradeNoRefundDouble mimics a provider whose refunds are bound to the
// upstream trade number (PaymentIntent id).
type stripeTradeNoRefundDouble struct {
	refundResponse *payment.RefundResponse
	refundCalls    int
	lastTradeNo    string
}

func (p *stripeTradeNoRefundDouble) Name() string        { return "stripe-trade-no-double" }
func (p *stripeTradeNoRefundDouble) ProviderKey() string { return payment.TypeStripe }
func (p *stripeTradeNoRefundDouble) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeStripe}
}
func (p *stripeTradeNoRefundDouble) CreatePayment(context.Context, payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	return nil, nil
}
func (p *stripeTradeNoRefundDouble) QueryOrder(context.Context, string) (*payment.QueryOrderResponse, error) {
	return nil, nil
}
func (p *stripeTradeNoRefundDouble) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	return nil, nil
}
func (p *stripeTradeNoRefundDouble) Refund(_ context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	p.refundCalls++
	p.lastTradeNo = req.TradeNo
	return p.refundResponse, nil
}
func (p *stripeTradeNoRefundDouble) RefundRequiresTradeNo() bool { return true }

// alipayOutTradeNoRefundDouble mimics a provider that refunds by out_trade_no.
type alipayOutTradeNoRefundDouble struct {
	refundResponse *payment.RefundResponse
	refundCalls    int
	lastOrderID    string
}

func (p *alipayOutTradeNoRefundDouble) Name() string        { return "alipay-out-trade-no-double" }
func (p *alipayOutTradeNoRefundDouble) ProviderKey() string { return payment.TypeAlipay }
func (p *alipayOutTradeNoRefundDouble) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeAlipay}
}
func (p *alipayOutTradeNoRefundDouble) CreatePayment(context.Context, payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	return nil, nil
}
func (p *alipayOutTradeNoRefundDouble) QueryOrder(context.Context, string) (*payment.QueryOrderResponse, error) {
	return nil, nil
}
func (p *alipayOutTradeNoRefundDouble) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	return nil, nil
}
func (p *alipayOutTradeNoRefundDouble) Refund(_ context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	p.refundCalls++
	p.lastOrderID = req.OrderID
	return p.refundResponse, nil
}

// TestOverRefundIsPreventedAcrossThreePartialRefunds is the regression test for the
// P0 over-refund defect.
//
// payment_audit_logs carries a unique index on (order_id, action) (migration 131),
// but the refund code used to treat REFUND_SUCCESS audit rows as a multi-row
// ledger while writeAuditLog silently swallowed insert failures. The second and
// later REFUND_SUCCESS rows therefore vanished, so the refunded total stayed at the
// first installment and a blank-amount refund defaulted to a remaining budget that
// was too large:
//
//	100 order, refunds of 40 then 30, then a blank-amount refund computed a
//	remaining of 60 (instead of 30) and sent it to the gateway -> 130 total.
//
// This test drives that exact sequence and asserts the invariant that the sum of
// everything sent to the gateway never exceeds the order amount.
func TestOverRefundIsPreventedAcrossThreePartialRefunds(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "over-refund", 100, OrderStatusCompleted)

	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_over"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}

	// Installment 1: explicit 40.
	first, _, err := svc.PrepareRefund(ctx, order.ID, 40, "first", false, true)
	require.NoError(t, err)
	result, err := svc.ExecuteRefund(ctx, first)
	require.NoError(t, err)
	require.True(t, result.Success)

	// Installment 2: explicit 30 against the remaining 60.
	second, _, err := svc.PrepareRefund(ctx, order.ID, 30, "second", false, true)
	require.NoError(t, err)
	require.Equal(t, 30.0, second.RefundAmount)
	result, err = svc.ExecuteRefund(ctx, second)
	require.NoError(t, err)
	require.True(t, result.Success)

	// The refunded total must be 70, so the remaining budget is 30 — not 60.
	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPartiallyRefunded, reloaded.Status)
	require.Equal(t, 70.0, reloaded.RefundAmount, "refund_amount accumulates every installment")

	// Installment 3: blank amount must default to the true remainder (30).
	third, _, err := svc.PrepareRefund(ctx, order.ID, 0, "rest", false, true)
	require.NoError(t, err)
	require.Equal(t, 30.0, third.RefundAmount, "the blank amount must not reuse a stale, too-large remainder")

	result, err = svc.ExecuteRefund(ctx, third)
	require.NoError(t, err)
	require.True(t, result.Success)

	// The invariant that the P0 defect violated: never refund more than the order.
	ledgerRows, err := client.PaymentRefund.Query().
		Where(paymentrefund.OrderIDEQ(order.ID), paymentrefund.StatusEQ(RefundLedgerStatusSucceeded)).
		All(ctx)
	require.NoError(t, err)
	var refundedTotal float64
	for _, row := range ledgerRows {
		refundedTotal += row.Amount
	}
	require.Equal(t, 100.0, refundedTotal, "the gateway must never be asked to refund more than the order amount")

	// A fourth refund must be refused: the order is fully refunded and terminal.
	_, _, err = svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.Error(t, err)
	require.Equal(t, "INVALID_STATUS", infraerrors.Reason(err))
}

// TestOverRefundRejectedWhenAmountExceedsRemainder asserts the explicit-amount
// guard still uses the ledger total (not the capped audit rows).
func TestOverRefundRejectedWhenAmountExceedsRemainder(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "over-refund-explicit", 100, OrderStatusCompleted)

	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_cap"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}

	first, _, err := svc.PrepareRefund(ctx, order.ID, 60, "first", false, true)
	require.NoError(t, err)
	_, err = svc.ExecuteRefund(ctx, first)
	require.NoError(t, err)

	// 50 exceeds the 40 that remains after a 60 refund.
	_, _, err = svc.PrepareRefund(ctx, order.ID, 50, "", false, true)
	require.Error(t, err)
	require.Equal(t, "REFUND_AMOUNT_EXCEEDED", infraerrors.Reason(err))

	// Exactly 40 is still allowed.
	second, _, err := svc.PrepareRefund(ctx, order.ID, 40, "", false, true)
	require.NoError(t, err)
	require.Equal(t, 40.0, second.RefundAmount)
}

// TestStuckRefundingOrderIsRecoverable is the regression test for the REFUNDING
// dead state.
//
// REFUNDING is set before the gateway refund call and cleared by its result. If the
// process exits in between, the order stays REFUNDING forever: PrepareRefund's
// allow-list did not include REFUNDING, and no sweeper handled it, so the order
// could neither be refunded nor restored — with the user's balance already deducted.
func TestStuckRefundingOrderIsRecoverable(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "stuck-refunding", 100, OrderStatusRefunding)
	// Make it stale so the sweeper picks it up.
	_, err := client.PaymentOrder.UpdateOneID(order.ID).SetUpdatedAt(time.Now().Add(-time.Hour)).Save(ctx)
	require.NoError(t, err)

	// An in-flight installment that never reached the gateway.
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(40).
		SetGatewayAmount(40).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}

	released, err := svc.SweepStuckRefunds(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, released)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusCompleted, reloaded.Status, "an interrupted refund with nothing refunded must become refundable again")

	// The in-flight installment must be gone, otherwise the partial unique index
	// would keep blocking every later refund attempt on this order.
	left, err := client.PaymentRefund.Query().Where(paymentrefund.OrderIDEQ(order.ID)).Count(ctx)
	require.NoError(t, err)
	require.Zero(t, left, "the stuck installment must be released")

	audits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_STUCK_RELEASED")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, audits)

	// And a fresh refund can now proceed.
	plan, _, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.NoError(t, err)
	require.Equal(t, 100.0, plan.RefundAmount)
}

// TestStuckRefundingOrderKeepsPartialRefundRefundable asserts the sweeper does not
// erase refund progress: if an installment already succeeded, the order must come
// back as PARTIALLY_REFUNDED with the correct remaining budget.
func TestStuckRefundingOrderKeepsPartialRefundRefundable(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "stuck-partial", 100, OrderStatusRefunding)
	_, err := client.PaymentOrder.UpdateOneID(order.ID).
		SetUpdatedAt(time.Now().Add(-time.Hour)).
		SetRefundAmount(30).
		Save(ctx)
	require.NoError(t, err)

	// One installment already succeeded...
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(30).
		SetGatewayAmount(30).
		SetStatus(RefundLedgerStatusSucceeded).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)
	// ...and a second one got stuck before reaching the gateway.
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":1").
		SetAmount(70).
		SetGatewayAmount(70).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}

	released, err := svc.SweepStuckRefunds(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, released)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPartiallyRefunded, reloaded.Status)

	// The succeeded installment survives; only the stuck one is released.
	rows, err := client.PaymentRefund.Query().Where(paymentrefund.OrderIDEQ(order.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, RefundLedgerStatusSucceeded, rows[0].Status)
	require.Equal(t, 30.0, rows[0].Amount)

	// The remaining budget must still account for the 30 already refunded.
	plan, _, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, false)
	require.NoError(t, err)
	require.Equal(t, 70.0, plan.RefundAmount)
}

// TestRefundingOrderCanBeReDrivenDirectly covers the other half of the REFUNDING
// recovery story: an admin does not have to wait for the periodic sweeper, because
// REFUNDING is accepted by the refund entry points and ExecuteRefund's status CAS.
//
// This is load-bearing for two changes: PrepareRefund's allow-list must include
// REFUNDING, and ExecuteRefund's CAS predicate must too. Without them a refund that
// died mid-flight could only be recovered by the sweeper, and only after the grace
// period, during which the user's balance is already deducted.
func TestRefundingOrderCanBeReDrivenDirectly(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "redrive-refunding", 100, OrderStatusRefunding)

	// The installment that was claimed before the process died.
	_, err := client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(100).
		SetGatewayAmount(100).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)

	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_redrive"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}

	// The admin can prepare a refund for a REFUNDING order without waiting for the sweep.
	plan, _, err := svc.PrepareRefund(ctx, order.ID, 100, "redrive", false, true)
	require.NoError(t, err)
	require.NotNil(t, plan)

	result, err := svc.ExecuteRefund(ctx, plan)
	require.NoError(t, err)
	require.True(t, result.Success)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefunded, reloaded.Status)

	// The re-drive reused the existing installment instead of adding a second one,
	// so the refunded total cannot double-count the same refund.
	rows, err := client.PaymentRefund.Query().
		Where(paymentrefund.OrderIDEQ(order.ID), paymentrefund.StatusEQ(RefundLedgerStatusSucceeded)).
		All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1, "re-driving a refund must refine the existing installment")
	require.Equal(t, 100.0, rows[0].Amount)
}

// TestRefundRetryReusesGatewayRefundReference asserts a re-driven refund sends the
// same gateway-facing refund reference on the retry.
//
// The installment number doubles as the provider idempotency reference (Alipay
// out_request_no). Deriving it deterministically is what makes a retry recognisable
// as the same request rather than a second, independent refund.
func TestRefundRetryReusesGatewayRefundReference(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "refund-ref-reuse", 100, OrderStatusCompleted)

	prov := &refundReferenceCapturingDouble{
		refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_ref"},
	}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}

	first, _, err := svc.PrepareRefund(ctx, order.ID, 40, "first", false, true)
	require.NoError(t, err)
	_, err = svc.ExecuteRefund(ctx, first)
	require.NoError(t, err)
	require.Len(t, prov.refundNos, 1)
	firstRef := prov.refundNos[0]
	require.NotEmpty(t, firstRef, "the gateway must receive a refund reference")
	// Alipay out_request_no accepts only letters, digits and underscores.
	require.Regexp(t, `^[A-Za-z0-9_]+$`, firstRef)

	// Simulate a re-drive of the *same* installment: an order stuck in REFUNDING
	// with its ledger row still in flight.
	_, err = client.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusRefunding).Save(ctx)
	require.NoError(t, err)
	stuck, err := client.PaymentRefund.Query().Where(paymentrefund.OrderIDEQ(order.ID)).Only(ctx)
	require.NoError(t, err)
	_, err = client.PaymentRefund.UpdateOneID(stuck.ID).SetStatus(RefundLedgerStatusRefunding).Save(ctx)
	require.NoError(t, err)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	rePlan, _, err := svc.PrepareRefund(ctx, order.ID, 0, "redrive", false, true)
	require.NoError(t, err)
	// Restore the status so ExecuteRefund's CAS accepts the retry.
	_, err = client.PaymentOrder.UpdateOneID(order.ID).SetStatus(OrderStatusRefunding).Save(ctx)
	require.NoError(t, err)
	rePlan.Order = reloaded
	_, err = svc.ExecuteRefund(ctx, rePlan)
	require.NoError(t, err)

	require.Len(t, prov.refundNos, 2, "the retry must reach the gateway")
	require.Equal(t, firstRef, prov.refundNos[1],
		"a retry of the same installment must reuse the same gateway reference")

	// The retry refined the same ledger row instead of adding a second installment.
	rows, err := client.PaymentRefund.Query().Where(paymentrefund.OrderIDEQ(order.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, firstRef, rows[0].RefundNo)
}

// refundReferenceCapturingDouble records the refund reference each gateway call saw.
type refundReferenceCapturingDouble struct {
	refundResponse *payment.RefundResponse
	refundNos      []string
}

func (p *refundReferenceCapturingDouble) Name() string        { return "refund-reference-double" }
func (p *refundReferenceCapturingDouble) ProviderKey() string { return payment.TypeStripe }
func (p *refundReferenceCapturingDouble) SupportedTypes() []payment.PaymentType {
	return []payment.PaymentType{payment.TypeStripe}
}
func (p *refundReferenceCapturingDouble) CreatePayment(context.Context, payment.CreatePaymentRequest) (*payment.CreatePaymentResponse, error) {
	return nil, nil
}
func (p *refundReferenceCapturingDouble) QueryOrder(context.Context, string) (*payment.QueryOrderResponse, error) {
	return nil, nil
}
func (p *refundReferenceCapturingDouble) VerifyNotification(context.Context, string, map[string]string) (*payment.PaymentNotification, error) {
	return nil, nil
}
func (p *refundReferenceCapturingDouble) Refund(_ context.Context, req payment.RefundRequest) (*payment.RefundResponse, error) {
	p.refundNos = append(p.refundNos, req.RefundNo)
	return p.refundResponse, nil
}

// TestStuckRefundWithUnrolledBackDeductionIsNotReleased is the regression test for
// the double-deduction defect in the REFUNDING sweeper.
//
// The crash window is *between* the balance deduction and the gateway response.
// deduction_rollback_ok is therefore set to false BEFORE the deduction runs, because
// the amounts on the row (balance_deducted/sub_days_deducted) are only written once
// the whole refund finishes and would still be 0 in exactly that window. Relying on
// the amounts alone would make the sweeper believe nothing happened, delete the
// installment, mark the order COMPLETED, and let the next refund deduct the user's
// balance a second time.
func TestStuckRefundWithUnrolledBackDeductionIsNotReleased(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "stuck-deduction", 100, OrderStatusRefunding)
	_, err := client.PaymentOrder.UpdateOneID(order.ID).SetUpdatedAt(time.Now().Add(-time.Hour)).Save(ctx)
	require.NoError(t, err)

	// The exact crash state: the pre-deduction marker is set, but no amount was
	// recorded yet because the refund never completed.
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(40).
		SetGatewayAmount(40).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		SetBalanceDeducted(0).
		SetSubDaysDeducted(0).
		SetDeductionRollbackOk(false).
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}

	released, err := svc.SweepStuckRefunds(ctx)
	require.NoError(t, err)
	require.Zero(t, released, "an order whose deduction may not be rolled back must not be auto-released")

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefundFailed, reloaded.Status,
		"the order must be parked for manual review, not returned to a refundable state")
	require.Contains(t, psStringValue(reloaded.FailedReason), "may not be rolled back")

	// The in-flight installment must survive so its partial unique index keeps
	// blocking a new refund on this order.
	left, err := client.PaymentRefund.Query().Where(paymentrefund.OrderIDEQ(order.ID)).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, left, "the blocking installment must be preserved")

	audits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_STUCK_NEEDS_REVIEW")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, audits)
}

// TestRetryDoesNotDeductAgainAfterInterruptedRefund asserts the retry path skips the
// deduction when the durable marker says a previous attempt may already have taken
// the money.
func TestRetryDoesNotDeductAgainAfterInterruptedRefund(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "no-double-deduct", 100, OrderStatusRefunding)

	// The pre-deduction marker, as left by a crash in the deduction/gateway window.
	_, err := client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(100).
		SetGatewayAmount(100).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		SetDeductionRollbackOk(false).
		Save(ctx)
	require.NoError(t, err)

	deductCalls := 0
	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				deductCalls++
				return amount, nil
			},
		},
	}
	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_nodedup"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	plan, _, err := svc.PrepareRefund(ctx, order.ID, 100, "retry", false, true)
	require.NoError(t, err)
	_, err = svc.ExecuteRefund(ctx, plan)
	require.NoError(t, err)

	require.Zero(t, deductCalls,
		"a retry after an interrupted deduction must not deduct the user's balance again")
}

// TestUserRequestedRefundRemainsApprovable is the regression test for the
// refund_amount semantics defect (Phase 2.8).
//
// RequestRefund wrote the *requested* amount into payment_orders.refund_amount,
// which everywhere else means "cumulative successfully refunded amount". The
// pre-ledger fallback (legacyRefundedTotal) takes the larger of the audit total
// and that column, so a user-requested refund immediately looked fully refunded:
// the admin could never approve it and it sat in REFUND_REQUESTED forever.
func TestUserRequestedRefundRemainsApprovable(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	user, order := createRefundableOrder(t, ctx, client, "user-requested", 100, OrderStatusCompleted)

	svc := &PaymentService{
		entClient: client,
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: user.ID, Email: user.Email, Username: user.Username, Balance: 1000},
		},
	}

	require.NoError(t, svc.RequestRefund(ctx, order.ID, user.ID, "I want my money back"))

	requested, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefundRequested, requested.Status)

	// The request itself must not consume any of the refundable budget.
	plan, _, err := svc.PrepareRefund(ctx, order.ID, 0, "", false, true)
	require.NoError(t, err, "a user-requested refund must remain approvable by an admin")
	require.Equal(t, 100.0, plan.RefundAmount, "the full amount is still refundable")
	require.Equal(t, 0.0, plan.RefundedBefore, "requesting a refund must not mark it as refunded")
}

// TestStaleRefundPlanCannotOverRefund is the regression test for the
// prepare/execute atomicity gap (Phase 6.3).
//
// The admin handler calls PrepareRefund and ExecuteRefund as two separate steps.
// PrepareRefund computes the refundable remainder from a snapshot, so a plan can go
// stale: if another refund succeeds in between, the stale plan still carries the
// full order amount. ExecuteRefund's status CAS serialises the two calls but cannot
// see that the amount was computed before the earlier refund landed, so without a
// fresh read the second refund would be paid out on top of the first.
func TestStaleRefundPlanCannotOverRefund(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "stale-plan", 100, OrderStatusCompleted)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}
	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_stale"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	// Two admins each prepare against a freshly created order: A for 60, and B for
	// the remaining 100 it believed was still refundable. Both plans therefore
	// carry refundedBefore = 0.
	planA, _, err := svc.PrepareRefund(ctx, order.ID, 60, "first", false, true)
	require.NoError(t, err)
	planB, _, err := svc.PrepareRefund(ctx, order.ID, 100, "second", false, true)
	require.NoError(t, err)
	require.Equal(t, 60.0, planA.RefundAmount)
	require.Equal(t, 100.0, planB.RefundAmount, "both plans were computed before either refund landed")

	// The first refund succeeds, leaving the order PARTIALLY_REFUNDED — still an
	// allowed status for a second refund, which is exactly why the status CAS alone
	// cannot catch the stale plan.
	_, err = svc.ExecuteRefund(ctx, planA)
	require.NoError(t, err)

	// The stale plan claims 100 more, but only 40 remains. It must be rejected
	// instead of paying the gateway on top of the first refund.
	_, err = svc.ExecuteRefund(ctx, planB)
	require.Error(t, err, "a stale plan must not be able to refund beyond the remainder")
	require.Contains(t, err.Error(), "REFUND_AMOUNT_EXCEEDED")

	// The order must not have been over-refunded (60 + 100 = 160 > 100).
	total, err := svc.refundSucceededTotal(ctx, order)
	require.NoError(t, err)
	require.Equal(t, 60.0, total, "the order must never be refunded beyond its amount")

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusPartiallyRefunded, reloaded.Status)
	require.Equal(t, 60.0, reloaded.RefundAmount)

	// And the correct remainder is still refundable afterwards.
	planC, _, err := svc.PrepareRefund(ctx, order.ID, 0, "remainder", false, true)
	require.NoError(t, err)
	require.Equal(t, 40.0, planC.RefundAmount)
}

// TestRefundedTotalNeverExceedsOrderAmount is the Phase 2.9 invariant.
//
// The ledger replaced the audit-row count as the source of truth for "already
// refunded". This asserts the property that source of truth must never violate:
// across any sequence of partial refunds — including over-refund attempts that are
// rejected and re-drives of a stuck installment — the successfully refunded total
// can never exceed the order amount.
func TestRefundedTotalNeverExceedsOrderAmount(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "invariant", 100, OrderStatusCompleted)

	svc := &PaymentService{
		entClient:    client,
		loadBalancer: newWebhookProviderTestLoadBalancer(client),
		userRepo: &mockUserRepo{
			getByIDUser: &User{ID: order.UserID, Email: order.UserEmail, Username: order.UserName, Balance: 1000},
			deductAvailableBalanceFn: func(_ context.Context, _ int64, amount float64) (float64, error) {
				return amount, nil
			},
		},
	}
	prov := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_inv"}}
	restore := replacePaymentProviderFactoryForTest(t, prov)
	t.Cleanup(restore)

	// A sequence of partial refunds followed by an over-refund attempt.
	for _, amount := range []float64{25, 25, 25, 25, 1 /* already exhausted */} {
		plan, _, err := svc.PrepareRefund(ctx, order.ID, amount, "partial", false, true)
		if err != nil {
			// Rejections are fine; the invariant is about what actually lands.
			continue
		}
		if _, err := svc.ExecuteRefund(ctx, plan); err != nil {
			continue
		}

		total, err := svc.refundSucceededTotal(ctx, order)
		require.NoError(t, err)
		require.LessOrEqual(t, total, order.Amount+0.001,
			"refunded total %v must never exceed the order amount %v (after refunding %v)", total, order.Amount, amount)
	}

	final, err := svc.refundSucceededTotal(ctx, order)
	require.NoError(t, err)
	require.InDelta(t, 100.0, final, 0.001, "the four 25 refunds must have gone through")
}

// TestStuckRefundingOrderAlreadyFullyRefundedBecomesRefunded pins the boundary
// between the two released statuses. A stuck order whose successful installments
// already cover the whole amount must come back as REFUNDED, not
// PARTIALLY_REFUNDED: the latter leaves remaining budget (order amount minus the
// ledger total), and PrepareRefund would then let an admin refund the order again
// on top of money already returned. The sweeper and restoreStatusFromLedger must
// agree, or the two recovery paths disagree about re-refundability.
func TestStuckRefundingOrderAlreadyFullyRefundedBecomesRefunded(t *testing.T) {
	ctx := context.Background()
	client := newPaymentConfigServiceTestClient(t)
	_, order := createRefundableOrder(t, ctx, client, "stuck-full", 100, OrderStatusRefunding)
	_, err := client.PaymentOrder.UpdateOneID(order.ID).
		SetUpdatedAt(time.Now().Add(-time.Hour)).
		Save(ctx)
	require.NoError(t, err)

	// The ledger already accounts for the entire order amount...
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":0").
		SetAmount(60).
		SetGatewayAmount(60).
		SetStatus(RefundLedgerStatusSucceeded).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":1").
		SetAmount(40).
		SetGatewayAmount(40).
		SetStatus(RefundLedgerStatusSucceeded).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)
	// ...and one more installment got stuck before reaching the gateway.
	_, err = client.PaymentRefund.Create().
		SetOrderID(order.ID).
		SetRefundNo(orderIDString(order.ID) + ":2").
		SetAmount(10).
		SetGatewayAmount(10).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator("admin").
		Save(ctx)
	require.NoError(t, err)

	svc := &PaymentService{entClient: client}
	released, err := svc.SweepStuckRefunds(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, released)

	reloaded, err := client.PaymentOrder.Get(ctx, order.ID)
	require.NoError(t, err)
	require.Equal(t, OrderStatusRefunded, reloaded.Status,
		"an order whose ledger already covers the full amount must not be released as refundable again")

	// A further refund must be impossible: there is no remaining budget.
	planSvc := &PaymentService{entClient: client, loadBalancer: newWebhookProviderTestLoadBalancer(client)}
	provider := &stripeTradeNoRefundDouble{refundResponse: &payment.RefundResponse{Status: payment.ProviderStatusSuccess, RefundID: "re_full"}}
	restore := replacePaymentProviderFactoryForTest(t, provider)
	t.Cleanup(restore)
	plan, _, err := planSvc.PrepareRefund(ctx, order.ID, 0, "again", false, true)
	if err == nil {
		require.Zero(t, plan.RefundAmount, "no budget may remain on a fully refunded order")
	}
}
