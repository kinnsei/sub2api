import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import AdminOrdersView from '../AdminOrdersView.vue'
import Select from '@/components/common/Select.vue'
import { ORDER_STATUSES } from '@/types/payment'
import type { PaymentOrder } from '@/types/payment'

const getOrders = vi.hoisted(() => vi.fn())

vi.mock('@/api/admin/payment', () => {
  const adminPaymentAPI = {
    getOrders,
    cancelOrder: vi.fn(),
    retryRecharge: vi.fn(),
    refundOrder: vi.fn(),
    queryRefund: vi.fn(),
  }
  return { adminPaymentAPI, default: adminPaymentAPI }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }),
}))

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

enableAutoUnmount(afterEach)

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 1,
    user_id: 9,
    amount: 100,
    pay_amount: 100,
    fee_rate: 0,
    payment_type: 'alipay',
    out_trade_no: 'sub2_20260420abcd1234',
    status: 'COMPLETED',
    order_type: 'balance',
    created_at: '2026-04-20T12:00:00Z',
    expires_at: '2026-04-20T12:30:00Z',
    refund_amount: 0,
    ...overrides,
  }
}

beforeEach(() => {
  vi.clearAllMocks()
  getOrders.mockResolvedValue({ data: { items: [orderFactory()], total: 1 } })
})

const stubs = {
  AppLayout: { template: '<div><slot /></div>' },
  OrderTable: {
    props: ['orders'],
    template: '<div><div v-for="row in orders" :key="row.id"><slot name="actions" :row="row" /></div></div>',
  },
  BaseDialog: true,
  AdminRefundDialog: true,
  OrderStatusBadge: true,
  Pagination: true,
  Icon: true,
  teleport: true,
}

async function mountView(order: Partial<PaymentOrder> = {}) {
  getOrders.mockResolvedValue({ data: { items: [orderFactory(order)], total: 1 } })
  const wrapper = mount(AdminOrdersView, { global: { stubs } })
  await flushPromises()
  return wrapper
}

describe('admin order status filter', () => {
  it('offers every status from the canonical union, including the refund ones', async () => {
    // ORDER_STATUSES is the same union the API types use; the filter must not
    // hardcode a narrower list or the new refund states cannot be filtered.
    const wrapper = await mountView()
    const selects = wrapper.findAllComponents(Select)
    const options = selects[0].props('options') as { value: string }[]
    const values = options.map(option => option.value)

    for (const status of ORDER_STATUSES) {
      expect(values, `filter is missing ${status}`).toContain(status)
    }
  })

  it('filters by REFUNDING and PARTIALLY_REFUNDED', async () => {
    const wrapper = await mountView()
    const statusSelect = wrapper.findAllComponents(Select)[0]

    const selectStatus = async (status: string) => {
      statusSelect.vm.$emit('update:modelValue', status)
      statusSelect.vm.$emit('change', status)
      await flushPromises()
    }

    await selectStatus('REFUNDING')
    expect(getOrders).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'REFUNDING' }))

    await selectStatus('PARTIALLY_REFUNDED')
    expect(getOrders).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'PARTIALLY_REFUNDED' }))
  })
})

describe('admin order row actions', () => {
  it('allows refunding a COMPLETED balance order', async () => {
    const wrapper = await mountView({ status: 'COMPLETED' })
    expect(wrapper.text()).toContain('payment.admin.refund')
  })

  it('allows refunding a PARTIALLY_REFUNDED balance order', async () => {
    const wrapper = await mountView({ status: 'PARTIALLY_REFUNDED' })
    expect(wrapper.text()).toContain('payment.admin.refund')
  })

  it('allows retrying fulfillment for PAID and RECHARGING orders', async () => {
    for (const status of ['PAID', 'RECHARGING'] as const) {
      const wrapper = await mountView({ status })
      expect(wrapper.text(), `${status} should offer a retry`).toContain('payment.admin.retry')
      wrapper.unmount()
    }
  })

  it('does not offer a refund for a state the backend cannot refund', async () => {
    const wrapper = await mountView({ status: 'PENDING' })
    expect(wrapper.text()).not.toContain('payment.admin.refund')
  })
})

// The refund endpoint requires an Idempotency-Key, and its lifetime must match the
// refund *intent*: stable across retries of one intent (double-click, timeout
// retry, or re-submitting after the backend asks for an explicit force
// confirmation) so the backend replays instead of paying the gateway twice — yet
// different for a separate refund of the same amount, so two legitimate partial
// refunds are never collapsed into one.
describe('AdminOrdersView refund idempotency key', () => {
  beforeEach(() => {
    getOrders.mockReset()
    getOrders.mockResolvedValue({ data: { items: [orderFactory({ id: 5, status: 'COMPLETED', order_type: 'balance' })], total: 1 } })
  })

  // Drive the view's own refund submission and inspect the key it passes.
  async function submitRefund(times: number, force = false) {
    const wrapper = mount(AdminOrdersView, { global: { stubs } })
    await flushPromises()
    const vm = wrapper.vm as unknown as {
      openRefundDialog: (o: PaymentOrder) => void
      handleRefund: (d: { amount: number; reason: string; deduct_balance: boolean; force: boolean }) => Promise<void>
    }
    vm.openRefundDialog(orderFactory({ id: 5, status: 'COMPLETED', order_type: 'balance' }) as PaymentOrder)
    for (let i = 0; i < times; i += 1) {
      await vm.handleRefund({ amount: 10, reason: 'r', deduct_balance: false, force })
      await flushPromises()
    }
    return wrapper
  }

  it('reuses one key when the same intent is retried', async () => {
    const { adminPaymentAPI } = await import('@/api/admin/payment')
    const refundOrder = vi.mocked(adminPaymentAPI.refundOrder)
    refundOrder.mockReset()
    // First attempt fails so the admin retries the identical payload.
    refundOrder.mockResolvedValue({ data: { success: false, warning: 'boom' } } as never)

    await submitRefund(2)

    expect(refundOrder).toHaveBeenCalledTimes(2)
    const firstKey = refundOrder.mock.calls[0][2]
    const secondKey = refundOrder.mock.calls[1][2]
    expect(firstKey).toBeTruthy()
    expect(secondKey).toBe(firstKey)
  })

  it('rotates the key when a separate refund intent is started', async () => {
    const { adminPaymentAPI } = await import('@/api/admin/payment')
    const refundOrder = vi.mocked(adminPaymentAPI.refundOrder)
    refundOrder.mockReset()
    refundOrder.mockResolvedValue({ data: { success: false, warning: 'boom' } } as never)

    // First intent.
    const wrapper = await submitRefund(1)
    // Close the dialog and start a fresh intent for the same amount.
    ;(wrapper.vm as unknown as { closeRefundDialog: () => void }).closeRefundDialog()
    await submitRefund(1)

    const keys = refundOrder.mock.calls.map((call) => call[2])
    expect(keys[0]).toBeTruthy()
    expect(keys[1]).toBeTruthy()
    expect(keys[1]).not.toBe(keys[0])
  })
})
