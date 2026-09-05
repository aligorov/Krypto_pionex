package autogrid

import (
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

// Close-class dictionaries and the v2.0.89 exchange-total sanity gate. The
// terminal-final estimate itself lives in finalprofit.go.

// lossClassClose reports whether a close reason (our closed_reason or the
// exchange's reasonBy) is a LOSS-class exit. The v2.0.89 sanity gate consults
// it for total-alias anomalies (a positive full total on a loss-class close).
func lossClassClose(reason string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(reason))
	switch {
	case strings.HasPrefix(normalized, "STOP_LOSS"),
		normalized == "LIQUIDATION",
		normalized == "STRUCT_INVALID_ANTI_HUNT",
		normalized == "RANGE_BREAK_DOWN",
		normalized == "RANGE_BREAK_UP",
		normalized == "RANGE_BREAK_UP_TREND_STOP",
		normalized == "LOSS_STOP",
		normalized == "FORCE_LIQUIDATION":
		return true
	}
	// RANGE_SHIFT_*_NO_ADJUSTMENTS_LEFT: budget exhaustion closed the bot
	// against the break, the same adverse exit as RANGE_BREAK_DOWN/UP.
	return strings.HasPrefix(normalized, "RANGE_SHIFT_") &&
		strings.HasSuffix(normalized, "_NO_ADJUSTMENTS_LEFT")
}

// unknownClassClose reports whether a close reason carries NO usable close
// class at all ("", ALREADY_CLOSED): nobody recorded what kind of exit this
// was.
func unknownClassClose(reason string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(reason))
	return normalized == "" || normalized == "ALREADY_CLOSED"
}

// gateSettledProfit is the v2.0.89 sanity gate on the exchange-total legs
// (profitExited / total-alias). The residual grid+funding legs it used to
// police are no longer accepted as finals anywhere (client.SettledProfit
// v2.0.89 returns only exchange totals), so the gate's only job is the
// unlikely total-alias anomaly: a POSITIVE full total on a loss-class close,
// decided from BOTH reason sources (stored closed_reason + exchange reasonBy
// — our manage stop comes back as "user cancel" at the exchange, so the
// stored reason is the only loss witness). Non-positive totals pass. Anything
// that is not an exchange total returns nil — the caller falls to the
// telemetry estimate ladder, it never treats the figure as a final.
func gateSettledProfit(
	total decimal.Decimal,
	source pionex.FinalProfitSource,
	storedReason, exchangeReason string,
) *decimal.Decimal {
	switch source {
	case pionex.FinalProfitExited, pionex.FinalProfitTotalAlias:
	default:
		return nil
	}
	if !total.IsPositive() {
		return &total
	}
	if lossClassClose(storedReason) || lossClassClose(exchangeReason) {
		return nil
	}
	return &total
}
