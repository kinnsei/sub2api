import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, shallowMount } from '@vue/test-utils'

const routeState = vi.hoisted(() => ({
  query: {} as Record<string, unknown>,
}))
const routerPush = vi.hoisted(() => vi.fn())
const getOrder = vi.hoisted(() => vi.fn())
const paymentStore = vi.hoisted(() => ({
  config: { stripe_publishable_key: 'pk_test' } as { stripe_publishable_key?: string },
  fetchConfig: vi.fn(),
  pollOrderStatus: vi.fn(),
}))
const loadStripe = vi.hoisted(() => vi.fn())
const stripeElements = vi.hoisted(() => ({
  create: vi.fn(),
}))
const stripePaymentElement = vi.hoisted(() => ({
  mount: vi.fn(),
  on: vi.fn(),
}))
const stripeInstance = vi.hoisted(() => ({
  elements: vi.fn(),
  confirmPayment: vi.fn(),
  confirmAlipayPayment: vi.fn(),
  confirmWechatPayPayment: vi.fn(),
}))

vi.mock('vue-router', async () => {
  const actual = await vi.importActual<typeof import('vue-router')>('vue-router')
  return {
    ...actual,
    useRoute: () => routeState,
    useRouter: () => ({ push: routerPush }),
  }
})

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
      locale: { value: 'zh-CN' },
    }),
  }
})

vi.mock('@/stores/payment', () => ({
  usePaymentStore: () => paymentStore,
}))

vi.mock('@/api/payment', () => ({
  paymentAPI: {
    getOrder,
  },
}))

vi.mock('@stripe/stripe-js/pure', () => ({
  loadStripe,
}))

import StripePaymentView from '../StripePaymentView.vue'
import { formatPaymentAmount } from '@/components/payment/currency'
import type { PaymentOrder } from '@/types/payment'

function orderFactory(overrides: Partial<PaymentOrder> = {}): PaymentOrder {
  return {
    id: 42,
    user_id: 7,
    amount: 100,
    pay_amount: 103,
    currency: 'CNY',
    fee_rate: 0.03,
    payment_type: 'stripe',
    out_trade_no: 'sub2_stripe_42',
    status: 'PENDING',
    order_type: 'balance',
    created_at: '2026-04-20T12:00:00Z',
    expires_at: '2026-04-20T12:30:00Z',
    refund_amount: 0,
    ...overrides,
  }
}

function mountView() {
  return shallowMount(StripePaymentView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        Icon: true,
      },
    },
  })
}

function resetViewMocks() {
  routerPush.mockReset()
  getOrder.mockReset()
  paymentStore.config = { stripe_publishable_key: 'pk_test' }
  paymentStore.fetchConfig.mockReset().mockResolvedValue(undefined)
  paymentStore.pollOrderStatus.mockReset()
  loadStripe.mockReset().mockResolvedValue(stripeInstance)
  stripeElements.create.mockReset().mockReturnValue(stripePaymentElement)
  stripePaymentElement.mount.mockReset()
  stripePaymentElement.on.mockReset().mockImplementation((event: string, callback: () => void) => {
    if (event === 'ready') callback()
  })
  stripeInstance.elements.mockReset().mockReturnValue(stripeElements)
  stripeInstance.confirmPayment.mockReset()
  stripeInstance.confirmAlipayPayment.mockReset()
  stripeInstance.confirmWechatPayPayment.mockReset()
  window.localStorage.clear()
}

describe('StripePaymentView', () => {
  beforeEach(() => {
    routeState.query = {
      order_id: '42',
      client_secret: 'pi_secret_42',
    }
    resetViewMocks()
  })

  it('本地恢复快照缺失时使用订单接口返回的 Stripe 币种展示金额', async () => {
    getOrder.mockResolvedValue({
      data: orderFactory({ currency: 'HKD', pay_amount: 103 }),
    })

    const wrapper = mountView()
    await flushPromises()
    await flushPromises()

    expect(getOrder).toHaveBeenCalledWith(42)
    expect(loadStripe).toHaveBeenCalledWith('pk_test')
    expect(wrapper.text()).toContain(formatPaymentAmount(103, 'HKD', 'zh-CN'))
  })
})

describe('StripePaymentView WeChat QR polling', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    routeState.query = {
      order_id: '42',
      client_secret: 'pi_secret_42',
      method: 'wechat_pay',
    }
    resetViewMocks()
    getOrder.mockResolvedValue({ data: orderFactory({ expires_at: '2099-01-01T00:00:00Z' }) })
    stripeInstance.confirmWechatPayPayment.mockResolvedValue({
      paymentIntent: {
        status: 'requires_action',
        next_action: {
          wechat_pay_display_qr_code: { image_data_url: 'data:image/png;base64,wechat-qr' },
        },
      },
    })
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  async function mountWechatView() {
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(0)
    await flushPromises()
    return wrapper
  }

  it('stops polling and shows the success state once the order completes', async () => {
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status: 'COMPLETED' }))

    const wrapper = await mountWechatView()
    expect(wrapper.text()).toContain('payment.qr.scanWxpay')

    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(paymentStore.pollOrderStatus).toHaveBeenCalledWith(42)
    expect(wrapper.text()).toContain('payment.result.success')

    await vi.advanceTimersByTimeAsync(9000)
    await flushPromises()
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledTimes(1)

    wrapper.unmount()
  })

  it('treats RECHARGING as a successful terminal state', async () => {
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status: 'RECHARGING' }))

    const wrapper = await mountWechatView()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(wrapper.text()).toContain('payment.result.success')
    expect(wrapper.text()).not.toContain('payment.qr.scanWxpay')

    wrapper.unmount()
  })

  it.each([
    ['CANCELLED', 'payment.qr.cancelled'],
    ['EXPIRED', 'payment.qr.expired'],
    ['FAILED', 'payment.result.failed'],
  ])('stops polling and surfaces the failure state for %s', async (status, expectedCopy) => {
    paymentStore.pollOrderStatus.mockResolvedValue(orderFactory({ status }))

    const wrapper = await mountWechatView()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(wrapper.text()).toContain(expectedCopy)
    expect(wrapper.text()).not.toContain('payment.qr.scanWxpay')
    expect(wrapper.text()).toContain('payment.result.backToRecharge')

    await vi.advanceTimersByTimeAsync(9000)
    await flushPromises()
    expect(paymentStore.pollOrderStatus).toHaveBeenCalledTimes(1)

    wrapper.unmount()
  })

  it('stops polling once the order expiry has passed', async () => {
    getOrder.mockResolvedValue({ data: orderFactory({ expires_at: '2000-01-01T00:00:00Z' }) })

    const wrapper = await mountWechatView()
    await vi.advanceTimersByTimeAsync(3000)
    await flushPromises()

    expect(paymentStore.pollOrderStatus).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('payment.qr.expired')

    wrapper.unmount()
  })

  it('falls back to a 30-minute expiry when the order has no expires_at', async () => {
    getOrder.mockResolvedValue({ data: orderFactory({ expires_at: '' }) })

    const wrapper = await mountWechatView()
    await vi.advanceTimersByTimeAsync(30 * 60 * 1000)
    await flushPromises()

    expect(paymentStore.pollOrderStatus.mock.calls.length).toBeGreaterThan(0)
    expect(wrapper.text()).toContain('payment.qr.expired')

    const callsAtExpiry = paymentStore.pollOrderStatus.mock.calls.length
    await vi.advanceTimersByTimeAsync(30 * 60 * 1000)
    await flushPromises()
    expect(paymentStore.pollOrderStatus.mock.calls.length).toBe(callsAtExpiry)

    wrapper.unmount()
  })
})
