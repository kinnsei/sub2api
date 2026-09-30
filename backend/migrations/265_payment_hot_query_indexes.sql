-- 支付热查询索引补齐。
--
-- 这些条件在每次下单限额校验、配置删除、取消频率限制时都会走，但此前没有任何
-- 索引，全部退化为全表扫描：
--   - countPendingOrders / countPendingOrdersByPlan 按下单的实例与套餐统计在途订单
--   - checkCancelRateLimit 按 (action, operator, created_at) 统计取消次数
-- 三者都随数据量线性变慢，并会拖慢同表上的其他写事务。

-- 下单限额：按服务商实例统计在途订单。
CREATE INDEX IF NOT EXISTS idx_payment_orders_provider_instance_id
    ON payment_orders(provider_instance_id);

-- 下单限额：按套餐统计在途订单。
CREATE INDEX IF NOT EXISTS idx_payment_orders_plan_id
    ON payment_orders(plan_id);

-- 取消频率限制：action + operator 定值，created_at 定范围。
-- 列顺序与查询谓词一致（等值列在前、范围列在后），才能走索引区间扫描。
CREATE INDEX IF NOT EXISTS idx_payment_audit_logs_action_operator_created_at
    ON payment_audit_logs(action, operator, created_at);
