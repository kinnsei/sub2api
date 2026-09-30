-- 退款台账：把「每次退款分期明细」从 payment_audit_logs 迁到独立表。
--
-- 背景（P0 超额退款）：131 号迁移为支付审计日志建立了 (order_id, action) 唯一索引，
-- 而退款流程把 payment_audit_logs 当作「可多条的分期台账」使用：
--   - refundedAmountTotal 汇总所有 REFUND_SUCCESS 行的 refundAmount
--   - writeAuditLog 只 slog.Error，不向上返回错误
-- 于是同一订单的第二条及以后的 REFUND_SUCCESS 被静默丢弃，已退总额被算小、
-- 剩余可退额度被算大，空白金额退款会按错误的剩余额度再次打到渠道（例：100 元订单
-- 已退 40+30，剩余被算成 60 而非 30），造成超额退款。
-- 同样的唯一索引把 FULFILLMENT_FAILED 限制为一行，使履约重试上限（5 次）永远达不到。
--
-- 本迁移只负责建表 + 一次性回填。权威来源切换由代码侧完成；回填方向一律保守
-- （宁可少算可退额度，也不可多算——少算只会拒绝退款，多算会重复打款给渠道）。

CREATE TABLE IF NOT EXISTS payment_refunds (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL,
    refund_no VARCHAR(64) NOT NULL,
    amount DECIMAL(20,2) NOT NULL,
    gateway_amount DECIMAL(20,2) NOT NULL DEFAULT 0,
    currency VARCHAR(10) NOT NULL DEFAULT '',
    reason TEXT,
    -- REFUNDING / PENDING / SUCCEEDED / FAILED（仅 SUCCEEDED 计入已退总额）
    status VARCHAR(30) NOT NULL DEFAULT 'REFUNDING',
    provider_refund_id VARCHAR(128) NOT NULL DEFAULT '',
    operator VARCHAR(100) NOT NULL DEFAULT 'system',
    deduction_type VARCHAR(20) NOT NULL DEFAULT 'none',
    balance_deducted DECIMAL(20,2) NOT NULL DEFAULT 0,
    sub_days_deducted INTEGER NOT NULL DEFAULT 0,
    deduction_rollback_ok BOOLEAN NOT NULL DEFAULT TRUE,
    force BOOLEAN NOT NULL DEFAULT FALSE,
    failure_reason TEXT,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 同一分期的重试只落一行台账（幂等键）。
CREATE UNIQUE INDEX IF NOT EXISTS paymentrefund_order_id_refund_no
    ON payment_refunds (order_id, refund_no);

CREATE INDEX IF NOT EXISTS paymentrefund_order_id ON payment_refunds (order_id);
CREATE INDEX IF NOT EXISTS paymentrefund_status ON payment_refunds (status);

-- 同一订单最多一个在途分期（REFUNDING/PENDING）。
-- 仅有 payment_orders.status 的 CAS 不足以防住这种情况：退款 A 已处于
-- REFUND_PENDING（订单状态回到可退的 REFUND_PENDING）时又发起退款 B，
-- B 会通过 CAS，于是两笔都打到渠道、合计超过订单金额。
CREATE UNIQUE INDEX IF NOT EXISTS paymentrefund_order_id_active
    ON payment_refunds (order_id)
    WHERE status IN ('REFUNDING', 'PENDING');

-- 回填 1：把仍然留存的 REFUND_SUCCESS 审计行转成 SUCCEEDED 台账。
-- 用 substring 从 detail 里提取数字，而不是 detail::jsonb->>'refundAmount'：
-- detail 是 TEXT，历史行里可能存在非法 JSON，直接 CAST 会在整个语句上报错，
-- 而 substring 永远不会抛错。正则同时保证随后的 ::numeric 一定合法。
--
-- 金额取 GREATEST(审计累计, payment_orders.refund_amount)：refund_amount 记录的是
-- 累计已退额（见 markRefundOk），因此当审计行已被唯一索引吞掉时，
-- GREATEST 能把已退总额补回到真实值，避免再次超额退款。
WITH audit_sums AS (
    SELECT order_id,
           SUM((substring(detail FROM '"refundAmount"\s*:\s*([0-9]+(?:\.[0-9]+)?)'))::numeric) AS total
    FROM payment_audit_logs
    WHERE action = 'REFUND_SUCCESS'
      AND order_id ~ '^[0-9]+$'
      AND substring(detail FROM '"refundAmount"\s*:\s*([0-9]+(?:\.[0-9]+)?)') IS NOT NULL
    GROUP BY order_id
)
INSERT INTO payment_refunds (order_id, refund_no, amount, currency, reason, status, operator, completed_at)
SELECT po.id,
       'backfill:' || po.id,
       CASE
           WHEN po.status = 'REFUND_PENDING' THEN a.total
           WHEN a.total > po.refund_amount THEN a.total
           ELSE po.refund_amount
       END,
       '',
       'legacy refund backfill from payment_audit_logs',
       'SUCCEEDED',
       'system',
       po.refund_at
FROM audit_sums a
JOIN payment_orders po ON po.id = a.order_id::bigint
WHERE a.total > 0
ON CONFLICT (order_id, refund_no) DO NOTHING;

-- 回填 2：仍在等待渠道确认的退款（REFUND_PENDING）落一条 PENDING 台账。
-- markRefundPending 会把 refund_amount 写成「本次分期金额」，因此这里直接用
-- refund_amount。PENDING 不计入已退总额，只用于后续 QueryAndFinalizeRefund 对账。
INSERT INTO payment_refunds (order_id, refund_no, amount, currency, reason, status, operator)
SELECT po.id,
       'backfill-pending:' || po.id,
       po.refund_amount,
       '',
       'legacy refund pending backfill',
       'PENDING',
       'system'
FROM payment_orders po
WHERE po.status = 'REFUND_PENDING'
  AND po.refund_amount > 0
ON CONFLICT (order_id, refund_no) DO NOTHING;

-- 回填 3：安全网。若某订单的 SUCCEEDED 台账合计仍小于 payment_orders.refund_amount
-- （即审计行已丢失且上面 GREATEST 未覆盖，例如历史状态为 REFUNDED/PARTIALLY_REFUNDED
-- 但 refund_amount 被后续操作改写），补一条差额行。
-- 方向保守：只会让可退额度变小，绝不会让已退总额变小。
INSERT INTO payment_refunds (order_id, refund_no, amount, currency, reason, status, operator, completed_at)
SELECT po.id,
       'backfill-topup:' || po.id,
       po.refund_amount - COALESCE(used.total, 0),
       '',
       'legacy refund topup to match payment_orders.refund_amount',
       'SUCCEEDED',
       'system',
       po.refund_at
FROM payment_orders po
LEFT JOIN (
    SELECT order_id, SUM(amount) AS total
    FROM payment_refunds
    WHERE status = 'SUCCEEDED'
    GROUP BY order_id
) used ON used.order_id = po.id
WHERE po.status IN ('REFUNDED', 'PARTIALLY_REFUNDED')
  AND po.refund_amount > COALESCE(used.total, 0)
ON CONFLICT (order_id, refund_no) DO NOTHING;
