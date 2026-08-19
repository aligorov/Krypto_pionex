package autogrid

import (
	"context"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

// harGeometryResult carries the HAR-derived grid geometry plus the raw
// forecast for observability (leverage reason strings, bot events).
type harGeometryResult struct {
	geo         marketdata.GridGeometry
	forecastPct float64
}

// harMinConfidence is the in-sample R² below which the HAR fit carries no
// information beyond the ATR baseline and the adaptive mesh stays in charge.
const harMinConfidence = 0.05

// harGridGeometry fetches ~60 daily candles, trains the HAR-RV volatility
// model and translates the next-day forecast into grid geometry (range
// width, level count, volatility-inverse leverage). This is the wiring that
// makes the v2.0 "HAR-RV → ширина/шаг/плечо" feature real for BOTH paper and
// real deploys. Returns nil on any failure — insufficient history, candle
// fetch error, degenerate or weak fit — and the caller keeps the
// battle-tested ATR adaptive mesh.
//
// Since v2.0.10 feeBps must be fee+slippage (round-trip viability), and
// budgetUsdt caps the level count so each level's order notional stays
// exchange-executable (≈$5 floor) — a 100-level grid on a $200 budget is
// fine on paper but would fail Pionex min-order checks in REAL mode.
func (worker *Worker) harGridGeometry(ctx context.Context, symbol string, feeBps, budgetUsdt float64) *harGeometryResult {
	candles, err := worker.publicClient.GetKlines(ctx, symbol, "1D", 60)
	if err != nil {
		worker.logger.Debug("har geometry: candle fetch failed, mesh fallback",
			"component", "autogrid_worker", "symbol", symbol, "error", err)
		return nil
	}
	model, forecast, err := marketdata.ForecastVolatilityFromCandles(candles)
	if err != nil {
		worker.logger.Debug("har geometry: forecast failed, mesh fallback",
			"component", "autogrid_worker", "symbol", symbol, "error", err)
		return nil
	}
	// Sanity clamps: R² below the floor or an annualized forecast outside
	// 5%..500% means the fit degenerated — the mesh is the safer answer.
	if model.RSquared < harMinConfidence || forecast < 5 || forecast > 500 {
		worker.logger.Debug("har geometry: weak fit, mesh fallback",
			"component", "autogrid_worker", "symbol", symbol,
			"r2", model.RSquared, "forecast", forecast)
		return nil
	}
	return &harGeometryResult{
		geo:         marketdata.ComputeGridGeometry(forecast, model.RSquared, feeBps, budgetUsdt),
		forecastPct: forecast,
	}
}

// applyToMesh recenters the grid on the current price with the HAR range
// width, level count and fee-viable step, replacing the S/R-derived bounds.
// The stop stays ATR-based (ComputeAntiHuntStop) — it is independent of the
// range source and already validated in production.
func (r *harGeometryResult) applyToMesh(price decimal.Decimal, mesh *AdaptiveMeshResult) {
	half := price.Mul(decimal.NewFromFloat(r.geo.RangePct / 200.0))
	mesh.LowerPrice = price.Sub(half)
	mesh.UpperPrice = price.Add(half)
	mesh.GridNum = r.geo.GridCount
	mesh.GridStepPct = decimal.NewFromFloat(r.geo.StepPct)
}
