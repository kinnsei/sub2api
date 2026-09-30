import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, mount } from '@vue/test-utils'
import UserOrdersView from '../UserOrdersView.vue'
import Select from '@/components/common/Select.vue'
import Pagination from '@/components/common/Pagination.vue'

const api = vi.hoisted(() => ({ getMyOrders: vi.fn(), getRefundEligibleProviders: vi.fn() }))
vi.mock('@/api/payment', () => ({ paymentAPI: api }))
vi.mock('@/stores', () => ({ useAppStore: () => ({ showError: vi.fn() }) }))
vi.mock('vue-router', () => ({ useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key })
}))
enableAutoUnmount(afterEach)

beforeEach(() => {
  vi.clearAllMocks()
  api.getMyOrders.mockResolvedValue({ data: { items: [], total: 100 } })
  api.getRefundEligibleProviders.mockResolvedValue({ data: { provider_instance_ids: [] } })
})

async function openOrders() {
  const wrapper = mount(UserOrdersView, {
    global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      OrderTable: true, BaseDialog: true, Icon: true, Pagination: true, teleport: true
    } }
  })
  await flushPromises()
  wrapper.getComponent(Pagination).vm.$emit('update:page', 4)
  await flushPromises()
  return wrapper
}

describe('order status filtering', () => {
  it('loads the first page with the selected status', async () => {
    const wrapper = await openOrders()
    const select = wrapper.getComponent(Select)
    await select.get('button').trigger('click')
    await select.findAll('[role="option"]').find(option => option.text() === 'payment.status.pending')!.trigger('click')
    await flushPromises()
    expect(api.getMyOrders).toHaveBeenLastCalledWith({ page: 1, page_size: 20, status: 'PENDING' })
    expect(wrapper.getComponent(Pagination).props('page')).toBe(1)
    expect(api.getMyOrders).toHaveBeenCalledTimes(3)
  })

  it('keeps the current page on manual refresh', async () => {
    const wrapper = await openOrders()
    await wrapper.get('[title="common.refresh"]').trigger('click')
    await flushPromises()
    expect(api.getMyOrders).toHaveBeenLastCalledWith({ page: 4, page_size: 20, status: undefined })
  })
})

function orderFactory(overrides: Record<string, unknown> = {}) {
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
    provider_instance_id: 'inst-1',
    ...overrides,
  }
}

async function mountWithOrder(order: Record<string, unknown>, eligibleProviders: string[] = ['inst-1']) {
  api.getMyOrders.mockResolvedValue({ data: { items: [order], total: 1 } })
  api.getRefundEligibleProviders.mockResolvedValue({ data: { provider_instance_ids: eligibleProviders } })
  const wrapper = mount(UserOrdersView, {
    global: { stubs: {
      AppLayout: { template: '<div><slot /></div>' },
      OrderTable: {
        props: ['orders'],
        template: '<div><slot v-if="orders[0]" name="actions" :row="orders[0]" /></div>',
      },
      BaseDialog: true, Icon: true, Pagination: true, teleport: true
    } }
  })
  await flushPromises()
  return wrapper
}

describe('refund eligibility', () => {
  it('offers a refund for a completed balance order from a refund-eligible provider', async () => {
    const wrapper = await mountWithOrder(orderFactory())
    expect(wrapper.text()).toContain('payment.orders.requestRefund')
  })

  it('hides the refund action for completed subscription orders the backend rejects', async () => {
    const wrapper = await mountWithOrder(orderFactory({ order_type: 'subscription' }))
    expect(wrapper.text()).not.toContain('payment.orders.requestRefund')
  })

  it('hides the refund action for a provider that does not allow user refunds', async () => {
    const wrapper = await mountWithOrder(orderFactory(), [])
    expect(wrapper.text()).not.toContain('payment.orders.requestRefund')
  })
})
