package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentauditlog"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/ent/paymentrefund"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	"github.com/Wei-Shaw/sub2api/internal/payment/provider"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/servertiming"
)

// --- Refund Flow ---

var createPaymentProviderFromInstance = provider.CreateProvider

// getOrderProviderInstance looks up the provider instance that processed this order.
// For legacy orders without provider_instance_id, it resolves only when the
// historical instance is uniquely identifiable from the stored order fields.
func (s *PaymentService) getOrderProviderInstance(ctx context.Context, o *dbent.PaymentOrder) (*dbent.PaymentProviderInstance, error) {
	if s == nil || s.entClient == nil || o == nil {
		return nil, nil
	}

	if snapshot := psOrderProviderSnapshot(o); snapshot != nil {
		return s.resolveSnapshotOrderProviderInstance(ctx, o, snapshot)
	}

	instIDStr := strings.TrimSpace(psStringValue(o.ProviderInstanceID))
	if instIDStr == "" {
		return s.resolveUniqueLegacyOrderProviderInstance(ctx, o)
	}

	instID, err := strconv.ParseInt(instIDStr, 10, 64)
	if err != nil {
		return nil, nil
	}
	return s.entClient.PaymentProviderInstance.Get(ctx, instID)
}

// getRefundOrderProviderInstance resolves the provider instance for refund paths.
// Refunds must be pinned to an explicit historical binding, so legacy
// "best-effort" provider guessing is intentionally not allowed here.
func (s *PaymentService) getRefundOrderProviderInstance(ctx context.Context, o *dbent.PaymentOrder) (*dbent.PaymentProviderInstance, error) {
	if s == nil || s.entClient == nil || o == nil {
		return nil, nil
	}

	if snapshot := psOrderProviderSnapshot(o); snapshot != nil {
		return s.resolveSnapshotOrderProviderInstance(ctx, o, snapshot)
	}

	instIDStr := strings.TrimSpace(psStringValue(o.ProviderInstanceID))
	if instIDStr == "" {
		return nil, nil
	}

	instID, err := strconv.ParseInt(instIDStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("order %d refund provider instance id is invalid: %s", o.ID, instIDStr)
	}
	inst, err := s.entClient.PaymentProviderInstance.Get(ctx, instID)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, fmt.Errorf("order %d refund provider instance %s is missing", o.ID, instIDStr)
		}
		return nil, err
	}
	return inst, nil
}

func (s *PaymentService) resolveUniqueLegacyOrderProviderInstance(ctx context.Context, o *dbent.PaymentOrder) (*dbent.PaymentProviderInstance, error) {
	paymentType := payment.GetBasePaymentType(strings.TrimSpace(o.PaymentType))
	providerKey := strings.TrimSpace(psStringValue(o.ProviderKey))
	if providerKey != "" {
		instances, err := s.entClient.PaymentProviderInstance.Query().
			Where(paymentproviderinstance.ProviderKeyEQ(providerKey)).
			All(ctx)
		if err != nil {
			return nil, err
		}
		matched := psFilterLegacyOrderProviderInstances(paymentType, instances)
		if len(matched) == 1 {
			return matched[0], nil
		}
		return nil, nil
	}

	if paymentType == "" {
		return nil, nil
	}

	instances, err := s.entClient.PaymentProviderInstance.Query().
		All(ctx)
	if err != nil {
		return nil, err
	}

	matched := psFilterLegacyOrderProviderInstances(paymentType, instances)
	if len(matched) == 1 {
		return matched[0], nil
	}
	return nil, nil
}

func psFilterLegacyOrderProviderInstances(orderPaymentType string, instances []*dbent.PaymentProviderInstance) []*dbent.PaymentProviderInstance {
	if len(instances) == 0 {
		return nil
	}
	if strings.TrimSpace(orderPaymentType) == "" {
		return instances
	}
	var matched []*dbent.PaymentProviderInstance
	for _, inst := range instances {
		if psLegacyOrderMatchesInstance(orderPaymentType, inst) {
			matched = append(matched, inst)
		}
	}
	return matched
}

func psLegacyOrderMatchesInstance(orderPaymentType string, inst *dbent.PaymentProviderInstance) bool {
	if inst == nil {
		return false
	}

	baseType := payment.GetBasePaymentType(strings.TrimSpace(orderPaymentType))
	instanceProviderKey := strings.TrimSpace(inst.ProviderKey)
	if baseType == "" {
		return false
	}

	if baseType == payment.TypeStripe {
		return instanceProviderKey == payment.TypeStripe
	}
	if instanceProviderKey == payment.TypeStripe {
		return false
	}
	if instanceProviderKey == baseType {
		return true
	}
	return payment.InstanceSupportsType(inst.SupportedTypes, baseType)
}

func (s *PaymentService) RequestRefund(ctx context.Context, oid, uid int64, reason string) error {
	o, err := s.validateRefundRequest(ctx, oid, uid)
	if err != nil {
		return err
	}
	u, err := s.userRepo.GetByID(ctx, o.UserID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if u.Balance < o.Amount {
		return infraerrors.BadRequest("BALANCE_NOT_ENOUGH", "refund amount exceeds balance")
	}
	nr := strings.TrimSpace(reason)
	now := time.Now()
	by := fmt.Sprintf("%d", uid)
	// refund_amount means "cumulative successfully refunded amount" everywhere
	// else, so a *request* must not write it: doing so made the pre-ledger fallback
	// (legacyRefundedTotal takes the max of this column and the audit total) treat
	// the order as already fully refunded, and the request could never be approved.
	c, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(oid), paymentorder.UserIDEQ(uid), paymentorder.StatusEQ(OrderStatusCompleted), paymentorder.OrderTypeEQ(payment.OrderTypeBalance)).SetStatus(OrderStatusRefundRequested).SetRefundRequestedAt(now).SetRefundRequestReason(nr).SetRefundRequestedBy(by).Save(ctx)
	if err != nil {
		return fmt.Errorf("update: %w", err)
	}
	if c == 0 {
		return infraerrors.Conflict("CONFLICT", "order status changed")
	}
	s.writeAuditLog(ctx, oid, "REFUND_REQUESTED", fmt.Sprintf("user:%d", uid), map[string]any{"amount": o.Amount, "reason": nr})
	return nil
}

func (s *PaymentService) validateRefundRequest(ctx context.Context, oid, uid int64) (*dbent.PaymentOrder, error) {
	o, err := s.entClient.PaymentOrder.Get(ctx, oid)
	if err != nil {
		return nil, infraerrors.NotFound("NOT_FOUND", "order not found")
	}
	if o.UserID != uid {
		return nil, infraerrors.Forbidden("FORBIDDEN", "no permission")
	}
	if o.OrderType != payment.OrderTypeBalance {
		return nil, infraerrors.BadRequest("INVALID_ORDER_TYPE", "only balance orders can request refund")
	}
	if o.Status != OrderStatusCompleted {
		return nil, infraerrors.BadRequest("INVALID_STATUS", "only completed orders can request refund")
	}
	// Check provider instance allows user refund
	inst, err := s.getRefundOrderProviderInstance(ctx, o)
	if err != nil || inst == nil {
		return nil, infraerrors.Forbidden("USER_REFUND_DISABLED", "refund is not available for this order")
	}
	if !inst.AllowUserRefund {
		return nil, infraerrors.Forbidden("USER_REFUND_DISABLED", "user refund is not enabled for this provider")
	}
	return o, nil
}

func (s *PaymentService) PrepareRefund(ctx context.Context, oid int64, amt float64, reason string, force, deduct bool) (*RefundPlan, *RefundResult, error) {
	o, err := s.entClient.PaymentOrder.Get(ctx, oid)
	if err != nil {
		return nil, nil, infraerrors.NotFound("NOT_FOUND", "order not found")
	}
	// REFUNDING is included so a refund that died mid-flight (process exit between
	// the status CAS and the gateway call) can be re-driven instead of becoming a
	// permanent dead state with the user's balance already deducted. The retry
	// reuses the same ledger installment, so it cannot double-count the refund.
	ok := []string{OrderStatusCompleted, OrderStatusRefundRequested, OrderStatusRefundPending, OrderStatusRefunding, OrderStatusRefundFailed, OrderStatusPartiallyRefunded}
	if !psSliceContains(ok, o.Status) {
		return nil, nil, infraerrors.BadRequest("INVALID_STATUS", "order status does not allow refund")
	}
	// Check provider instance allows admin refund
	inst, instErr := s.getRefundOrderProviderInstance(ctx, o)
	if instErr != nil {
		slog.Warn("refund: provider instance lookup failed", "orderID", oid, "error", instErr)
		return nil, nil, infraerrors.InternalServer("PROVIDER_LOOKUP_FAILED", "failed to look up payment provider for this order")
	}
	if inst == nil {
		// Legacy order without provider_instance_id — block refund
		return nil, nil, infraerrors.Forbidden("REFUND_DISABLED", "refund is not available for this order")
	}
	if !inst.RefundEnabled {
		return nil, nil, infraerrors.Forbidden("REFUND_DISABLED", "refund is not enabled for this provider")
	}
	if math.IsNaN(amt) || math.IsInf(amt, 0) {
		return nil, nil, infraerrors.BadRequest("INVALID_AMOUNT", "invalid refund amount")
	}
	orderCurrency := PaymentOrderCurrency(o)
	tolerance := paymentAmountToleranceForCurrency(orderCurrency)
	// Partially refunded orders may be refunded again, but only up to the
	// remaining amount: the budget is the order amount minus everything already
	// refunded successfully. The refunded total comes from the payment_refunds
	// ledger, not from audit entries: payment_audit_logs is capped at one row per
	// (order_id, action) by a unique index, so audit rows cannot represent more
	// than one refund installment and would inflate the remaining budget.
	refundedBefore, err := s.refundSucceededTotal(ctx, o)
	if err != nil {
		return nil, nil, infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to compute already refunded amount")
	}
	remaining := o.Amount - refundedBefore
	if remaining <= tolerance {
		return nil, nil, infraerrors.BadRequest("REFUND_ALREADY_COMPLETED", "order is already fully refunded")
	}
	if amt <= 0 {
		amt = remaining
	}
	if amt-remaining > tolerance {
		return nil, nil, infraerrors.BadRequest("REFUND_AMOUNT_EXCEEDED", "refund amount exceeds the refundable remainder").
			WithMetadata(map[string]string{"remaining": fmt.Sprintf("%.2f", remaining)})
	}
	ga := calculateGatewayRefundAmount(o.Amount, o.PayAmount, amt, orderCurrency)
	rr := strings.TrimSpace(reason)
	if rr == "" && o.RefundRequestReason != nil {
		rr = *o.RefundRequestReason
	}
	if rr == "" {
		rr = fmt.Sprintf("refund order:%d", o.ID)
	}
	p := &RefundPlan{OrderID: oid, Order: o, RefundAmount: amt, GatewayAmount: ga, Reason: rr, Force: force, DeductBalance: deduct, DeductionType: payment.DeductionTypeNone, RefundedBefore: refundedBefore}
	if deduct {
		if er := s.prepDeduct(ctx, o, p, force); er != nil {
			return nil, er, nil
		}
	}
	return p, nil, nil
}

// auditRefundedTotal sums REFUND_SUCCESS audit rows for an order.
//
// Deprecated as a source of truth: payment_audit_logs carries a unique index on
// (order_id, action) (migration 131), so at most one REFUND_SUCCESS row can exist
// per order and this value under-reports any order refunded more than once. It is
// kept only as a conservative legacy fallback for orders that predate the
// payment_refunds ledger. See refundSucceededTotal.
func (s *PaymentService) auditRefundedTotal(ctx context.Context, oid int64) (float64, error) {
	entries, err := s.entClient.PaymentAuditLog.Query().
		Where(
			paymentauditlog.OrderIDEQ(strconv.FormatInt(oid, 10)),
			paymentauditlog.ActionEQ("REFUND_SUCCESS"),
		).All(ctx)
	if err != nil {
		return 0, fmt.Errorf("query refund audit entries: %w", err)
	}
	var total float64
	for _, entry := range entries {
		var detail struct {
			RefundAmount float64 `json:"refundAmount"`
		}
		if err := json.Unmarshal([]byte(entry.Detail), &detail); err != nil {
			return 0, fmt.Errorf("parse refund audit entry %d: %w", entry.ID, err)
		}
		total += detail.RefundAmount
	}
	return total, nil
}

func (s *PaymentService) prepDeduct(ctx context.Context, o *dbent.PaymentOrder, p *RefundPlan, force bool) *RefundResult {
	if o.OrderType == payment.OrderTypeSubscription {
		p.DeductionType = payment.DeductionTypeSubscription
		if o.SubscriptionGroupID != nil && o.SubscriptionDays != nil {
			p.SubDaysToDeduct = *o.SubscriptionDays
			sub, err := s.subscriptionSvc.GetActiveSubscription(ctx, o.UserID, *o.SubscriptionGroupID)
			if err == nil && sub != nil {
				p.SubscriptionID = sub.ID
			} else if !force {
				return &RefundResult{Success: false, Warning: "cannot find active subscription for deduction, use force", RequireForce: true}
			}
		}
		return nil
	}
	u, err := s.userRepo.GetByID(ctx, o.UserID)
	if err != nil {
		if !force {
			return &RefundResult{Success: false, Warning: "cannot fetch user balance, use force", RequireForce: true}
		}
		return nil
	}
	p.DeductionType = payment.DeductionTypeBalance
	if u.Balance < p.RefundAmount && !force {
		return &RefundResult{Success: false, Warning: "user balance is insufficient for deduction, use force", RequireForce: true}
	}
	p.BalanceToDeduct = math.Max(0, math.Min(p.RefundAmount, u.Balance))
	return nil
}

type availableBalanceDeductor interface {
	DeductAvailableBalance(ctx context.Context, id int64, amount float64) (float64, error)
}

func (s *PaymentService) deductAvailableBalance(ctx context.Context, userID int64, amount float64) (float64, error) {
	repo, ok := s.userRepo.(availableBalanceDeductor)
	if !ok {
		return 0, errors.New("user repository does not support available balance deduction")
	}
	return repo.DeductAvailableBalance(ctx, userID, amount)
}

func (s *PaymentService) ExecuteRefund(ctx context.Context, p *RefundPlan) (*RefundResult, error) {
	c, err := s.entClient.PaymentOrder.Update().Where(paymentorder.IDEQ(p.OrderID), paymentorder.StatusIn(OrderStatusCompleted, OrderStatusRefundRequested, OrderStatusRefundPending, OrderStatusRefunding, OrderStatusRefundFailed, OrderStatusPartiallyRefunded)).SetStatus(OrderStatusRefunding).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if c == 0 {
		return nil, infraerrors.Conflict("CONFLICT", "order status changed")
	}

	// Re-validate the refundable remainder *after* winning the status CAS.
	//
	// PrepareRefund computed the remainder from a snapshot, and the admin handler
	// calls PrepareRefund and ExecuteRefund as two separate steps. Two concurrent
	// refunds can therefore both prepare against "nothing refunded yet" and both
	// carry the full order amount; the status CAS only lets one of them proceed at
	// a time, but the second one would then execute with a stale refundedBefore and
	// over-refund. Re-reading the ledger here — while the order is pinned to
	// REFUNDING, so no other refund can be mid-flight — closes that window.
	//
	// A re-drive of the current installment is unaffected: that row is still
	// REFUNDING and so is not counted as already refunded.
	if err := s.validateRefundRemainder(ctx, p); err != nil {
		// Rejecting the plan must not damage state: the order is pinned to
		// REFUNDING right now, and a successful refund may have landed since the
		// plan was prepared. Releasing it back to the status recorded in the stale
		// snapshot would erase that PARTIALLY_REFUNDED/REFUNDED outcome.
		s.restoreStatusFromLedger(ctx, p)
		return nil, err
	}

	// Claim this refund installment in the ledger before touching the gateway. The
	// ledger is what makes the refunded total correct, so a gateway call must never
	// happen without a corresponding ledger row. A retry of the same installment
	// reuses p.RefundNo and updates that row in place.
	refundNo, err := s.ensureRefundLedgerClaim(ctx, p)
	if err != nil {
		s.restoreStatus(ctx, p)
		return nil, err
	}
	p.RefundNo = refundNo

	if p.DeductionType == payment.DeductionTypeBalance && p.BalanceToDeduct > 0 {
		// Skip balance deduction on retry if a previous attempt already deducted
		// but failed to roll back; the ledger records that outcome explicitly
		// instead of relying on an audit-log existence check.
		if gErr := s.refundLedgerAllowsDeduction(ctx, p.OrderID, refundNo); gErr == nil {
			// Persist "a deduction may be in flight" BEFORE touching the user's
			// balance: there is no transaction spanning the deduction and the
			// gateway call, so a crash in that window must leave a durable record
			// that the money may already be gone.
			if err := s.markRefundLedgerDeductionInFlight(ctx, p.OrderID, refundNo); err != nil {
				s.restoreStatus(ctx, p)
				return nil, err
			}
			deducted, err := s.deductAvailableBalance(ctx, p.Order.UserID, p.BalanceToDeduct)
			if err != nil {
				// The deduction call failed, so nothing was taken: clear the
				// in-flight marker rather than under-deducting the user forever.
				s.markRefundLedgerDeductionNotApplied(ctx, p.OrderID, refundNo)
				s.restoreStatus(ctx, p)
				return nil, fmt.Errorf("deduction: %w", err)
			}
			p.BalanceToDeduct = deducted
		} else {
			slog.Warn("skipping balance deduction on retry (previous rollback failed)", "orderID", p.OrderID)
			p.BalanceToDeduct = 0
		}
	}
	if p.DeductionType == payment.DeductionTypeSubscription && p.SubDaysToDeduct > 0 && p.SubscriptionID > 0 {
		if s.refundLedgerAllowsDeduction(ctx, p.OrderID, refundNo) == nil {
			if err := s.markRefundLedgerDeductionInFlight(ctx, p.OrderID, refundNo); err != nil {
				s.restoreStatus(ctx, p)
				return nil, err
			}
			_, err := s.subscriptionSvc.ExtendSubscription(ctx, p.SubscriptionID, -p.SubDaysToDeduct)
			if err != nil {
				// Only ErrAdjustWouldExpire and hard errors reach here; both leave
				// the subscription unchanged, so the marker can be cleared safely.
				s.markRefundLedgerDeductionNotApplied(ctx, p.OrderID, refundNo)
				if errors.Is(err, ErrAdjustWouldExpire) {
					// Deduction would expire the subscription — revoke it entirely
					slog.Info("subscription deduction would expire, revoking", "orderID", p.OrderID, "subID", p.SubscriptionID, "days", p.SubDaysToDeduct)
					if revokeErr := s.subscriptionSvc.RevokeSubscription(ctx, p.SubscriptionID); revokeErr != nil {
						s.restoreStatus(ctx, p)
						return nil, fmt.Errorf("revoke subscription: %w", revokeErr)
					}
				} else {
					// Other errors (DB failure, not found) — abort refund
					s.restoreStatus(ctx, p)
					return nil, fmt.Errorf("deduct subscription days: %w", err)
				}
			}
		} else {
			slog.Warn("skipping subscription deduction on retry (previous rollback failed)", "orderID", p.OrderID)
			p.SubDaysToDeduct = 0
		}
	}
	resp, err := s.gwRefund(ctx, p)
	if err != nil {
		return s.handleGwFail(ctx, p, err)
	}
	return s.finishRefund(ctx, p, resp)
}

// ensureRefundLedgerClaim returns the ledger installment number for this attempt.
//
// A REFUNDING order is one whose previous attempt died before reaching a terminal
// state. In that case we reuse the existing in-flight installment (same RefundNo)
// so the retry refines that row instead of adding a second one. Otherwise a new
// installment number is derived for a fresh refund.
func (s *PaymentService) ensureRefundLedgerClaim(ctx context.Context, p *RefundPlan) (string, error) {
	if strings.TrimSpace(p.RefundNo) != "" {
		return p.RefundNo, nil
	}
	client := paymentServiceRefundClient{s.entClient}
	if row, err := s.activeRefundLedgerRow(ctx, p.OrderID); err != nil {
		return "", infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to read refund ledger")
	} else if row != nil {
		// Re-drive the same installment; keep the original amount so the ledger
		// stays consistent with what was sent to the gateway.
		p.RefundNo = row.RefundNo
		p.RefundAmount = row.Amount
		p.GatewayAmount = row.GatewayAmount
		return row.RefundNo, nil
	}
	refundNo, err := s.nextRefundNo(ctx, client, p.OrderID)
	if err != nil {
		return "", infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to allocate refund installment number")
	}
	if err := s.upsertRefundLedgerClaim(ctx, client, p.OrderID, refundNo, p.RefundAmount, p.GatewayAmount, PaymentOrderCurrency(p.Order), p.Reason, "admin", p.Force); err != nil {
		if infraerrors.Reason(err) == "REFUND_IN_PROGRESS" {
			return "", err
		}
		return "", infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to record refund installment")
	}
	return refundNo, nil
}

// refundLedgerAllowsDeduction reports (nil error) whether a retry of this
// installment should still perform the balance/subscription deduction. If a
// previous attempt already deducted but could not roll the deduction back, the
// ledger row records deductionRollbackOK=false and the retry must not deduct again.
func (s *PaymentService) refundLedgerAllowsDeduction(ctx context.Context, orderID int64, refundNo string) error {
	row, err := s.entClient.PaymentRefund.Query().
		Where(paymentrefund.OrderIDEQ(orderID), paymentrefund.RefundNoEQ(refundNo)).
		Only(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil
		}
		return err
	}
	if row.DeductionRollbackOk {
		return nil
	}
	return fmt.Errorf("previous refund attempt left a failed deduction rollback")
}

func (s *PaymentService) gwRefund(ctx context.Context, p *RefundPlan) (*payment.RefundResponse, error) {
	// Use the exact provider instance that created this order, not a random one
	// from the registry. Each instance has its own merchant credentials.
	prov, err := s.getRefundProvider(ctx, p.Order)
	if err != nil {
		return nil, fmt.Errorf("get refund provider: %w", err)
	}
	if requiresTradeNo, ok := prov.(payment.TradeNoRefundProvider); ok && requiresTradeNo.RefundRequiresTradeNo() {
		// Providers that refund against the upstream trade number (payment intent
		// id) cannot be refunded without it. Fail loudly instead of recording a
		// local-only "successful" refund that never reached the gateway.
		if strings.TrimSpace(p.Order.PaymentTradeNo) == "" {
			s.writeAuditLog(ctx, p.Order.ID, "REFUND_NO_TRADE_NO", "admin", map[string]any{
				"detail":   "order has no upstream trade number, refund was not sent to the gateway",
				"provider": prov.ProviderKey(),
			})
			return nil, infraerrors.BadRequest("REFUND_NO_TRADE_NO", "order has no upstream trade number, refund was not sent to the gateway")
		}
	}
	if err := validateProviderSnapshotMetadata(p.Order, prov.ProviderKey(), providerMerchantIdentityMetadata(prov)); err != nil {
		s.writeAuditLog(ctx, p.Order.ID, "REFUND_PROVIDER_METADATA_MISMATCH", "admin", map[string]any{
			"detail": err.Error(),
		})
		return nil, err
	}
	finishProviderCall := servertiming.ObserveDependency(ctx, "payment")
	resp, err := prov.Refund(ctx, payment.RefundRequest{
		TradeNo: p.Order.PaymentTradeNo,
		OrderID: p.Order.OutTradeNo,
		Amount:  formatGatewayRefundAmount(p.GatewayAmount, p.Order),
		Reason:  p.Reason,
		// Passing the persisted installment number gives providers that support an
		// idempotency reference (Alipay out_request_no) the same value on a retry,
		// so a re-driven refund is recognised as the same request instead of being
		// sent to the gateway a second time.
		RefundNo: p.RefundNo,
	})
	finishProviderCall()
	if err != nil {
		if resp != nil && strings.TrimSpace(resp.Status) == payment.ProviderStatusPending {
			return resp, nil
		}
		return nil, err
	}
	if err := validateRefundProviderResponse(resp); err != nil {
		return nil, err
	}
	return resp, nil
}

func formatGatewayRefundAmount(amount float64, order *dbent.PaymentOrder) string {
	return payment.FormatAmountForCurrency(amount, PaymentOrderCurrency(order))
}

func validateRefundProviderResponse(resp *payment.RefundResponse) error {
	if resp == nil {
		return fmt.Errorf("payment refund response missing")
	}
	status := strings.TrimSpace(resp.Status)
	switch status {
	case payment.ProviderStatusSuccess, payment.ProviderStatusRefunded, payment.ProviderStatusPending:
		return nil
	case payment.ProviderStatusFailed:
		return fmt.Errorf("payment refund failed: status %s", status)
	default:
		return fmt.Errorf("payment refund returned unknown status: %s", status)
	}
}

func (s *PaymentService) finishRefund(ctx context.Context, p *RefundPlan, resp *payment.RefundResponse) (*RefundResult, error) {
	if err := validateRefundProviderResponse(resp); err != nil {
		return s.handleGwFail(ctx, p, err)
	}
	switch strings.TrimSpace(resp.Status) {
	case payment.ProviderStatusSuccess, payment.ProviderStatusRefunded:
		return s.markRefundOk(ctx, p)
	case payment.ProviderStatusPending:
		return s.markRefundPending(ctx, p, resp)
	default:
		return s.handleGwFail(ctx, p, fmt.Errorf("payment refund returned unknown status: %s", strings.TrimSpace(resp.Status)))
	}
}

func (s *PaymentService) QueryAndFinalizeRefund(ctx context.Context, oid int64) (*RefundResult, error) {
	o, err := s.entClient.PaymentOrder.Get(ctx, oid)
	if err != nil {
		return nil, infraerrors.NotFound("NOT_FOUND", "order not found")
	}
	if o.Status != OrderStatusRefundPending {
		return nil, infraerrors.BadRequest("INVALID_STATUS", "only refund pending orders can be finalized")
	}

	prov, err := s.getRefundProvider(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("get refund provider: %w", err)
	}
	queryProvider, ok := prov.(payment.RefundQueryProvider)
	if !ok {
		return nil, infraerrors.BadRequest("REFUND_QUERY_UNSUPPORTED", "this payment provider does not support refund status query; please verify manually")
	}

	// The in-flight ledger installment is the source of truth for the pending
	// refund's amount, provider refund id and deduction rollback outcome.
	// legacyPendingDetail covers orders whose refund predates the ledger.
	ledgerRow, err := s.activeRefundLedgerRow(ctx, oid)
	if err != nil {
		return nil, infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to read refund ledger")
	}
	pendingAmount := 0.0
	refundNo := ""
	refundID := ""
	// Assigned by both branches below; no default is needed.
	var deductionRollbackOK bool
	if ledgerRow != nil {
		pendingAmount = ledgerRow.Amount
		refundNo = ledgerRow.RefundNo
		refundID = ledgerRow.ProviderRefundID
		deductionRollbackOK = ledgerRow.DeductionRollbackOk
	} else {
		pendingDetail := s.latestRefundPendingDetail(ctx, oid)
		pendingAmount = pendingDetail.RefundAmount
		refundID = pendingDetail.RefundID
		deductionRollbackOK = pendingDetail.DeductionRollbackOk
	}
	// Legacy REFUND_PENDING audit entries (and pre-ledger orders) predate the
	// recorded installment amount; fall back to the order's refund_amount.
	if pendingAmount <= 0 {
		pendingAmount = o.RefundAmount
	}
	finishProviderCall := servertiming.ObserveDependency(ctx, "payment")
	resp, err := queryProvider.QueryRefund(ctx, payment.RefundQueryRequest{
		TradeNo:  o.PaymentTradeNo,
		OrderID:  o.OutTradeNo,
		RefundID: refundID,
		Amount:   formatGatewayRefundAmount(pendingAmount, o),
	})
	finishProviderCall()
	if err != nil {
		return nil, fmt.Errorf("query refund: %w", err)
	}
	if err := validateRefundProviderResponse(resp); err != nil {
		return s.finalizeRefundFailed(ctx, o, refundNo, err)
	}

	refundedBefore, err := s.refundSucceededTotal(ctx, o)
	if err != nil {
		return nil, infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to compute already refunded amount")
	}
	plan := s.refundFinalizePlan(o, pendingAmount, refundedBefore)
	plan.RefundNo = refundNo
	if !deductionRollbackOK {
		plan.BalanceToDeduct = 0
		plan.SubDaysToDeduct = 0
	} else if o.OrderType == payment.OrderTypeSubscription {
		if early := s.prepDeduct(ctx, o, plan, true); early != nil {
			return early, nil
		}
	}
	switch strings.TrimSpace(resp.Status) {
	case payment.ProviderStatusSuccess, payment.ProviderStatusRefunded:
		return s.finalizePendingRefundSuccess(ctx, plan)
	case payment.ProviderStatusPending:
		s.writeAuditLog(ctx, oid, "REFUND_QUERY_PENDING", "admin", map[string]any{"refundID": resp.RefundID})
		return &RefundResult{Success: false, Warning: "gateway refund is still pending confirmation"}, nil
	default:
		return s.finalizeRefundFailed(ctx, o, refundNo, fmt.Errorf("payment refund returned unknown status: %s", strings.TrimSpace(resp.Status)))
	}
}

func (s *PaymentService) finalizePendingRefundSuccess(ctx context.Context, p *RefundPlan) (_ *RefundResult, err error) {
	tx, err := s.entClient.Tx(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin refund finalization: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	txCtx := dbent.NewTxContext(ctx, tx)

	claimed, err := tx.PaymentOrder.Update().
		Where(paymentorder.IDEQ(p.OrderID), paymentorder.StatusEQ(OrderStatusRefundPending)).
		SetStatus(OrderStatusRefunding).
		Save(txCtx)
	if err != nil {
		return nil, fmt.Errorf("claim pending refund: %w", err)
	}
	if claimed == 0 {
		return nil, infraerrors.Conflict("CONFLICT", "order status changed")
	}

	if err := s.applyRefundFinalDeduction(txCtx, p); err != nil {
		return nil, err
	}
	result, err := s.markRefundOkTx(txCtx, tx.Client(), p)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit refund finalization: %w", err)
	}
	return result, nil
}

func (s *PaymentService) refundFinalizePlan(o *dbent.PaymentOrder, pendingAmount, refundedBefore float64) *RefundPlan {
	refundAmount := pendingAmount
	if refundAmount <= 0 {
		refundAmount = o.RefundAmount
	}
	reason := strings.TrimSpace(psStringValue(o.RefundReason))
	if reason == "" {
		reason = fmt.Sprintf("refund order:%d", o.ID)
	}
	return &RefundPlan{
		OrderID:        o.ID,
		Order:          o,
		RefundAmount:   refundAmount,
		GatewayAmount:  calculateGatewayRefundAmount(o.Amount, o.PayAmount, refundAmount, PaymentOrderCurrency(o)),
		Reason:         reason,
		Force:          o.ForceRefund,
		DeductBalance:  true,
		DeductionType:  payment.DeductionTypeBalance,
		RefundedBefore: refundedBefore,
		BalanceToDeduct: func() float64 {
			if o.OrderType == payment.OrderTypeBalance {
				return refundAmount
			}
			return 0
		}(),
	}
}

func (s *PaymentService) applyRefundFinalDeduction(ctx context.Context, p *RefundPlan) error {
	if p.DeductionType == payment.DeductionTypeBalance && p.BalanceToDeduct > 0 {
		deducted, err := s.deductAvailableBalance(ctx, p.Order.UserID, p.BalanceToDeduct)
		if err != nil {
			return fmt.Errorf("deduction: %w", err)
		}
		p.BalanceToDeduct = deducted
	}
	if p.DeductionType == payment.DeductionTypeSubscription && p.SubDaysToDeduct > 0 && p.SubscriptionID > 0 {
		if _, err := s.subscriptionSvc.ExtendSubscription(ctx, p.SubscriptionID, -p.SubDaysToDeduct); err != nil {
			if errors.Is(err, ErrAdjustWouldExpire) {
				if revokeErr := s.subscriptionSvc.RevokeSubscription(ctx, p.SubscriptionID); revokeErr != nil {
					return fmt.Errorf("revoke subscription: %w", revokeErr)
				}
			} else {
				return fmt.Errorf("deduct subscription days: %w", err)
			}
		}
	}
	return nil
}

func (s *PaymentService) finalizeRefundFailed(ctx context.Context, o *dbent.PaymentOrder, refundNo string, gErr error) (*RefundResult, error) {
	now := time.Now()
	_, _ = s.entClient.PaymentOrder.UpdateOneID(o.ID).SetStatus(OrderStatusRefundFailed).SetFailedAt(now).SetFailedReason(psErrMsg(gErr)).Save(ctx)
	if refundNo != "" {
		// Mark the installment FAILED so it does not stay in flight forever and
		// block (via the partial unique index) any later refund attempt.
		if err := s.markRefundLedgerFailed(ctx, paymentServiceRefundClient{s.entClient}, o.ID, refundNo, psErrMsg(gErr), 0, 0, true); err != nil {
			slog.Error("failed to mark refund ledger failed", "orderID", o.ID, "refundNo", refundNo, "error", err)
		}
	}
	s.writeAuditLog(ctx, o.ID, "REFUND_FAILED", "admin", map[string]any{"detail": psErrMsg(gErr)})
	return &RefundResult{Success: false, Warning: "gateway refund failed: " + psErrMsg(gErr)}, nil
}

type refundPendingAuditDetail struct {
	RefundID            string  `json:"refundID"`
	RefundAmount        float64 `json:"refundAmount"`
	DeductionRollbackOk bool    `json:"deductionRollbackOK"`
}

func (s *PaymentService) latestRefundPendingDetail(ctx context.Context, oid int64) refundPendingAuditDetail {
	logEntry, err := s.entClient.PaymentAuditLog.Query().
		Where(paymentauditlog.OrderIDEQ(strconv.FormatInt(oid, 10)), paymentauditlog.ActionEQ("REFUND_PENDING")).
		Order(paymentauditlog.ByCreatedAt(sql.OrderDesc())).
		First(ctx)
	if err != nil || logEntry == nil {
		return refundPendingAuditDetail{DeductionRollbackOk: true}
	}
	detail := refundPendingAuditDetail{DeductionRollbackOk: true}
	_ = json.Unmarshal([]byte(logEntry.Detail), &detail)
	detail.RefundID = strings.TrimSpace(detail.RefundID)
	return detail
}

// getRefundProvider creates a provider using the order's original instance config.
// Delegates to getOrderProvider which handles instance lookup and fallback.
func (s *PaymentService) getRefundProvider(ctx context.Context, o *dbent.PaymentOrder) (payment.Provider, error) {
	inst, err := s.getRefundOrderProviderInstance(ctx, o)
	if err != nil {
		return nil, err
	}
	if inst == nil {
		return nil, fmt.Errorf("refund provider instance is unavailable for order %d", o.ID)
	}
	return s.createProviderFromInstance(ctx, inst)
}

func (s *PaymentService) handleGwFail(ctx context.Context, p *RefundPlan, gErr error) (*RefundResult, error) {
	rollbackOK := s.RollbackRefund(ctx, p, gErr)
	if rollbackOK {
		s.restoreStatus(ctx, p)
		s.writeAuditLog(ctx, p.OrderID, "REFUND_GATEWAY_FAILED", "admin", map[string]any{"detail": psErrMsg(gErr)})
		if p.RefundNo != "" {
			// The gateway never accepted this installment and every side effect was
			// rolled back, so the installment never happened: remove it rather than
			// leaving a row in flight that would block (via the partial unique index
			// on active installments) any later refund attempt.
			if err := s.deleteRefundLedgerRow(ctx, p.OrderID, p.RefundNo); err != nil {
				slog.Error("failed to delete rolled-back refund ledger row", "orderID", p.OrderID, "refundNo", p.RefundNo, "error", err)
			}
		}
		return &RefundResult{Success: false, Warning: "gateway failed: " + psErrMsg(gErr) + ", rolled back"}, nil
	}
	now := time.Now()
	_, _ = s.entClient.PaymentOrder.UpdateOneID(p.OrderID).SetStatus(OrderStatusRefundFailed).SetFailedAt(now).SetFailedReason(psErrMsg(gErr)).Save(ctx)
	if p.RefundNo != "" {
		// Record that the deduction could not be rolled back so a retry knows not
		// to deduct again (replaces the REFUND_ROLLBACK_FAILED audit existence check).
		if err := s.markRefundLedgerFailed(ctx, paymentServiceRefundClient{s.entClient}, p.OrderID, p.RefundNo, psErrMsg(gErr), p.BalanceToDeduct, p.SubDaysToDeduct, false); err != nil {
			slog.Error("failed to mark refund ledger failed", "orderID", p.OrderID, "refundNo", p.RefundNo, "error", err)
		}
	}
	s.writeAuditLog(ctx, p.OrderID, "REFUND_FAILED", "admin", map[string]any{"detail": psErrMsg(gErr)})
	return nil, infraerrors.InternalServer("REFUND_FAILED", psErrMsg(gErr))
}

func (s *PaymentService) markRefundOk(ctx context.Context, p *RefundPlan) (*RefundResult, error) {
	fs := refundResultStatus(p)
	now := time.Now()
	// Mark the ledger installment SUCCEEDED first: it is what makes the refunded
	// total correct, so it must never lag behind the order status.
	if err := s.markRefundLedgerSucceeded(ctx, paymentServiceRefundClient{s.entClient}, p); err != nil {
		return nil, err
	}
	// refund_amount records the cumulative refunded total, which is how the admin
	// UI reads it (order amount minus refund_amount = refundable remainder).
	_, err := s.entClient.PaymentOrder.UpdateOneID(p.OrderID).SetStatus(fs).SetRefundAmount(p.RefundedBefore + p.RefundAmount).SetRefundReason(p.Reason).SetRefundAt(now).SetForceRefund(p.Force).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("mark refund: %w", err)
	}
	s.writeAuditLog(ctx, p.OrderID, "REFUND_SUCCESS", "admin", map[string]any{"refundAmount": p.RefundAmount, "reason": p.Reason, "balanceDeducted": p.BalanceToDeduct, "force": p.Force})
	return &RefundResult{Success: true, BalanceDeducted: p.BalanceToDeduct, SubDaysDeducted: p.SubDaysToDeduct}, nil
}

// refundResultStatus decides the terminal refund status for this attempt:
// REFUNDED once the cumulative refunded amount covers the order, otherwise
// PARTIALLY_REFUNDED (which stays eligible for another refund).
func refundResultStatus(p *RefundPlan) string {
	if p == nil {
		return OrderStatusPartiallyRefunded
	}
	return refundedStatusForTotal(p.Order, p.RefundedBefore+p.RefundAmount)
}

func (s *PaymentService) markRefundOkTx(ctx context.Context, client *dbent.Client, p *RefundPlan) (*RefundResult, error) {
	fs := refundResultStatus(p)
	now := time.Now()
	// The ledger write happens inside the same transaction as the order update, so
	// a failure here rolls the whole finalization back and can be retried. The
	// previous implementation wrote an audit row here, which both capped at one
	// row per (order_id, action) and made a REFUND_PENDING order unfinalizable once
	// any REFUND_SUCCESS row already existed.
	if err := s.markRefundLedgerSucceeded(ctx, paymentServiceRefundClient{client}, p); err != nil {
		return nil, err
	}
	// refund_amount records the cumulative refunded total (see markRefundOk).
	_, err := client.PaymentOrder.UpdateOneID(p.OrderID).SetStatus(fs).SetRefundAmount(p.RefundedBefore + p.RefundAmount).SetRefundReason(p.Reason).SetRefundAt(now).SetForceRefund(p.Force).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("mark refund: %w", err)
	}
	// 审计行只作为人类可读的观测记录；重复写入会因 (order_id, action) 唯一索引
	// 失败，但那只影响审计展示，不影响台账正确性，因此这里不再让它回滚事务
	// （旧实现在这里返回错误，导致已有 REFUND_SUCCESS 行的订单永远无法完成退款确认）。
	if err := client.PaymentAuditLog.Create().
		SetOrderID(orderIDString(p.OrderID)).
		SetAction("REFUND_SUCCESS").
		SetDetail(formatRefundSuccessAuditDetail(p)).
		SetOperator("admin").
		OnConflictColumns(paymentauditlog.FieldOrderID, paymentauditlog.FieldAction).
		DoNothing().
		Exec(ctx); err != nil {
		slog.Warn("refund success audit write failed (ledger is authoritative)", "orderID", p.OrderID, "error", err)
	}
	return &RefundResult{Success: true, BalanceDeducted: p.BalanceToDeduct, SubDaysDeducted: p.SubDaysToDeduct}, nil
}

// formatRefundSuccessAuditDetail 渲染 REFUND_SUCCESS 审计行的 detail。
func formatRefundSuccessAuditDetail(p *RefundPlan) string {
	detail, err := json.Marshal(map[string]any{"refundAmount": p.RefundAmount, "reason": p.Reason, "balanceDeducted": p.BalanceToDeduct, "force": p.Force})
	if err != nil {
		return "{}"
	}
	return string(detail)
}

func (s *PaymentService) markRefundPending(ctx context.Context, p *RefundPlan, resp *payment.RefundResponse) (*RefundResult, error) {
	balanceDeducted := p.BalanceToDeduct
	subDaysDeducted := p.SubDaysToDeduct
	rollbackOK := s.RollbackRefund(ctx, p, nil)
	if rollbackOK {
		p.BalanceToDeduct = 0
		p.SubDaysToDeduct = 0
	}

	// The ledger is the source of truth for this in-flight installment; make sure a
	// row exists even for legacy orders whose refund predates the ledger, so the
	// pending refund can still be queried and finalized later.
	if err := s.upsertRefundLedgerPending(ctx, p, refundResponseID(resp), rollbackOK); err != nil {
		return nil, err
	}

	_, err := s.entClient.PaymentOrder.UpdateOneID(p.OrderID).
		SetStatus(OrderStatusRefundPending).
		SetRefundAmount(p.RefundedBefore + p.RefundAmount).
		SetRefundReason(p.Reason).
		ClearRefundAt().
		SetForceRefund(p.Force).
		ClearFailedAt().
		ClearFailedReason().
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("mark refund pending: %w", err)
	}

	detail := map[string]any{
		"refundID":            refundResponseID(resp),
		"refundAmount":        p.RefundAmount,
		"reason":              p.Reason,
		"force":               p.Force,
		"balanceDeducted":     p.BalanceToDeduct,
		"subDaysDeducted":     p.SubDaysToDeduct,
		"balanceRolledBack":   balanceDeducted,
		"subDaysRolledBack":   subDaysDeducted,
		"deductionRollbackOK": rollbackOK,
	}
	s.writeAuditLog(ctx, p.OrderID, "REFUND_PENDING", "admin", detail)

	warning := "gateway refund is pending confirmation"
	if !rollbackOK {
		warning += "; refund deduction rollback failed"
	}
	return &RefundResult{Success: false, Warning: warning}, nil
}

func refundResponseID(resp *payment.RefundResponse) string {
	if resp == nil {
		return ""
	}
	return strings.TrimSpace(resp.RefundID)
}

func (s *PaymentService) RollbackRefund(ctx context.Context, p *RefundPlan, gErr error) bool {
	if p.DeductionType == payment.DeductionTypeBalance && p.BalanceToDeduct > 0 {
		if err := s.userRepo.UpdateBalance(ctx, p.Order.UserID, p.BalanceToDeduct); err != nil {
			slog.Error("[CRITICAL] rollback failed", "orderID", p.OrderID, "amount", p.BalanceToDeduct, "error", err)
			s.writeAuditLog(ctx, p.OrderID, "REFUND_ROLLBACK_FAILED", "admin", map[string]any{"gatewayError": psErrMsg(gErr), "rollbackError": psErrMsg(err), "balanceDeducted": p.BalanceToDeduct})
			return false
		}
	}
	if p.DeductionType == payment.DeductionTypeSubscription && p.SubDaysToDeduct > 0 && p.SubscriptionID > 0 {
		if _, err := s.subscriptionSvc.ExtendSubscription(ctx, p.SubscriptionID, p.SubDaysToDeduct); err != nil {
			slog.Error("[CRITICAL] subscription rollback failed", "orderID", p.OrderID, "subID", p.SubscriptionID, "days", p.SubDaysToDeduct, "error", err)
			s.writeAuditLog(ctx, p.OrderID, "REFUND_ROLLBACK_FAILED", "admin", map[string]any{"gatewayError": psErrMsg(gErr), "rollbackError": psErrMsg(err), "subDaysDeducted": p.SubDaysToDeduct})
			return false
		}
	}
	return true
}

// restoreStatus returns the order to the status it had when the refund attempt
// started, so a failed gateway call does not lose refund bookkeeping (e.g. a
// previous PARTIALLY_REFUNDED or a REFUND_PENDING awaiting confirmation).
func (s *PaymentService) restoreStatus(ctx context.Context, p *RefundPlan) {
	rs := p.Order.Status
	switch rs {
	case OrderStatusCompleted, OrderStatusRefundRequested, OrderStatusRefundPending, OrderStatusRefundFailed, OrderStatusPartiallyRefunded:
	default:
		rs = OrderStatusCompleted
	}
	_, _ = s.entClient.PaymentOrder.UpdateOneID(p.OrderID).SetStatus(rs).Save(ctx)
}

// validateRefundRemainder re-reads the already-refunded total from the ledger and
// rejects a plan whose amount no longer fits the remaining budget.
//
// It exists because PrepareRefund and ExecuteRefund are separate calls (the admin
// HTTP handler does prepare-then-execute), so a plan can go stale between them:
// another refund may have succeeded in the meantime. The order status CAS in
// ExecuteRefund serialises the two calls, but it cannot detect that the *amount*
// was computed before the earlier refund landed — only a fresh read can.
//
// Direction is conservative: this can only reject a refund, never enlarge one.
func (s *PaymentService) validateRefundRemainder(ctx context.Context, p *RefundPlan) error {
	if p == nil || p.Order == nil {
		// Nothing to validate against; skip rather than invent a failure.
		return nil
	}
	tolerance := paymentAmountToleranceForCurrency(PaymentOrderCurrency(p.Order))
	refunded, err := s.refundSucceededTotal(ctx, p.Order)
	if err != nil {
		return infraerrors.InternalServer("REFUND_LOOKUP_FAILED", "failed to compute already refunded amount")
	}
	remaining := p.Order.Amount - refunded
	if p.RefundAmount-remaining > tolerance {
		return infraerrors.BadRequest("REFUND_AMOUNT_EXCEEDED", "refund amount exceeds the refundable remainder").
			WithMetadata(map[string]string{"remaining": fmt.Sprintf("%.2f", remaining)})
	}
	// Carry the freshly read total forward so the terminal status decision
	// (refundResultStatus) and the cumulative refund_amount write both use it
	// instead of the value captured during PrepareRefund.
	p.RefundedBefore = refunded
	return nil
}

// restoreStatusFromLedger releases an order from the REFUNDING pin, deriving the
// status from the durable refund ledger rather than from the caller's snapshot.
//
// restoreStatus cannot be used on the rejection path: by the time a refund plan is
// rejected as stale, another refund may already have succeeded, so the plan's
// snapshot still says COMPLETED while the ledger says part of the order is gone.
// Reporting COMPLETED there would erase a real partial-refund outcome and let the
// whole order be refunded again. The ledger is the only authority for that.
func (s *PaymentService) restoreStatusFromLedger(ctx context.Context, p *RefundPlan) {
	if p == nil {
		return
	}
	status := OrderStatusCompleted
	if p.Order != nil {
		if refunded, err := s.refundSucceededTotal(ctx, p.Order); err == nil {
			status = refundedStatusForTotal(p.Order, refunded)
		} else {
			// Unknown ledger state: the order must not look fully refundable again.
			status = OrderStatusPartiallyRefunded
		}
	}
	if _, err := s.entClient.PaymentOrder.UpdateOneID(p.OrderID).SetStatus(status).Save(ctx); err != nil {
		slog.Error("[PaymentService] failed to release order from REFUNDING", "orderID", p.OrderID, "status", status, "error", err)
	}
}
