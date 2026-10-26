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
		normalized == "RANGE_BREAK_UP_EARLY",
		normalized == "RANGE_BREAK_DOWN_EARLY",
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
// total-alias anomaly: a NON-NEGATIVE full total on a loss-class close, decided
// from BOTH reason sources (stored closed_reason + exchange reasonBy — our
// manage stop comes back as "user cancel" at the exchange, so the stored
// reason is the only loss witness). Anything that is not an exchange total
// returns nil — the caller falls to the telemetry estimate ladder, it never
// treats the figure as a final.
//
// v2.0.196: on a loss-class close the carrier must be strictly NEGATIVE. The
// old `!IsPositive()` pass let a settled 0 through, and SettledProfit's switch
// to presence-based parsing (client.go v2.0.196) turned the XMR-class record —
// `profitExited:"0"` next to a live `profitReduce` leg on a loss stop — into a
// booked final of exactly 0. A loss exit cost money by definition, so a flat
// carrier there is as unverifiable as a positive one, and booking it would
// erase the loss from realized_pnl_usdt and from the 24h daily-loss breaker.
//
// v2.0.196: nil means UNVERIFIABLE, not "proved wrong". The API docs define
// profitExited only as «Exited profit» and publish no netting formula (no
// field is documented as the bot's total/net profit), so a positive figure on a
// stop-class close may be a genuine grid-over-stop win (CRWVX #1440) or a
// partial leg that omits the position close — the payload cannot tell them
// apart. A caller that gets nil must therefore prefer the unlock identity
// (unlockIdentityFinal: cash back − cash in, true by construction) and
// otherwise keep the row re-probeable; freezing it at the floored telemetry
// estimate would overwrite a figure the wallet may still contradict.
func gateSettledProfit(
	total decimal.Decimal,
	source pionex.FinalProfitSource,
	storedReason, exchangeReason string,
) *decimal.Decimal {
	switch source {
	case pionex.FinalProfitUnlockIdentity:
		// unlock_identity is the post-settlement cash reality:
		// unlockUsdtAmount − quote_investment. True by construction, bounded by
		// isolated margin (checked in terminal_recheck). Always accepted, including
		// positive totals on stop exits (e.g. CRWVX #1440 where grid profit exceeded stop loss).
		return &total

	case pionex.FinalProfitExited, pionex.FinalProfitTotalAlias:
		if lossClassClose(storedReason) || lossClassClose(exchangeReason) {
			if total.IsNegative() {
				return &total
			}
			return nil
		}
		return &total

	default:
		return nil
	}
}
