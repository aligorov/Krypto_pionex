package marketdata

import (
	"testing"

	"github.com/aligorov/pionex-bot/backend/internal/pionex"
	"github.com/shopspring/decimal"
)

func makeCandle(o, h, l, c, v float64) pionex.KlineCandle {
	return pionex.KlineCandle{
		Open:   decimal.NewFromFloat(o),
		High:   decimal.NewFromFloat(h),
		Low:    decimal.NewFromFloat(l),
		Close:  decimal.NewFromFloat(c),
		Volume: decimal.NewFromFloat(v),
	}
}

func TestAnalyzeCandle_PinBarBullish(t *testing.T) {
	// Classic hammer / bullish pin bar: Open 10, Low 7, High 10.5, Close 10.2
	// Range: 3.5. Lower wick: 10 - 7 = 3.0. LowerWickRatio: 3.0 / 3.5 = 85.7%
	candle := makeCandle(10.0, 10.5, 7.0, 10.2, 1000)
	metrics := AnalyzeCandle(candle)

	if !metrics.IsPinBarBull {
		t.Fatalf("expected IsPinBarBull to be true, got false (LowerWickRatio=%.2f, BodyRatio=%.2f)",
			metrics.LowerWickRatio, metrics.BodyRatio)
	}
	if metrics.IsPinBarBear {
		t.Fatalf("did not expect IsPinBarBear")
	}
	if metrics.LowerWickRatio < 0.60 {
		t.Fatalf("expected LowerWickRatio >= 0.60, got %.2f", metrics.LowerWickRatio)
	}
}

func TestAnalyzeCandle_MarubozuBearish(t *testing.T) {
	// Impulsive red candle (falling knife): Open 10, High 10.05, Low 8.95, Close 9.0
	// Range: 1.10. Body: 1.0. BodyRatio: 90.9%
	candle := makeCandle(10.0, 10.05, 8.95, 9.0, 5000)
	metrics := AnalyzeCandle(candle)

	if !metrics.IsMarubozuBear {
		t.Fatalf("expected IsMarubozuBear to be true, got false (BodyRatio=%.2f)", metrics.BodyRatio)
	}
	if metrics.IsPinBarBull {
		t.Fatalf("did not expect IsPinBarBull on a knife")
	}
}

func TestDetectEngulfing(t *testing.T) {
	// Prev: red candle Open 10, High 10.2, Low 9.4, Close 9.5
	prev := makeCandle(10.0, 10.2, 9.4, 9.5, 100)
	// Curr: giant green candle Open 9.4, High 10.6, Low 9.3, Close 10.5 (engulfs prev)
	curr := makeCandle(9.4, 10.6, 9.3, 10.5, 300)

	isEngulf, side := DetectEngulfing(prev, curr)
	if !isEngulf || side != "BULLISH" {
		t.Fatalf("expected BULLISH engulfing, got isEngulf=%v, side=%s", isEngulf, side)
	}
}

func TestDetectSFP_ORDI_ReversalCase(t *testing.T) {
	// Key support level was at 10.00
	priorLow := decimal.NewFromFloat(10.00)

	// ORDI-style sweep: price dropped to 9.60 (sweeping stops), but buyers stepped in,
	// driving price back up to close at 10.15 (High 10.20, Open 9.95)
	candle := makeCandle(9.95, 10.20, 9.60, 10.15, 2500)

	isSweep, wickRatio := DetectSFP(priorLow, candle)
	if !isSweep {
		t.Fatalf("expected SFP sweep to be detected for ORDI case, wickRatio=%.2f", wickRatio)
	}
	if wickRatio < 0.50 {
		t.Fatalf("expected meaningful wick ratio >= 0.50, got %.2f", wickRatio)
	}
}
