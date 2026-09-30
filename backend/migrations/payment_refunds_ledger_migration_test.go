package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stripSQLComments removes `--` line comments so assertions inspect executable SQL
// rather than the explanatory prose around it.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(strings.Fields(strings.Join(out, "\n")), " ")
}

// payment_refunds 是退款分期台账，取代此前被 (order_id, action) 唯一索引压成单行的
// REFUND_SUCCESS 审计记录。这些断言锁定迁移的关键结构，避免后续误删约束。
func TestPaymentRefundsLedgerMigration(t *testing.T) {
	content, err := FS.ReadFile("263_payment_refunds_ledger.sql")
	require.NoError(t, err)

	code := stripSQLComments(string(content))

	require.Contains(t, code, "CREATE TABLE IF NOT EXISTS payment_refunds")
	// 金额必须与 payment_orders.amount 同样的精度，否则剩余额度会出现舍入误差。
	require.Contains(t, code, "amount DECIMAL(20,2) NOT NULL")
	require.Contains(t, code, "gateway_amount DECIMAL(20,2) NOT NULL DEFAULT 0")
	require.Contains(t, code, "status VARCHAR(30) NOT NULL DEFAULT 'REFUNDING'")
	require.Contains(t, code, "deduction_rollback_ok BOOLEAN NOT NULL DEFAULT TRUE")

	// 幂等键：同一分期的重试必须命中同一行。
	require.Contains(t, code, "CREATE UNIQUE INDEX IF NOT EXISTS paymentrefund_order_id_refund_no ON payment_refunds (order_id, refund_no)")

	// 同一订单最多一个在途分期。缺少它时，「退款 A 处于 REFUND_PENDING 时又发起
	// 退款 B」可让两笔都通过订单状态 CAS 并同时打到渠道。
	require.Contains(t, code, "CREATE UNIQUE INDEX IF NOT EXISTS paymentrefund_order_id_active ON payment_refunds (order_id) WHERE status IN ('REFUNDING', 'PENDING')")

	// 回填必须用 substring 提取金额，而不是把 TEXT 直接 CAST 成 jsonb：
	// 历史 detail 可能是非法 JSON，CAST 会让整个迁移语句报错。
	require.Contains(t, code, `substring(detail FROM '"refundAmount"\s*:\s*([0-9]+(?:\.[0-9]+)?)')`)
	require.NotContains(t, code, "::jsonb")

	// 回填方向必须保守：取审计累计与 refund_amount 的较大值，宁可少算可退额度
	// （只会拒绝退款），也绝不可多算（会重复打款给渠道）。
	require.Contains(t, code, "WHEN a.total > po.refund_amount THEN a.total ELSE po.refund_amount END")
	require.Contains(t, code, "ON CONFLICT (order_id, refund_no) DO NOTHING")

	// 这是普通迁移（非 *_notx），不得出现 CONCURRENTLY。
	require.NotContains(t, strings.ToUpper(code), "CONCURRENTLY")
}

// fulfillment_attempts 让履约重试上限真正生效（审计计数被唯一索引压在 1）。
func TestPaymentOrderFulfillmentAttemptsMigration(t *testing.T) {
	content, err := FS.ReadFile("264_payment_order_fulfillment_attempts.sql")
	require.NoError(t, err)

	code := stripSQLComments(string(content))

	require.Contains(t, code, "ALTER TABLE payment_orders ADD COLUMN IF NOT EXISTS fulfillment_attempts INTEGER NOT NULL DEFAULT 0")
	// 回填把历史 FULFILLMENT_FAILED 折算为 1 次，避免升级时把失败记录清零。
	require.Contains(t, code, "SET fulfillment_attempts = 1")
	require.Contains(t, code, "WHERE pal.action = 'FULFILLMENT_FAILED'")
	require.NotContains(t, strings.ToUpper(code), "CONCURRENTLY")
}

func TestPaymentOrdersCurrencyMigration(t *testing.T) {
	raw, err := FS.ReadFile("266_payment_orders_currency.sql")
	require.NoError(t, err)
	sql := stripSQLComments(string(raw))

	// The column must be added before any statement reads it.
	addIdx := strings.Index(sql, "ADD COLUMN IF NOT EXISTS currency")
	require.Greater(t, addIdx, -1, "migration must add payment_orders.currency")
	require.Contains(t, sql[addIdx:addIdx+80], "VARCHAR(3)")

	// Existing rows need a value: NOT NULL is only satisfiable because of the default.
	require.Contains(t, sql[addIdx:addIdx+120], "NOT NULL")
	require.Contains(t, sql[addIdx:addIdx+120], "DEFAULT ''")

	// The snapshot backfill must run before the default backfill, otherwise every
	// historical row would be stamped CNY and the snapshot value would be lost.
	snapshotIdx := strings.Index(sql, "provider_snapshot ->> 'currency'")
	defaultIdx := strings.Index(sql, "SET currency = 'CNY'")
	require.Greater(t, snapshotIdx, -1, "migration must backfill from the provider snapshot")
	require.Greater(t, defaultIdx, -1, "migration must backfill the remaining rows")
	require.Less(t, snapshotIdx, defaultIdx,
		"the snapshot backfill must precede the CNY default backfill")

	// Both backfills must be scoped to rows the column left empty, so re-running the
	// migration (or running it after new orders exist) never rewrites live data.
	require.Equal(t, 2, strings.Count(sql, "WHERE currency = ''"),
		"both backfills must be limited to rows with an empty currency")
}
