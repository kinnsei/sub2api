//go:build unit

package service

import (
	"context"
	"strconv"
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
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

	successAudits, err := client.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(order.ID, 10)), paymentauditlog.ActionEQ("REFUND_SUCCESS")).
		Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, successAudits)

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
