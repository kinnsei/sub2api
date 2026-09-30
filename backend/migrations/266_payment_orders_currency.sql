-- payment_orders.currency：订单结算币种显式落列。
--
-- 背景：此前订单币种只能从 provider_snapshot JSON 反推，而该快照只有
-- Stripe / Airwallex / 微信三个渠道写入 currency（见 payment_order.go 的
-- buildPaymentOrderProviderSnapshot）。支付宝与 EasyPay 订单无从反推，
-- PaymentOrderCurrency 只能回退到默认币种 CNY。把币种落列后，所有渠道
-- 的结算币种在下单时确定，金额容差与展示不再依赖快照内容。
--
-- 回填策略（保守，只做能确定的推断）：
--   1) 已有快照且含 currency 的订单（Stripe/Airwallex/微信）→ 取快照值；
--   2) 其余历史订单 → 默认币种 CNY。
-- 与 Go 侧 PaymentOrderCurrency 的回退行为完全一致，因此回填不会改变任何
-- 现有订单的语义。

ALTER TABLE payment_orders
    ADD COLUMN IF NOT EXISTS currency VARCHAR(3) NOT NULL DEFAULT '';

-- 快照币种是唯一可靠的来源；用 jsonb 取值并统一为大写三字母。
UPDATE payment_orders
SET currency = UPPER(BTRIM(provider_snapshot ->> 'currency'))
WHERE currency = ''
  AND provider_snapshot IS NOT NULL
  AND BTRIM(COALESCE(provider_snapshot ->> 'currency', '')) <> ''
  AND BTRIM(provider_snapshot ->> 'currency') ~ '^[A-Za-z]{3}$';

-- 其余历史订单沿用默认币种，与 Go 侧回退一致。
UPDATE payment_orders
SET currency = 'CNY'
WHERE currency = '';
