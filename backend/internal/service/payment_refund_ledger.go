package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentorder"
	"github.com/Wei-Shaw/sub2api/ent/paymentrefund"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// 退款台账（payment_refunds）的状态取值。
//
// 只有 SUCCEEDED 计入「已退总额」，因此：
//
//	剩余可退额度 = payment_orders.amount - SUM(amount) WHERE status = 'SUCCEEDED'
//
// REFUNDING / PENDING 是「在途」状态，不计入额度，但同一订单只允许存在一个在途分期
// （由 paymentrefund_order_id_active 局部唯一索引保证），避免并发退款各自通过状态
// CAS 后同时打到渠道。
const (
	RefundLedgerStatusRefunding = "REFUNDING"
	RefundLedgerStatusPending   = "PENDING"
	RefundLedgerStatusSucceeded = "SUCCEEDED"
	RefundLedgerStatusFailed    = "FAILED"
)

// refundLedgerClient 抽象出「在事务内或用全局 client 操作台账」的两种用法。
type refundLedgerClient interface {
	PaymentRefundCreate() *dbent.PaymentRefundCreate
	PaymentRefundUpdate() *dbent.PaymentRefundUpdate
	PaymentRefundQuery() *dbent.PaymentRefundQuery
}

// paymentServiceRefundClient 让 *dbent.Client 满足 refundLedgerClient。
type paymentServiceRefundClient struct{ c *dbent.Client }

func (p paymentServiceRefundClient) PaymentRefundCreate() *dbent.PaymentRefundCreate {
	return p.c.PaymentRefund.Create()
}
func (p paymentServiceRefundClient) PaymentRefundUpdate() *dbent.PaymentRefundUpdate {
	return p.c.PaymentRefund.Update()
}
func (p paymentServiceRefundClient) PaymentRefundQuery() *dbent.PaymentRefundQuery {
	return p.c.PaymentRefund.Query()
}

// refundSucceededTotal 汇总订单已成功退款的总额，是剩余额度的唯一可信来源。
//
// 该值不再从 payment_audit_logs 的 REFUND_SUCCESS 行汇总：审计日志被
// (order_id, action) 唯一索引限制为单行（见 131 号迁移），且 writeAuditLog 会静默
// 吞掉插入错误，因此审计行无法表达多次分期退款。
//
// 为兼容尚未回填的历史订单，当台账为空时回退到审计汇总与 payment_orders.refund_amount
// 中的较大值（refund_amount 记录的是累计已退额）。回退方向保守：取较大值只会让可退
// 额度变小，绝不会放大到重复打款。
func (s *PaymentService) refundSucceededTotal(ctx context.Context, o *dbent.PaymentOrder) (float64, error) {
	if s == nil || s.entClient == nil || o == nil {
		return 0, fmt.Errorf("refund ledger: no client or order")
	}
	total, err := s.refundSucceededTotalWith(ctx, paymentServiceRefundClient{s.entClient}, o.ID)
	if err != nil {
		return 0, err
	}
	if total > 0 {
		return total, nil
	}
	// 台账为空：回退到历史数据源，避免升级后把已退额度算成 0 而重复退款。
	legacy, err := s.legacyRefundedTotal(ctx, o)
	if err != nil {
		return 0, err
	}
	return legacy, nil
}

// legacyRefundedTotal 是台账回填缺失时的保守兜底：取审计汇总与 refund_amount 的较大值。
func (s *PaymentService) legacyRefundedTotal(ctx context.Context, o *dbent.PaymentOrder) (float64, error) {
	auditTotal, err := s.auditRefundedTotal(ctx, o.ID)
	if err != nil {
		return 0, err
	}
	refundAmount := o.RefundAmount
	if refundAmount < 0 {
		refundAmount = 0
	}
	if auditTotal > refundAmount {
		return auditTotal, nil
	}
	return refundAmount, nil
}

func (s *PaymentService) refundSucceededTotalWith(ctx context.Context, c refundLedgerClient, orderID int64) (float64, error) {
	rows, err := c.PaymentRefundQuery().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.StatusEQ(RefundLedgerStatusSucceeded),
		).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("query refund ledger: %w", err)
	}
	var total float64
	for _, row := range rows {
		if row != nil {
			total += row.Amount
		}
	}
	return total, nil
}

// nextRefundNo 生成退款分期号（幂等键）。
//
// 分期序号取「已成功 + 在途」的行数，因此：
//   - 同一分期的重试会算出同一个号，upsert 只覆盖原行，不会重复计入已退总额；
//   - 一次成功退款之后再发起新退款会得到新的号，成为独立分期。
//
// 并发场景下即使两次调用算出同一个号，也会被「同一订单仅一个在途分期」的局部唯一
// 索引拦下，不会双双打到渠道。
func (s *PaymentService) nextRefundNo(ctx context.Context, c refundLedgerClient, orderID int64) (string, error) {
	n, err := c.PaymentRefundQuery().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.StatusIn(RefundLedgerStatusSucceeded, RefundLedgerStatusPending),
		).
		Count(ctx)
	if err != nil {
		return "", fmt.Errorf("count refund ledger installments: %w", err)
	}
	// The value doubles as the gateway-facing refund reference (Alipay
	// out_request_no), which permits only letters, digits and underscores, so keep
	// it strictly alphanumeric and deterministic: the same installment always
	// derives the same reference, and different installments never collide.
	return fmt.Sprintf("sub2o%dr%d", orderID, n), nil
}

// upsertRefundLedgerClaim 在事务内抢占一个退款分期（状态 REFUNDING）。
//
// 若该订单已有在途分期（REFUNDING/PENDING），局部唯一索引会返回唯一键冲突，
// 这里转换为 Conflict 错误，使并发退款无法同时进行。
func (s *PaymentService) upsertRefundLedgerClaim(ctx context.Context, c refundLedgerClient, orderID int64, refundNo string, amount, gatewayAmount float64, currency, reason, operator string, force bool) error {
	err := c.PaymentRefundCreate().
		SetOrderID(orderID).
		SetRefundNo(refundNo).
		SetAmount(amount).
		SetGatewayAmount(gatewayAmount).
		SetCurrency(currency).
		SetReason(reason).
		SetStatus(RefundLedgerStatusRefunding).
		SetOperator(operator).
		SetForce(force).
		OnConflictColumns(paymentrefund.FieldOrderID, paymentrefund.FieldRefundNo).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		if isRefundLedgerConflict(err) {
			return infraerrors.Conflict("REFUND_IN_PROGRESS", "another refund for this order is still in progress")
		}
		return fmt.Errorf("claim refund ledger: %w", err)
	}
	return nil
}

// isRefundLedgerConflict 判断错误是否为唯一键冲突（在途分期已存在）。
func isRefundLedgerConflict(err error) bool {
	if err == nil {
		return false
	}
	if dbent.IsConstraintError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	// SQLite 与 PostgreSQL 的措辞不同，两种都要覆盖。
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "unique failed")
}

// markRefundLedgerSucceeded 把分期标记为已成功，并记录扣减与渠道退款单号。
//
// 若该分期在台账里不存在（历史订单：退款发起时台账尚未上线），这里会补建一行，
// 使台账始终能覆盖全部已退金额。返回是否确实落库。
func (s *PaymentService) markRefundLedgerSucceeded(ctx context.Context, c refundLedgerClient, p *RefundPlan) error {
	refundNo := strings.TrimSpace(p.RefundNo)
	if refundNo != "" {
		n, err := c.PaymentRefundUpdate().
			Where(
				paymentrefund.OrderIDEQ(p.OrderID),
				paymentrefund.RefundNoEQ(refundNo),
			).
			SetStatus(RefundLedgerStatusSucceeded).
			SetBalanceDeducted(p.BalanceToDeduct).
			SetSubDaysDeducted(p.SubDaysToDeduct).
			SetDeductionRollbackOk(true).
			SetCompletedAt(time.Now()).
			ClearFailureReason().
			Save(ctx)
		if err != nil {
			return fmt.Errorf("mark refund ledger succeeded: %w", err)
		}
		if n > 0 {
			return nil
		}
		// 没有匹配行：落到下面的补建逻辑。
	}
	if refundNo == "" {
		// 历史退款没有分期号，用当时的累计已退额生成一个稳定编号，
		// 使同一订单重复确认也只会命中同一行。
		refundNo = fmt.Sprintf("sub2o%dlegacy%s", p.OrderID, formatLedgerKey(p.RefundedBefore+p.RefundAmount))
	}
	if err := c.PaymentRefundCreate().
		SetOrderID(p.OrderID).
		SetRefundNo(refundNo).
		SetAmount(p.RefundAmount).
		SetGatewayAmount(p.GatewayAmount).
		SetCurrency(PaymentOrderCurrency(p.Order)).
		SetReason(p.Reason).
		SetStatus(RefundLedgerStatusSucceeded).
		SetOperator("admin").
		SetBalanceDeducted(p.BalanceToDeduct).
		SetSubDaysDeducted(p.SubDaysToDeduct).
		SetDeductionRollbackOk(true).
		SetForce(p.Force).
		SetCompletedAt(time.Now()).
		OnConflictColumns(paymentrefund.FieldOrderID, paymentrefund.FieldRefundNo).
		UpdateNewValues().
		Exec(ctx); err != nil {
		return fmt.Errorf("record refund ledger success: %w", err)
	}
	p.RefundNo = refundNo
	return nil
}

// formatLedgerKey 把金额渲染成稳定的纯数字字符串（以分为单位），用于构造历史分期的
// 幂等键。只使用字母与数字，因为该键会作为退款单号发给渠道（支付宝 out_request_no
// 仅接受字母、数字与下划线）。
func formatLedgerKey(amount float64) string {
	return strconv.FormatInt(int64(math.Round(amount*100)), 10)
}

// markRefundLedgerFailed 把分期标记为失败，并记录失败原因。
func (s *PaymentService) markRefundLedgerFailed(ctx context.Context, c refundLedgerClient, orderID int64, refundNo, failureReason string, balanceDeducted float64, subDaysDeducted int, rollbackOK bool) error {
	if _, err := c.PaymentRefundUpdate().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.RefundNoEQ(refundNo),
		).
		SetStatus(RefundLedgerStatusFailed).
		SetFailureReason(failureReason).
		SetDeductionRollbackOk(rollbackOK).
		SetBalanceDeducted(balanceDeducted).
		SetSubDaysDeducted(subDaysDeducted).
		Save(ctx); err != nil {
		return fmt.Errorf("mark refund ledger failed: %w", err)
	}
	return nil
}

// activeRefundLedgerRow 读取订单当前的在途退款分期（REFUNDING/PENDING）。
// 用于最终确认退款结果时定位分期，避免依赖审计日志里的 REFUND_PENDING 明细。
func (s *PaymentService) activeRefundLedgerRow(ctx context.Context, orderID int64) (*dbent.PaymentRefund, error) {
	row, err := s.entClient.PaymentRefund.Query().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.StatusIn(RefundLedgerStatusPending, RefundLedgerStatusRefunding),
		).
		Order(dbent.Desc(paymentrefund.FieldID)).
		First(ctx)
	if err != nil {
		if dbent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// orderIDString 是 payment_audit_logs.order_id 的字符串形式。
func orderIDString(orderID int64) string {
	return strconv.FormatInt(orderID, 10)
}

// deleteRefundLedgerRow 删除一条已被完全回滚的退款分期。
//
// 仅在「渠道从未接受该笔退款」且扣减已成功回滚时调用：此时这笔分期在事实上
// 不存在，保留一行在途记录会通过局部唯一索引阻塞该订单后续的所有退款。
func (s *PaymentService) deleteRefundLedgerRow(ctx context.Context, orderID int64, refundNo string) error {
	if _, err := s.entClient.PaymentRefund.Delete().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.RefundNoEQ(refundNo),
			paymentrefund.StatusEQ(RefundLedgerStatusRefunding),
		).
		Exec(ctx); err != nil {
		return fmt.Errorf("delete refund ledger row: %w", err)
	}
	return nil
}

// upsertRefundLedgerPending 记录（或补建）一条等待渠道确认的退款分期。
//
// 对于台账上线之前发起的退款，p.RefundNo 可能为空：此时用「累计已退额」生成稳定
// 编号，使重复确认只会命中同一行。空编号若不处理，历史订单会直接插入失败。
func (s *PaymentService) upsertRefundLedgerPending(ctx context.Context, p *RefundPlan, providerRefundID string, rollbackOK bool) error {
	refundNo := strings.TrimSpace(p.RefundNo)
	if refundNo == "" {
		refundNo = fmt.Sprintf("sub2o%dlegacypending%s", p.OrderID, formatLedgerKey(p.RefundedBefore+p.RefundAmount))
	}
	p.RefundNo = refundNo
	err := s.entClient.PaymentRefund.Create().
		SetOrderID(p.OrderID).
		SetRefundNo(refundNo).
		SetAmount(p.RefundAmount).
		SetGatewayAmount(p.GatewayAmount).
		SetCurrency(PaymentOrderCurrency(p.Order)).
		SetReason(p.Reason).
		SetStatus(RefundLedgerStatusPending).
		SetProviderRefundID(providerRefundID).
		SetOperator("admin").
		SetBalanceDeducted(p.BalanceToDeduct).
		SetSubDaysDeducted(p.SubDaysToDeduct).
		SetDeductionRollbackOk(rollbackOK).
		SetForce(p.Force).
		OnConflictColumns(paymentrefund.FieldOrderID, paymentrefund.FieldRefundNo).
		UpdateNewValues().
		Exec(ctx)
	if err != nil {
		if isRefundLedgerConflict(err) {
			return infraerrors.Conflict("REFUND_IN_PROGRESS", "another refund for this order is still in progress")
		}
		return fmt.Errorf("record pending refund ledger: %w", err)
	}
	return nil
}

// stuckRefundSweepGracePeriod 是判定一笔退款「卡住」的静默时长。
// 取值需大于正常的渠道退款调用耗时，避免回收仍在进行中的退款。
const stuckRefundSweepGracePeriod = 15 * time.Minute

// SweepStuckRefunds 回收卡在 REFUNDING 的退款订单。
//
// REFUNDING 是 ExecuteRefund / finalizePendingRefundSuccess 在调用渠道前后设置的
// 中间状态。若进程在「已扣减用户余额」与「拿到渠道结果」之间退出，订单会永久停在
// REFUNDING：余额已扣，退款既没打到渠道也无法再被处理（PrepareRefund 原先不接受
// REFUNDING，也没有任何回收任务）。ExecuteRefund 现已在确认渠道未受理且扣减已成功
// 回滚时删除台账行，本任务处理那些连删除都没执行到的极端情况。
//
// 处理方式：把静默超时的 REFUNDING 订单与其在途台账行一并复位，
// 使退款可以重新走一遍完整流程（余额扣减会因台账行被删除而重新执行）。
//
// 与 RetryStuckFulfillments 一样，仅由 leader 实例的周期任务调用。
func (s *PaymentService) SweepStuckRefunds(ctx context.Context) (int, error) {
	staleBefore := time.Now().Add(-stuckRefundSweepGracePeriod)
	orders, err := s.entClient.PaymentOrder.Query().
		Where(
			paymentorder.StatusEQ(OrderStatusRefunding),
			paymentorder.UpdatedAtLTE(staleBefore),
		).
		Order(dbent.Asc(paymentorder.FieldUpdatedAt)).
		Limit(paymentFulfillmentAutoRetryLimit).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("query stuck refund orders: %w", err)
	}

	released := 0
	for _, o := range orders {
		// Read the in-flight installments first: whether the sweep may release this
		// order at all depends on whether a deduction may already have happened.
		rows, derr := s.entClient.PaymentRefund.Query().
			Where(
				paymentrefund.OrderIDEQ(o.ID),
				paymentrefund.StatusEQ(RefundLedgerStatusRefunding),
			).
			All(ctx)
		if derr != nil {
			slog.Warn("[PaymentService] list stuck refund installments failed", "orderID", o.ID, "error", derr)
			continue
		}

		// A deduction may have been applied and left un-rolled-back when either the
		// durable pre-deduction marker is set (deduction_rollback_ok=false) or an
		// amount was actually recorded. Note the marker is what catches the common
		// crash case: the amounts are only written once the whole refund finishes,
		// so a crash between the deduction and the gateway call leaves them at 0.
		deductionMayBeApplied := false
		for _, row := range rows {
			if !row.DeductionRollbackOk || row.BalanceDeducted > 0 || row.SubDaysDeducted > 0 {
				deductionMayBeApplied = true
			}
		}

		if deductionMayBeApplied {
			// Fail closed. The user's balance/days may already be gone and the
			// gateway may or may not have processed the refund. Returning the order
			// to a refundable state would risk deducting the same money twice, so:
			// keep the installment row (its partial unique index keeps blocking new
			// refunds) and park the order in a visible, non-auto-retried state for a
			// human to reconcile.
			slog.Error("[CRITICAL] stuck refund left a deduction that may not be rolled back; manual reconciliation required",
				"orderID", o.ID, "installments", len(rows))
			if _, err := s.entClient.PaymentOrder.UpdateOneID(o.ID).
				SetStatus(OrderStatusRefundFailed).
				SetFailedAt(time.Now()).
				SetFailedReason("refund interrupted after deduction; may not be rolled back — verify user balance and gateway refund before retrying").
				Save(ctx); err != nil {
				slog.Error("[PaymentService] failed to park stuck refund order", "orderID", o.ID, "error", err)
				continue
			}
			s.writeAuditLog(ctx, o.ID, "REFUND_STUCK_NEEDS_REVIEW", "system", map[string]any{
				"previousStatus":   o.Status,
				"installments":     len(rows),
				"deductionApplied": true,
			})
			continue
		}

		// Nothing was deducted and no installment reached the gateway, so the
		// refund genuinely never happened and can be retried from scratch.
		succeeded, err := s.refundSucceededTotalWith(ctx, paymentServiceRefundClient{s.entClient}, o.ID)
		if err != nil {
			slog.Warn("[PaymentService] read refund ledger failed during sweep", "orderID", o.ID, "error", err)
			continue
		}
		// Derive the released status from the same ledger total the refund logic
		// uses, so the recovery paths cannot disagree about whether the order is
		// fully or only partly refunded.
		target := refundedStatusForTotal(o, succeeded)

		// Delete the in-flight installment so the partial unique index no longer
		// blocks a fresh attempt.
		for _, row := range rows {
			if _, err := s.entClient.PaymentRefund.Delete().Where(paymentrefund.IDEQ(row.ID)).Exec(ctx); err != nil {
				slog.Warn("[PaymentService] delete stuck refund installment failed", "orderID", o.ID, "refundNo", row.RefundNo, "error", err)
			}
		}

		if _, err := s.entClient.PaymentOrder.UpdateOneID(o.ID).
			SetStatus(target).
			ClearFailedAt().
			ClearFailedReason().
			Save(ctx); err != nil {
			slog.Warn("[PaymentService] release stuck refund order failed", "orderID", o.ID, "error", err)
			continue
		}

		s.writeAuditLog(ctx, o.ID, "REFUND_STUCK_RELEASED", "system", map[string]any{
			"previousStatus": o.Status,
			"restoredStatus": target,
			"installments":   len(rows),
		})
		released++
	}
	return released, nil
}

// markRefundLedgerDeductionInFlight 在**执行扣减之前**把该分期标记为
// 「可能已扣减且尚未回滚」。
//
// 为什么必须在扣减之前落库：扣减（扣余额 / 扣订阅天数）与渠道退款调用之间没有事务，
// 进程可能在「钱已经从用户账上扣掉」与「渠道结果落库」之间退出。此时台账若仍显示
// deduction_rollback_ok=true，重试与被回收后的再次退款都会**再扣一次**用户的钱。
// 提前置为 false 后，refundLedgerAllowsDeduction 会让重试跳过扣减，SweepStuckRefunds
// 也不会把订单重新放回可退池，从而不会二次扣款。
//
// 该标记只表达「可能已扣、且未回滚」，因此在确认「确实没扣成功」时必须用
// markRefundLedgerDeductionNotApplied 复位，否则会白白少扣用户的钱。
func (s *PaymentService) markRefundLedgerDeductionInFlight(ctx context.Context, orderID int64, refundNo string) error {
	refundNo = strings.TrimSpace(refundNo)
	if refundNo == "" {
		return nil
	}
	if _, err := s.entClient.PaymentRefund.Update().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.RefundNoEQ(refundNo),
		).
		SetDeductionRollbackOk(false).
		Save(ctx); err != nil {
		return fmt.Errorf("mark refund ledger deduction in flight: %w", err)
	}
	return nil
}

// markRefundLedgerDeductionNotApplied 复位 markRefundLedgerDeductionInFlight 的标记。
// 只在扣减调用明确返回错误（即确定未改动用户余额/订阅）时使用。
func (s *PaymentService) markRefundLedgerDeductionNotApplied(ctx context.Context, orderID int64, refundNo string) {
	refundNo = strings.TrimSpace(refundNo)
	if refundNo == "" {
		return
	}
	if _, err := s.entClient.PaymentRefund.Update().
		Where(
			paymentrefund.OrderIDEQ(orderID),
			paymentrefund.RefundNoEQ(refundNo),
		).
		SetDeductionRollbackOk(true).
		Save(ctx); err != nil {
		slog.Error("failed to clear refund ledger deduction-in-flight marker", "orderID", orderID, "refundNo", refundNo, "error", err)
	}
}

// refundedStatusForTotal maps "how much of this order has actually been refunded"
// to the order status that should be persisted.
//
// This used to be open-coded in three places (refundResultStatus,
// restoreStatusFromLedger and the stuck-refund sweep) and they drifted: the sweep
// only distinguished "nothing refunded" from "something refunded", so an order
// whose installments already covered the full amount was released as
// PARTIALLY_REFUNDED — leaving it with a refundable remainder and letting money go
// out twice. Keeping the boundary in one function is what prevents a fourth
// divergence; callers should pass a ledger-derived total rather than a snapshot.
//
// The "fully refunded" test mirrors the amount tolerance used elsewhere for
// comparing provider amounts to the order amount. That tolerance is deliberately
// NOT reused to decide "nothing was refunded": it is 0.01 for CNY and amounts are
// stored as DECIMAL(20,2), so `> tolerance` would report a legitimate 0.01 refund
// as COMPLETED and erase real money movement. Any total above zero is a partial
// refund.
func refundedStatusForTotal(order *dbent.PaymentOrder, refundedTotal float64) string {
	if order == nil {
		return OrderStatusPartiallyRefunded
	}
	if order.Amount > 0 {
		tolerance := paymentAmountToleranceForCurrency(PaymentOrderCurrency(order))
		if refundedTotal >= order.Amount-tolerance {
			return OrderStatusRefunded
		}
	}
	if refundedTotal > 0 {
		return OrderStatusPartiallyRefunded
	}
	return OrderStatusCompleted
}
