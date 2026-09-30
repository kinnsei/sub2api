package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/paymentproviderinstance"
	"github.com/Wei-Shaw/sub2api/internal/payment"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// GetAvailableMethodLimits collects all payment types from enabled provider
// instances and returns limits for each, plus the global widest range.
// Stripe sub-types (card, link) are aggregated under "stripe".
func (s *PaymentConfigService) GetAvailableMethodLimits(ctx context.Context) (*MethodLimitsResponse, error) {
	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(paymentproviderinstance.EnabledEQ(true)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query provider instances: %w", err)
	}
	typeInstances := pcGroupByPaymentType(instances)
	typeInstances = s.pcApplyEnabledVisibleMethodInstances(ctx, typeInstances, instances)
	resp := &MethodLimitsResponse{
		Methods: make(map[string]MethodLimits, len(typeInstances)),
	}
	feeRate := s.pcMethodFeeRate(ctx)
	for pt, insts := range typeInstances {
		insts = pcReadableLimitsInstances(insts, pt)
		if len(insts) == 0 {
			// Every instance for this method has unreadable limits, and the load
			// balancer will refuse to route to them. Advertising the method with
			// an unlimited range would promise an order that cannot be created.
			continue
		}
		currency, ok := s.pcAggregateMethodCurrency(insts)
		if !ok {
			continue
		}
		ml := pcAggregateMethodLimits(pt, insts)
		ml.DisplayName = s.pcAggregateMethodDisplayName(pt, insts)
		ml.Currency = currency
		ml.FeeRate = feeRate
		resp.Methods[ml.PaymentType] = ml
	}
	resp.GlobalMin, resp.GlobalMax = pcComputeGlobalRange(resp.Methods)
	return resp, nil
}

// pcReadableLimitsInstances drops instances whose stored limits JSON cannot be
// parsed. Such instances are excluded from load-balancer selection, so they must
// not influence the advertised limits either.
func pcReadableLimitsInstances(instances []*dbent.PaymentProviderInstance, pt string) []*dbent.PaymentProviderInstance {
	readable := make([]*dbent.PaymentProviderInstance, 0, len(instances))
	for _, inst := range instances {
		if _, _, ok := pcInstanceTypeLimits(inst, pt); ok {
			readable = append(readable, inst)
		}
	}
	return readable
}

// pcMethodFeeRate returns the fee rate reported with per-method limits. Fees are
// configured globally (RECHARGE_FEE_RATE) and applied to every order at creation,
// so each method reports the same rate; a config lookup failure reports 0 so the
// UI hides the fee hint instead of showing a stale or wrong value.
func (s *PaymentConfigService) pcMethodFeeRate(ctx context.Context) float64 {
	if s == nil || s.settingRepo == nil {
		return 0
	}
	cfg, err := s.GetPaymentConfig(ctx)
	if err != nil || cfg == nil {
		return 0
	}
	return cfg.RechargeFeeRate
}

func (s *PaymentConfigService) pcApplyEnabledVisibleMethodInstances(ctx context.Context, typeInstances map[string][]*dbent.PaymentProviderInstance, instances []*dbent.PaymentProviderInstance) map[string][]*dbent.PaymentProviderInstance {
	if len(typeInstances) == 0 {
		return typeInstances
	}

	filtered := make(map[string][]*dbent.PaymentProviderInstance, len(typeInstances))
	for paymentType, groupedInstances := range typeInstances {
		filtered[paymentType] = groupedInstances
	}

	for _, method := range []string{payment.TypeAlipay, payment.TypeWxpay} {
		matching := filterEnabledVisibleMethodInstances(instances, method)
		providerKey, err := s.resolveVisibleMethodProviderKey(ctx, method, matching)
		if err != nil {
			delete(filtered, method)
			continue
		}
		if providerKey == "" {
			if len(matching) == 0 {
				delete(filtered, method)
				continue
			}
			filtered[method] = matching
			continue
		}
		selectedInstances := filterVisibleMethodInstancesByProviderKey(instances, method, providerKey)
		if len(selectedInstances) == 0 {
			delete(filtered, method)
			continue
		}
		filtered[method] = selectedInstances
	}
	return filtered
}

// GetMethodLimits returns per-payment-type limits from enabled provider instances.
func (s *PaymentConfigService) GetMethodLimits(ctx context.Context, types []string) ([]MethodLimits, error) {
	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(paymentproviderinstance.EnabledEQ(true)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("query provider instances: %w", err)
	}
	result := make([]MethodLimits, 0, len(types))
	feeRate := s.pcMethodFeeRate(ctx)
	for _, pt := range types {
		var matching []*dbent.PaymentProviderInstance
		for _, inst := range instances {
			if payment.InstanceSupportsType(inst.SupportedTypes, pt) {
				matching = append(matching, inst)
			}
		}
		matching = pcReadableLimitsInstances(matching, pt)
		if len(matching) == 0 {
			// No instance can serve this method (none configured, or all have
			// unreadable limits). Reporting a zero-range "available" method here
			// would contradict the load balancer, which refuses those instances.
			continue
		}
		currency, ok := s.pcAggregateMethodCurrency(matching)
		if !ok {
			continue
		}
		ml := pcAggregateMethodLimits(pt, matching)
		ml.DisplayName = s.pcAggregateMethodDisplayName(pt, matching)
		ml.Currency = currency
		ml.FeeRate = feeRate
		result = append(result, ml)
	}
	return result, nil
}

func (s *PaymentConfigService) ValidateMethodCurrencyConsistency(ctx context.Context, paymentType string) (string, error) {
	method := NormalizeVisibleMethod(paymentType)
	if method == "" || s == nil || s.entClient == nil {
		return payment.DefaultPaymentCurrency, nil
	}

	instances, err := s.entClient.PaymentProviderInstance.Query().
		Where(paymentproviderinstance.EnabledEQ(true)).All(ctx)
	if err != nil {
		return "", fmt.Errorf("query provider instances: %w", err)
	}

	typeInstances := pcGroupByPaymentType(instances)
	typeInstances = s.pcApplyEnabledVisibleMethodInstances(ctx, typeInstances, instances)
	matching := typeInstances[method]
	if len(matching) == 0 {
		return payment.DefaultPaymentCurrency, nil
	}

	currency, ok := s.pcAggregateMethodCurrency(matching)
	if !ok {
		return "", infraerrors.ServiceUnavailable(
			"PAYMENT_METHOD_CURRENCY_CONFLICT",
			"payment method has enabled provider instances with mixed currencies",
		).WithMetadata(map[string]string{"payment_type": method})
	}
	return currency, nil
}

func (s *PaymentConfigService) pcAggregateMethodCurrency(instances []*dbent.PaymentProviderInstance) (string, bool) {
	currency := ""
	for _, inst := range instances {
		next := s.pcInstancePaymentCurrency(inst)
		if next == "" {
			continue
		}
		if currency == "" {
			currency = next
			continue
		}
		if currency != next {
			return "", false
		}
	}
	if currency == "" {
		return payment.DefaultPaymentCurrency, true
	}
	return currency, true
}

func (s *PaymentConfigService) pcInstancePaymentCurrency(inst *dbent.PaymentProviderInstance) string {
	if inst == nil {
		return payment.DefaultPaymentCurrency
	}
	cfg := map[string]string{}
	if s != nil {
		decrypted, err := s.decryptConfig(inst.Config)
		if err == nil && decrypted != nil {
			cfg = decrypted
		}
	}
	return paymentProviderConfigCurrency(inst.ProviderKey, cfg)
}

type easyPayCustomMethodDisplayConfig struct {
	Type        string `json:"type"`
	DisplayName string `json:"displayName"`
}

func (s *PaymentConfigService) pcAggregateMethodDisplayName(pt string, instances []*dbent.PaymentProviderInstance) string {
	pt = strings.TrimSpace(pt)
	if pt == "" {
		return ""
	}
	for _, inst := range instances {
		displayName := s.pcInstanceEasyPayCustomMethodDisplayName(inst, pt)
		if displayName != "" {
			return displayName
		}
	}
	return ""
}

func (s *PaymentConfigService) pcInstanceEasyPayCustomMethodDisplayName(inst *dbent.PaymentProviderInstance, pt string) string {
	if inst == nil || inst.ProviderKey != payment.TypeEasyPay {
		return ""
	}
	cfg := map[string]string{}
	if s != nil {
		decrypted, err := s.decryptConfig(inst.Config)
		if err == nil && decrypted != nil {
			cfg = decrypted
		}
	}
	raw := strings.TrimSpace(cfg["customMethods"])
	if raw == "" {
		return ""
	}

	var methods []easyPayCustomMethodDisplayConfig
	if err := json.Unmarshal([]byte(raw), &methods); err != nil {
		return ""
	}
	for _, method := range methods {
		if strings.TrimSpace(method.Type) == pt {
			return strings.TrimSpace(method.DisplayName)
		}
	}
	return ""
}

// pcGroupByPaymentType groups instances by user-facing payment type.
// For Stripe providers, ALL sub-types (card, link, alipay, wxpay) map to "stripe"
// because the user sees a single "Stripe" button, not individual sub-methods.
// Uses a seen set to avoid counting one instance twice.
func pcGroupByPaymentType(instances []*dbent.PaymentProviderInstance) map[string][]*dbent.PaymentProviderInstance {
	typeInstances := make(map[string][]*dbent.PaymentProviderInstance)
	seen := make(map[string]map[int64]bool)
	add := func(key string, inst *dbent.PaymentProviderInstance) {
		if seen[key] == nil {
			seen[key] = make(map[int64]bool)
		}
		if !seen[key][int64(inst.ID)] {
			seen[key][int64(inst.ID)] = true
			typeInstances[key] = append(typeInstances[key], inst)
		}
	}
	for _, inst := range instances {
		// Stripe provider: all sub-types → single "stripe" group
		if inst.ProviderKey == payment.TypeStripe {
			add(payment.TypeStripe, inst)
			continue
		}
		for _, t := range splitTypes(inst.SupportedTypes) {
			add(t, inst)
		}
	}
	return typeInstances
}

// pcInstanceTypeLimits extracts per-type limits from a provider instance.
// Returns (limits, configured, readable).
//   - configured=false, readable=true: no cap for this type → unlimited.
//   - readable=false: the stored JSON cannot be parsed. The load balancer
//     excludes such instances from selection, so callers that advertise or
//     aggregate limits must not count them as unlimited either.
//
// For Stripe instances, limits are stored under "stripe" key regardless of sub-types.
func pcInstanceTypeLimits(inst *dbent.PaymentProviderInstance, pt string) (payment.ChannelLimits, bool, bool) {
	if inst == nil || inst.Limits == "" {
		return payment.ChannelLimits{}, false, true
	}
	var limits payment.InstanceLimits
	if err := json.Unmarshal([]byte(inst.Limits), &limits); err != nil {
		slog.Error("payment provider instance limits unreadable, ignoring instance when aggregating limits",
			"instance_id", inst.ID,
			"provider_key", inst.ProviderKey,
			"payment_type", pt,
			"error", err)
		return payment.ChannelLimits{}, false, false
	}
	cl, ok := limits[pt]
	return cl, ok, true
}

// unionFloat merges a single limit value into the aggregate using UNION semantics.
//   - For "min" fields (wantMin=true): keeps the lowest non-zero value
//   - For "max"/"cap" fields (wantMin=false): keeps the highest non-zero value
//   - If any value is 0 (unlimited), the result is unlimited.
//
// Returns (aggregated value, still limited).
func unionFloat(agg float64, limited bool, val float64, wantMin bool) (float64, bool) {
	if val == 0 {
		return agg, false
	}
	if !limited {
		return agg, false
	}
	if agg == 0 {
		return val, true
	}
	if wantMin && val < agg {
		return val, true
	}
	if !wantMin && val > agg {
		return val, true
	}
	return agg, true
}

// pcAggregateMethodLimits computes the UNION (least restrictive) of limits
// across all provider instances for a given payment type.
//
// Since the load balancer can route an order to any available instance,
// the user should see the widest possible range:
//   - SingleMin: lowest floor across instances; 0 if any is unlimited
//   - SingleMax: highest ceiling across instances; 0 if any is unlimited
//   - DailyLimit: highest cap across instances; 0 if any is unlimited
func pcAggregateMethodLimits(pt string, instances []*dbent.PaymentProviderInstance) MethodLimits {
	ml := MethodLimits{PaymentType: pt}
	minLimited, maxLimited, dailyLimited := true, true, true

	for _, inst := range instances {
		cl, hasLimits, readable := pcInstanceTypeLimits(inst, pt)
		if !readable {
			// The load balancer refuses to route to an instance whose limits are
			// unreadable, so it cannot be the source of an unlimited channel.
			continue
		}
		if !hasLimits {
			return MethodLimits{PaymentType: pt} // any unlimited instance → all zeros
		}
		ml.SingleMin, minLimited = unionFloat(ml.SingleMin, minLimited, cl.SingleMin, true)
		ml.SingleMax, maxLimited = unionFloat(ml.SingleMax, maxLimited, cl.SingleMax, false)
		ml.DailyLimit, dailyLimited = unionFloat(ml.DailyLimit, dailyLimited, cl.DailyLimit, false)
	}

	if !minLimited {
		ml.SingleMin = 0
	}
	if !maxLimited {
		ml.SingleMax = 0
	}
	if !dailyLimited {
		ml.DailyLimit = 0
	}
	return ml
}

// pcComputeGlobalRange computes the widest [min, max] across all methods.
// Uses the same union logic: lowest min, highest max, 0 if any is unlimited.
func pcComputeGlobalRange(methods map[string]MethodLimits) (globalMin, globalMax float64) {
	minLimited, maxLimited := true, true
	for _, ml := range methods {
		globalMin, minLimited = unionFloat(globalMin, minLimited, ml.SingleMin, true)
		globalMax, maxLimited = unionFloat(globalMax, maxLimited, ml.SingleMax, false)
	}
	if !minLimited {
		globalMin = 0
	}
	if !maxLimited {
		globalMax = 0
	}
	return globalMin, globalMax
}
