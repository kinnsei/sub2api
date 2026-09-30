import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, mount } from '@vue/test-utils'
import type { PaymentOrder } from '@/types/payment'
import AdminRefundDialog from '../AdminRefundDialog.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
enableAutoUnmount(afterEach)

async function openRefund(overrides: Partial<PaymentOrder> = {}) {
  const order: PaymentOrder = {
    id: 1, user_id: 10, amount: 100, pay_amount: 100, fee_rate: 0,
    payment_type: 'stripe', out_trade_no: 'order-1', status: 'COMPLETED',
    order_type: 'balance', created_at: '2026-09-01', expires_at: '2026-09-02',
    refund_amount: 0, ...overrides
  }
  const wrapper = mount(AdminRefundDialog, {
    props: { show: false, order, userBalance: 50 },
    global: { stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
  })
  await wrapper.setProps({ show: true })
  return wrapper
}

const warning = 'payment.admin.insufficientBalance'

describe('refund balance warning', () => {
  it('compares the available balance with the edited refund amount', async () => {
    const wrapper = await openRefund()
    expect(wrapper.text()).toContain(warning)
    await wrapper.get('input[type="number"]').setValue('20')
    expect(wrapper.text()).not.toContain(warning)
    await wrapper.get('input[type="number"]').setValue('75')
    expect(wrapper.text()).toContain(warning)
  })

  it('does not warn when the balance exactly covers the refund', async () => {
    const wrapper = await openRefund()
    await wrapper.get('input[type="number"]').setValue('50')
    expect(wrapper.text()).not.toContain(warning)
  })

  it('uses the remaining amount for an already partially refunded order', async () => {
    const wrapper = await openRefund({ status: 'PARTIALLY_REFUNDED', refund_amount: 80 })
    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('20')
    expect(wrapper.text()).not.toContain(warning)
  })

  it('keeps the warning hidden when balance deduction is disabled', async () => {
    const wrapper = await openRefund()
    await wrapper.get('#deduct-balance').setValue(false)
    expect(wrapper.text()).not.toContain(warning)
  })
})

describe('refundable cap', () => {
  const exceedsKey = 'payment.admin.refundAmountExceedsRemaining'

  it('caps the remainder by the refund already recorded for a partial refund', async () => {
    const wrapper = await openRefund({ status: 'PARTIALLY_REFUNDED', refund_amount: 20 })

    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('80')
  })

  it('treats a fully refunded order as having nothing left to refund', async () => {
    // refund_amount holds only the last installment (20), not the cumulative
    // total, so deriving the cap from it would over-state the remainder as 80.
    const wrapper = await openRefund({ status: 'REFUNDED', refund_amount: 20 })

    expect((wrapper.get('input[type="number"]').element as HTMLInputElement).value).toBe('0')
  })

  it('does not treat an in-flight refund request as money already refunded', async () => {
    // REFUND_REQUESTED pre-fills the requested amount so the admin can approve
    // it, but nothing has left the account yet — the request must not shrink the
    // refundable cap the way a completed partial refund does.
    const wrapper = await openRefund({ status: 'REFUND_REQUESTED', refund_amount: 30 })

    const input = wrapper.get('input[type="number"]')
    expect((input.element as HTMLInputElement).value).toBe('30')

    await input.setValue('90')
    expect(wrapper.text()).not.toContain(exceedsKey)
  })

  it('explains an over-cap amount inline instead of submitting silently', async () => {
    const wrapper = await openRefund({ status: 'PARTIALLY_REFUNDED', refund_amount: 80 })

    const input = wrapper.get('input[type="number"]')
    await input.setValue('50')
    expect(wrapper.text()).toContain(exceedsKey)

    await wrapper.get('form').trigger('submit')
    expect(wrapper.emitted('confirm')).toBeUndefined()

    await input.setValue('20')
    expect(wrapper.text()).not.toContain(exceedsKey)

    await wrapper.get('form').trigger('submit')
    expect(wrapper.emitted('confirm')).toHaveLength(1)
  })
})
