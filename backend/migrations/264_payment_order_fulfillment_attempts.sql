-- 履约自动重试次数。
--
-- 该计数原本由 FULFILLMENT_FAILED 审计行数得出，但 payment_audit_logs 在
-- (order_id, action) 上有唯一索引（131 号迁移），同一订单最多只有一行审计，
-- 因此计数永远停在 1、达不到重试上限（5），卡住的订单会被无限自动重试。
-- 计数改为订单列持久化后，上限才真正生效。
--
-- 回填：把已存在的 FULFILLMENT_FAILED 审计行折算为 1 次（唯一索引决定了历史
-- 数据最多也只能表达 1 次），避免升级时把已有失败记录清零而多给几次重试。
ALTER TABLE payment_orders
    ADD COLUMN IF NOT EXISTS fulfillment_attempts INTEGER NOT NULL DEFAULT 0;

UPDATE payment_orders po
SET fulfillment_attempts = 1
WHERE po.fulfillment_attempts = 0
  AND EXISTS (
      SELECT 1 FROM payment_audit_logs pal
      WHERE pal.action = 'FULFILLMENT_FAILED'
        AND pal.order_id = po.id::text
  );
