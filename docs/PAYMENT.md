# Payment System Configuration Guide

Sub2API has a built-in payment system that enables user self-service top-up without deploying a separate payment service.

---

## Table of Contents

- [Supported Payment Methods](#supported-payment-methods)
- [Quick Start](#quick-start)
- [System Settings](#system-settings)
- [Provider Configuration](#provider-configuration)
- [Provider Instance Management](#provider-instance-management)
- [Webhook Configuration](#webhook-configuration)
- [Payment Flow](#payment-flow)
- [Migrating from Sub2ApiPay](#migrating-from-sub2apipay)

---

## Supported Payment Methods

| Provider | Payment Methods | Description |
|----------|----------------|-------------|
| **EasyPay** | Alipay, WeChat Pay | Third-party aggregation via EasyPay protocol |
| **Alipay (Direct)** | Desktop QR code, mobile Alipay redirect | Direct integration with Alipay Open Platform, returning desktop QR codes and mobile WAP/app launch links |
| **WeChat Pay (Direct)** | Native QR, H5, MP/JSAPI Pay | Direct integration with WeChat Pay APIv3 with environment-aware routing |
| **Stripe** | Card, Alipay, WeChat Pay, Link, etc. | International payments, multi-currency support |
| **Airwallex** | Card, Alipay, WeChat Pay, etc. | Hosted checkout through the Airwallex Components SDK, multi-currency support |

> Alipay/WeChat Pay direct and EasyPay can both exist as backend provider instances, but the frontend always exposes only two visible buttons: `Alipay` and `WeChat Pay`. Admins choose exactly one source for each visible method: direct or EasyPay. Direct channels connect to payment APIs directly with lower fees; EasyPay aggregates through third-party platforms with easier setup.

> **EasyPay Provider Recommendations**: Both options below are third-party aggregators compatible with the EasyPay protocol. Pick based on the funding channel and settlement currency you need:
>
> - **Domestic channel / CNY settlement** — [ZPay](https://z-pay.cn/?uid=23808) (`https://z-pay.cn/?uid=23808`): direct integration with official Alipay / WeChat Pay APIs, fee **1.6%**; funds go straight to the merchant account with **T+1 automatic settlement**. Supports **individual users** (no business license required) with up to 10,000 CNY daily transactions; business-licensed accounts have no limit. Link contains the referral code of [Sub2ApiPay](https://github.com/touwaeriol/sub2apipay) original author [@touwaeriol](https://github.com/touwaeriol) — feel free to remove it.
> - **International channel / USDT or USD settlement** — [Kyren Topup](https://kyrenpay.com/?code=SUB2API) (`https://kyrenpay.com/?code=SUB2API`): a ready-to-launch global payment stack for AI startups with WeChat Pay and Alipay support, local-currency checkout, and USD settlement. Fees: WeChat 2.5%, Alipay 2.5%; Multiple withdrawal methods are available. Withdrawals to overseas company accounts incur a $20 fee, while USDT withdrawals incur a $30 fee plus a 0.4% transaction fee, settled in **USDT or USD**. No qualification review required — sign up and use immediately, making it the lowest barrier to entry. Withdrawal threshold is relatively high, recommended for users **who do not use domestic Chinese payment channels, cannot tolerate Stripe's 6%+ fees, have high transaction volume, and have USD or USDT channels to receive withdrawn funds**. Kyren Topup charges a $200 account opening fee; signing up via this link (which contains Sub2Api author [@Wei-Shaw](https://github.com/Wei-Shaw)'s referral code) **waives the opening fee**. Feel free to remove it if you prefer.
>
> Please evaluate the security, reliability, and compliance of any third-party payment provider on your own — this project does not endorse or guarantee any of them.

---

## Quick Start

1. Go to Admin Dashboard → **Settings** → **Payment Settings** tab
2. Enable **Payment**
3. Configure basic parameters (amount range, timeout, etc.)
4. Add at least one provider instance in **Provider Management**
5. Users can now top up from the frontend

---

## System Settings

Configure the following in Admin Dashboard **Settings → Payment Settings**:

### Basic Settings

| Setting | Description | Default |
|---------|-------------|---------|
| **Enable Payment** | Enable or disable the payment system | Off |
| **Product Name Prefix** | Prefix shown on payment page | - |
| **Product Name Suffix** | Suffix (e.g., "Credits") | - |
| **Minimum Amount** | Minimum single top-up amount | 1 |
| **Maximum Amount** | Maximum single top-up amount (empty = unlimited) | - |
| **Daily Limit** | Per-user daily cumulative limit (empty = unlimited) | - |
| **Order Timeout** | Order timeout in minutes (minimum 1) | 30 |
| **Max Pending Orders** | Maximum concurrent pending orders per user | 3 |
| **Load Balance Strategy** | Strategy for selecting provider instances | Round Robin |
| **Balance Recharge Multiplier** | Credits granted per unit paid (1 = 1:1) | 1 |
| **Balance Payment Disabled** | Hide the balance top-up option entirely | Off |
| **Subscription USD to CNY Rate** | Explicit opt-in conversion for plan prices charged in CNY (0 = charge the plan price as-is) | 0 |
| **Recharge Fee Rate** | Percentage fee added on top of every order (0-100) | 0 |
| **Alipay Force QR Code** | Always use QR/redirect instead of the in-app launch for Alipay | Off |
| **Alipay Mobile Precreate Deep Link** | Mobile Alipay orders use `alipay.trade.precreate` plus an app launch attempt (requires Face-to-Face payment to be enabled) | Off |

### Frontend Visible Method Routing

The current payment UX keeps the frontend method list unified and does not expose provider brands directly:

- **Alipay**: when enabled, this button must be routed to either `Alipay (Direct)` or `EasyPay Alipay`
- **WeChat Pay**: when enabled, this button must be routed to either `WeChat Pay (Direct)` or `EasyPay WeChat`
- Each visible method can route to only one source at a time
- If a visible method is enabled without a selected source, the frontend will not expose that method

The source and enable flag are stored in the `payment_visible_method_alipay_source` / `payment_visible_method_wxpay_source` settings (`official_alipay`, `easypay_alipay`, `official_wxpay`, `easypay_wxpay`) together with the matching `payment_visible_method_*_enabled` flag. Set them through the admin settings API (`PUT /api/v1/admin/settings`); the payment tab does not expose a dedicated control for them yet.

### Load Balance Strategies

| Strategy | Description |
|----------|-------------|
| **Round Robin** | Distribute orders to instances in rotation |
| **Least Amount** | Prefer instances with the lowest daily cumulative amount |

### Cancel Rate Limiting

Prevents users from repeatedly creating and canceling orders:

| Setting | Description |
|---------|-------------|
| **Enable Limit** | Toggle |
| **Window Mode** | Sliding / Fixed window |
| **Time Window** | Window duration |
| **Window Unit** | Minutes / Hours |
| **Max Cancels** | Maximum cancellations allowed within the window |

### Help Information

| Setting | Description |
|---------|-------------|
| **Help Image** | Customer service QR code or help image (supports upload) |
| **Help Text** | Instructions displayed on the payment page |

---

## Provider Configuration

Each provider type requires different credentials. Select the type when adding a new provider instance in **Provider Management → Add Provider**.

> **Callback URLs are auto-generated**: When adding a provider, the Notify URL and Return URL are automatically constructed from your site domain. You only need to confirm the domain is correct.

### EasyPay

Compatible with any payment service that implements the EasyPay protocol.

> The EasyPay protocol only exposes payment creation, status query (`act=order`) and refund (`act=refund`). It has **no cancel endpoint and no refund-status query**, so cancelling an EasyPay order only closes it locally and a `REFUND_PENDING` EasyPay refund must be confirmed in the aggregator's own dashboard.

| Parameter | Description | Required |
|-----------|-------------|----------|
| **Merchant ID (PID)** | EasyPay merchant ID | Yes |
| **Merchant Key (PKey)** | EasyPay merchant secret key | Yes |
| **API Base URL** | EasyPay API base address | Yes |
| **Alipay Channel ID** | Specify Alipay channel (optional) | No |
| **WeChat Channel ID** | Specify WeChat channel (optional) | No |

### Alipay (Direct)

Direct integration with Alipay Open Platform. Mobile flows return an Alipay WAP/app redirect URL. Desktop flows prefer Face-to-Face Precreate QR payloads; if the merchant has not enabled that product, the provider falls back to Computer Website Pay and also returns the cashier URL so the frontend can render a QR code or open the hosted checkout page directly.

| Parameter | Description | Required |
|-----------|-------------|----------|
| **AppID** | Alipay application AppID | Yes |
| **Private Key** | RSA2 application private key | Yes |
| **Alipay Public Key** | Alipay public key | Yes |

### WeChat Pay (Direct)

Direct integration with WeChat Pay APIv3. Supports Native QR code payment, H5 payment, and MP/JSAPI payment inside the WeChat environment.

| Parameter | Description | Required |
|-----------|-------------|----------|
| **AppID** | WeChat Pay AppID | Yes |
| **Merchant ID (MchID)** | WeChat Pay merchant ID | Yes |
| **Merchant API Private Key** | Merchant API private key (PEM format) | Yes |
| **APIv3 Key** | 32-byte APIv3 key | Yes |
| **WeChat Pay Public Key** | WeChat Pay public key (PEM format) | Yes |
| **WeChat Pay Public Key ID** | WeChat Pay public key ID | Yes |
| **Certificate Serial Number** | Merchant certificate serial number | Yes |

### Stripe

International payment platform supporting multiple payment methods and currencies.

| Parameter | Description | Required |
|-----------|-------------|----------|
| **Secret Key** | Stripe secret key (`sk_live_...` or `sk_test_...`) | Yes |
| **Publishable Key** | Stripe publishable key (`pk_live_...` or `pk_test_...`) | No - required only for the embedded Stripe Payment Element |
| **Webhook Secret** | Stripe Webhook signing secret (`whsec_...`) | Yes - webhook signature verification fails without it |

### Airwallex

Hosted checkout through the Airwallex Components SDK. Orders return a payment intent id plus a client secret; the frontend redirects to the Airwallex checkout page and the payment is confirmed by webhook.

| Parameter | Description | Required |
|-----------|-------------|----------|
| **Client ID** | Airwallex API client id | Yes |
| **API Key** | Airwallex API key | Yes |
| **Webhook Secret** | Airwallex webhook signing secret (HMAC-SHA256) | Yes |
| **API Base URL** | Airwallex API base address | Yes |
| **Account ID** | Airwallex account id, echoed in provider snapshot checks | No |
| **Currency / Country Code** | Checkout currency (default CNY) and country code (default CN) | No |

---

## Provider Instance Management

You can create **multiple instances** of the same provider type for load balancing and risk control:

- **Multi-instance load balancing** — Distribute orders via round-robin or least-amount strategy
- **Independent limits** — Each instance can have its own min/max amount and daily limit
- **Independent toggle** — Enable/disable individual instances without affecting others
- **Refund control** — Enable or disable refunds per instance
- **Payment methods** — Each instance can support a subset of payment methods
- **Ordering** — Drag to reorder instances

### Instance Limit Configuration

Each instance supports these limits:

| Limit | Description |
|-------|-------------|
| **Minimum Amount** | Minimum order amount accepted by this instance |
| **Maximum Amount** | Maximum order amount accepted by this instance |
| **Daily Limit** | Daily cumulative transaction limit for this instance |

> During load balancing, instances that exceed their limits are automatically skipped.

---

## Webhook Configuration

Payment callbacks are essential for the payment system to work correctly.

### Callback URL Format

When adding a provider, the system auto-generates callback URLs from your site domain:

| Provider | Callback Path |
|----------|-------------|
| **EasyPay** | `https://your-domain.com/api/v1/payment/webhook/easypay` |
| **Alipay (Direct)** | `https://your-domain.com/api/v1/payment/webhook/alipay` |
| **WeChat Pay (Direct)** | `https://your-domain.com/api/v1/payment/webhook/wxpay` |
| **Stripe** | `https://your-domain.com/api/v1/payment/webhook/stripe` |
| **Airwallex** | `https://your-domain.com/api/v1/payment/webhook/airwallex` |

> Replace `your-domain.com` with your actual domain. For EasyPay / Alipay / WeChat Pay, the callback URL is auto-filled when adding the provider — no manual configuration needed.

### Stripe Webhook Setup

1. Log in to [Stripe Dashboard](https://dashboard.stripe.com/)
2. Go to **Developers → Webhooks**
3. Add an endpoint with the callback URL
4. Subscribe to events: `payment_intent.succeeded`, `payment_intent.payment_failed`
5. Copy the generated Webhook Secret (`whsec_...`) to your provider configuration

### Airwallex Webhook Setup

1. Log in to the Airwallex web app
2. Go to **Developer → Webhooks**
3. Add an endpoint with the callback URL
4. Subscribe to `payment_intent.succeeded` and `payment_intent.cancelled`
5. Copy the signing secret into the provider's **Webhook Secret** field

### Important Notes

- Callback URLs must use **HTTPS** (required by Stripe, strongly recommended for others)
- Ensure your firewall allows callback requests from payment platforms
- The system automatically verifies callback signatures to prevent forgery
- Balance top-up is processed automatically upon successful payment — no manual intervention needed

---

## Payment Flow

```
User selects amount and payment method
       │
       ▼
  Create Order (PENDING)
  ├─ Validate amount range, pending order count, daily limit
  ├─ Load balance to select provider instance
  └─ Call provider to get payment info
       │
       ▼
  User completes payment
  ├─ EasyPay     → QR code / H5 redirect
  ├─ Alipay      → Desktop QR payload (Face-to-Face preferred, Website Pay fallback) / mobile Alipay redirect
  ├─ WeChat Pay  → Desktop Native QR / non-WeChat H5 / in-WeChat JSAPI
  └─ Stripe      → Payment Element (card/Alipay/WeChat/etc.)
       │
       ▼
  Webhook callback verified → Order PAID
       │
       ▼
  Auto top-up to user balance → Order COMPLETED
```

### Order Status Reference

| Status | Description |
|--------|-------------|
| `PENDING` | Waiting for user to complete payment |
| `PAID` | Payment confirmed, awaiting balance credit |
| `COMPLETED` | Balance credited successfully |
| `EXPIRED` | Timed out without payment |
| `CANCELLED` | Cancelled by user |
| `FAILED` | Balance credit failed, admin can retry |
| `REFUND_REQUESTED` | Refund requested by the user, awaiting admin review |
| `REFUNDING` | Refund in progress |
| `REFUND_PENDING` | Gateway accepted the refund but has not settled it yet; an admin can query the status |
| `PARTIALLY_REFUNDED` | Part of the order was refunded; the remainder can still be refunded |
| `REFUNDED` | Fully refunded (terminal) |
| `REFUND_FAILED` | Refund failed at the gateway or was rolled back; retry from the order list |

### Timeout and Fallback

- Before marking an order as expired, the background job queries the upstream payment status first
- If that query fails, the order is left pending and retried on the next cycle: an unverified status never cancels or expires an order
- If the user has actually paid but the callback was delayed, the system reconciles automatically (Alipay, WeChat Pay and EasyPay pending orders are all re-queried)
- A provider-confirmed payment is always credited, even when the notification arrives after the order expired; late recoveries are recorded as `ORDER_RECOVERED` in the order audit log
- Orders stuck in `PAID` / `RECHARGING` / `FAILED` (for example after a restart) are retried automatically until the failure budget is exhausted, after which they stay `FAILED` for manual review
- The background job runs every 60 seconds

---

## Refunds

Refunds are driven from the admin order list, with an optional user-initiated request:

1. A user can request a refund for a completed **balance** order when the provider instance has **Allow user refund** enabled and their balance covers the amount. The order moves to `REFUND_REQUESTED`; no gateway call happens yet.
2. An admin reviews the order and runs the refund (balance or subscription deduction, with a force option when the deduction cannot be planned automatically).
3. The gateway call result decides the outcome: `REFUNDED` / `PARTIALLY_REFUNDED`, `REFUND_PENDING` (the admin can query the status later), or `REFUND_FAILED` (the deduction is rolled back and the order returns to its previous status).

Notes:

- **Partial refunds can be repeated.** The refundable amount is the order amount minus everything already refunded, so a second refund for the remainder is allowed and only then moves the order to `REFUNDED`.
- **Providers that refund by upstream trade number (Stripe, Airwallex) are refused when the order has no recorded trade number.** The attempt is logged as `REFUND_NO_TRADE_NO` and no local-only "successful" refund is recorded; providers that refund by merchant order id (Alipay, WeChat Pay, EasyPay) are unaffected.
- Refunds can be disabled per provider instance (**Refund control**), and legacy orders without a pinned provider instance cannot be refunded.
- **EasyPay has no refund-status query**, so a pending EasyPay refund cannot be finalized from the admin list (the API returns `REFUND_QUERY_UNSUPPORTED`); confirm it in the aggregator's dashboard. Alipay, WeChat Pay, Stripe and Airwallex all support status queries.

---

## Migrating from Sub2ApiPay

If you previously used [Sub2ApiPay](https://github.com/touwaeriol/sub2apipay) as an external payment system, you can migrate to the built-in payment system:

### Key Differences

| Aspect | Sub2ApiPay | Built-in Payment |
|--------|-----------|-----------------|
| Deployment | Separate service (Next.js + PostgreSQL) | Built into Sub2API, no extra deployment |
| Payment Methods | EasyPay, Alipay, WeChat, Stripe | Same |
| Configuration | Environment variables + separate admin UI | Unified in Sub2API admin dashboard |
| Top-up Integration | Via Admin API callback | Internal processing, more reliable |
| Subscription Plans | Supported | Supported (admin -> Orders -> Payment Plans) |
| Order Management | Separate admin interface | Integrated in Sub2API admin dashboard |

### Migration Steps

1. Enable payment in Sub2API admin dashboard and configure providers (use the same payment credentials)
2. Update webhook callback URLs to Sub2API's callback endpoints
3. Verify that new orders are processed correctly via built-in payment
4. Decommission the Sub2ApiPay service

> **Note**: Historical order data from Sub2ApiPay will not be automatically migrated. Keep Sub2ApiPay running for a while to access historical records.
