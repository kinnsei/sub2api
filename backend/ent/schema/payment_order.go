package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// PaymentOrder holds the schema definition for the PaymentOrder entity.
//
// 删除策略：硬删除
// PaymentOrder 使用硬删除而非软删除，原因如下：
//   - 订单通过 status 字段追踪完整生命周期，无需依赖软删除
//   - 订单审计通过 PaymentAuditLog 表记录，删除前可归档
//   - 减少查询复杂度，避免软删除过滤开销
type PaymentOrder struct {
	ent.Schema
}

func (PaymentOrder) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entsql.Annotation{Table: "payment_orders"},
	}
}

func (PaymentOrder) Fields() []ent.Field {
	return []ent.Field{
		// 用户信息（冗余存储，避免关联查询）
		field.Int64("user_id"),
		field.String("user_email").
			MaxLen(255),
		field.String("user_name").
			MaxLen(100),
		field.String("user_notes").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		// 金额信息
		field.Float("amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.Float("pay_amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}),
		field.Float("fee_rate").
			SchemaType(map[string]string{dialect.Postgres: "decimal(10,4)"}).
			Default(0),
		field.String("recharge_code").
			MaxLen(64),

		// 支付信息
		field.String("out_trade_no").
			MaxLen(64).
			Default(""),
		field.String("payment_type").
			MaxLen(30),
		field.String("payment_trade_no").
			MaxLen(128),
		field.String("pay_url").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.String("qr_code").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.String("qr_code_img").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		// 订单类型 & 订阅关联
		field.String("order_type").
			MaxLen(20).
			Default("balance"),
		field.Int64("plan_id").
			Optional().
			Nillable(),
		field.Int64("subscription_group_id").
			Optional().
			Nillable(),
		field.Int("subscription_days").
			Optional().
			Nillable(),
		field.String("provider_instance_id").
			Optional().
			Nillable().
			MaxLen(64),
		field.String("provider_key").
			Optional().
			Nillable().
			MaxLen(30),
		// currency 是订单**实际结算币种**，在下单时确定并冻结。
		//
		// 此前只能从 provider_snapshot JSON 反推，而该快照只对 Stripe/Airwallex/
		// 微信写入 currency：支付宝与 EasyPay 订单无从反推，只能默认 CNY。把币种
		// 显式落列后，所有渠道的币种都在下单时确定，后续金额容差
		// （paymentAmountToleranceForCurrency）与展示都不再依赖快照内容。
		field.String("currency").
			MaxLen(3).
			Default(""),
		field.JSON("provider_snapshot", map[string]any{}).
			Optional().
			SchemaType(map[string]string{dialect.Postgres: "jsonb"}),

		// 状态
		field.String("status").
			MaxLen(30).
			Default("PENDING"),

		// 退款信息
		field.Float("refund_amount").
			SchemaType(map[string]string{dialect.Postgres: "decimal(20,2)"}).
			Default(0),
		field.String("refund_reason").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.Time("refund_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Bool("force_refund").
			Default(false),
		field.Time("refund_requested_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("refund_request_reason").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),
		field.String("refund_requested_by").
			Optional().
			Nillable().
			MaxLen(20),

		// 时间节点
		field.Time("expires_at").
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("paid_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("completed_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.Time("failed_at").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "timestamptz"}),
		field.String("failed_reason").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		// 履约自动重试次数。
		//
		// 该计数原本通过统计 FULFILLMENT_FAILED 审计行得出，但审计日志在
		// (order_id, action) 上有唯一索引（131 号迁移），同一订单最多只有一行，
		// 因此计数永远停在 1，达不到重试上限（5），卡住的订单会被无限重试。
		// 计数改为本列持久化，审计行仅保留为人类可读的观测记录。
		field.Int("fulfillment_attempts").
			Default(0),

		// 来源信息
		field.String("client_ip").
			MaxLen(50),
		field.String("src_host").
			MaxLen(255),
		field.String("src_url").
			Optional().
			Nillable().
			SchemaType(map[string]string{dialect.Postgres: "text"}),

		// 时间戳
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

func (PaymentOrder) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("payment_orders").
			Field("user_id").
			Unique().
			Required(),
	}
}

func (PaymentOrder) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("out_trade_no").
			Unique().
			Annotations(entsql.IndexWhere("out_trade_no <> ''")),
		index.Fields("user_id"),
		index.Fields("status"),
		index.Fields("expires_at"),
		index.Fields("created_at"),
		index.Fields("paid_at"),
		index.Fields("payment_type", "paid_at"),
		index.Fields("order_type"),
		// 下单限额校验按实例/套餐统计在途订单。
		index.Fields("provider_instance_id").
			StorageKey("idx_payment_orders_provider_instance_id"),
		index.Fields("plan_id").
			StorageKey("idx_payment_orders_plan_id"),
	}
}
