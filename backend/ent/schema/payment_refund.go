package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PaymentRefund holds the schema definition for the PaymentRefund entity.
//
// 删除策略：硬删除
//
// 退款台账。
//
// 背景：payment_orders.refund_amount 只能表达「累计退款额」这一个标量，而
// 「剩余可退额度」需要的是「每次退款分期金额之和」。此前该明细被写入
// payment_audit_logs 并以 (order_id, action) 聚合，但 131 号迁移为支付审计日志
// 建立了 (order_id, action) 唯一索引，导致同一订单的第二条及以后的
// REFUND_SUCCESS 审计行被静默丢弃（writeAuditLog 只记日志、不返回错误），
// 于是剩余额度被算大，空白金额退款会按错误的剩余额度再次打到渠道，造成超额退款。
//
// 因此退款分期明细改为本表持久化，并成为剩余额度的唯一可信来源：
//   - 剩余可退额度 = payment_orders.amount - SUM(payment_refunds.amount WHERE status='SUCCEEDED')
//   - 在途分期（REFUNDING/PENDING）不计入已退总额，但同一订单最多只允许一个：
//     否则「先发起 40 元退款（尚未确认）再发起 100 元退款」会让两笔都打到渠道。
//     该约束由 paymentrefund_order_id_active 局部唯一索引在数据库层保证。
//   - 卡住的 REFUNDING 由 SweepStuckRefunds 回收，不会永久阻塞后续退款；
//     且同一分期的重试会算出相同的 refund_no，直接更新原行而不触发该索引。
type PaymentRefund struct {
	ent.Schema
}

func (PaymentRefund) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "payment_refunds"},
	}
}

func (PaymentRefund) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("order_id"),

		// refund_no 是退款分期号（幂等键）。同一分期的重试会重新计算出
		// 相同的 refund_no，因此重试只会更新同一行，不会重复计入已退总额。
		field.String("refund_no").
			MaxLen(64).
			NotEmpty(),

		// 本分期金额（订单币种）与换算后的渠道金额。
		field.Float("amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.Float("gateway_amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}).
			Default(0),
		field.String("currency").
			MaxLen(10).
			Default(""),

		field.String("reason").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		// REFUNDING / PENDING / SUCCEEDED / FAILED
		// 只有 SUCCEEDED 计入已退总额。
		field.String("status").
			MaxLen(30).
			Default("REFUNDING"),

		// 渠道返回的退款单号，用于后续 QueryRefund 对账。
		field.String("provider_refund_id").
			MaxLen(128).
			Default(""),

		field.String("operator").
			MaxLen(100).
			Default("system"),

		// 扣减信息：余额/订阅天数扣减与回滚结果。原先这些状态靠
		// REFUND_ROLLBACK_FAILED 审计行的存在性表达，现在落在台账上。
		field.String("deduction_type").
			MaxLen(20).
			Default("none"),
		field.Float("balance_deducted").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}).
			Default(0),
		field.Int("sub_days_deducted").
			Default(0),
		field.Bool("deduction_rollback_ok").
			Default(true),
		field.Bool("force").
			Default(false),

		field.String("failure_reason").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		field.Time("completed_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("created_at").
			Immutable().
			Default(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now).
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
	}
}

func (PaymentRefund) Indexes() []ent.Index {
	return []ent.Index{
		// 同一分期的重试只落一行台账。
		index.Fields("order_id", "refund_no").
			Unique(),
		index.Fields("order_id"),
		index.Fields("status"),
		// 同一订单最多一个在途分期（REFUNDING/PENDING）。
		// 这是退款并发控制的核心：仅有 payment_orders.status 的 CAS 不足以防住
		// 「退款 A 处于 REFUND_PENDING 时又发起退款 B」——那时订单状态回到了可退
		// 状态，B 会通过 CAS，从而让两笔退款都打到渠道、合计超过订单金额。
		index.Fields("order_id").
			Unique().
			StorageKey("paymentrefund_order_id_active").
			Annotations(entsql.IndexWhere("status IN ('REFUNDING', 'PENDING')")),
	}
}
