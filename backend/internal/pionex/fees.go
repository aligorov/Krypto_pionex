package pionex

import "github.com/shopspring/decimal"

// Round-trip fee model for the REAL ledger (v2.0.89).
//
// The exchange's per-bot PnL fields (profitReduce, fundingFeePayment) are
// gross of trading fees: the 2026-09-03..05 REAL epoch closed with the
// operator's futures wallet at −$31 while the bot aggregate summed to −$6.87
// — the difference is fee drag no bot field ever showed. The ledger books it
// explicitly:
//
//	entry fee  = taker 0.05% on the notional every deploy and every
//	             invest_in pour opens (investment × leverage);
//	close cost = taker 0.05% + slippage 0.05% on the inventory notional the
//	             last telemetry row carried into the close.
//
// Both rates are fixed exchange truth, not operator-tunable settings — they
// must never drift with the scanner's fee/slippage bps.
const (
	// TakerFeeRate is Pionex's futures taker fee (0.05%).
	TakerFeeRate = 0.0005
	// CloseCostSlippageRate is the assumed adverse slippage on the closing
	// fill (0.05% of inventory notional) on top of the taker fee.
	CloseCostSlippageRate = 0.0005
)

// closeCostTotalRate is the full per-dollar-of-inventory cost of closing a
// position at market: taker fee + slippage.
var closeCostTotalRate = decimal.NewFromFloat(TakerFeeRate + CloseCostSlippageRate)

// takerFeeRateDec is the decimal form of TakerFeeRate for the entry legs.
var takerFeeRateDec = decimal.NewFromFloat(TakerFeeRate)

// FinalProfitTelemetryMissing is the v2.0.89 settle marker for a terminal the
// exchange gave no total AND our own telemetry cannot price: the final stays
// NULL (better empty than a guess). It lives beside the fee model because the
// marker vocabulary in client.go must stay stable for parallel edits; the
// constant is the settle-path twin of FinalProfitTelemetryNetClose.
const FinalProfitTelemetryMissing FinalProfitSource = "telemetry_missing"

// EntryFeeUSDT prices the taker fee a deploy or an invest_in pour pays on the
// position notional it opens: taker 0.05% × quote dollars × leverage. Empirical
// anchor: the operator's 25-entry epoch (~$150–600 notional each) paid ≈$2.3
// of entry fees, consistent with taker-on-notional.
func EntryFeeUSDT(investment decimal.Decimal, leverage int) decimal.Decimal {
	if investment.IsNegative() || investment.IsZero() || leverage <= 0 {
		return decimal.Zero
	}
	return investment.Mul(decimal.NewFromInt(int64(leverage))).Mul(takerFeeRateDec)
}

// CloseCostUSDT prices the cost of closing a terminal bot's inventory at
// market: (taker 0.05% + slippage 0.05%) × inventory notional. A flat position
// (zero notional) closes for free — there is nothing to unload.
func CloseCostUSDT(inventoryNotional decimal.Decimal) decimal.Decimal {
	if inventoryNotional.IsNegative() || inventoryNotional.IsZero() {
		return decimal.Zero
	}
	return inventoryNotional.Mul(closeCostTotalRate)
}
