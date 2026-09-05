package autogrid

import (
	"context"
	"testing"
	"time"
)

// TestClosedBotsEstimateAndStateEpoch pins the v2.0.88 «одна правда на
// экране» contract end to end on one seeded fleet:
//
//   - a settled final (+1.5) — closed listing shows the realized figure,
//     no estimate leg;
//   - a refused settle (NULL final) WITH a telemetry trace (−2.5 one minute
//     before close) — closed listing shows realized NULL +
//     estimatedFinalUsdt −2.5, the SAME figure the epoch PnL sums into
//     closed_estimated;
//   - a refused settle with NO telemetry — realized NULL and no estimate:
//     the UI must render «стоп, финал неизвестен», never a fake 0.00;
//   - the /api/autogrid state payload carries the epoch summary itself
//     (State.Epoch), so the dashboard card, the autopilot hero and the
//     /equity endpoint cannot disagree.
func TestClosedBotsEstimateAndStateEpoch(t *testing.T) {
	env := newEquityTestEnv(t)
	ctx := context.Background()
	env.pinEpochAnchor(t, time.Now().UTC().Add(-time.Hour))

	knownBot := env.seedBot(t, "EQCE1_USDT_PERP", "EQ-CE1", "STOPPED", 50, "1.5", 0)
	_ = knownBot
	estimatedBot := env.seedBot(t, "EQCE2_USDT_PERP", "EQ-CE2", "STOPPED", 50, nil, 0)
	// The refused-settle trace: the last telemetry total before closed_at
	// is the exchange truth minus closing fees (−2.5, tagged estimated).
	env.seedTelemetry(t, estimatedBot, "-2.5", time.Now().UTC().Add(-time.Minute))
	unknownBot := env.seedBot(t, "EQCE3_USDT_PERP", "EQ-CE3", "STOPPED", 50, nil, 0)
	_ = unknownBot

	state, err := env.service.State(ctx)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	// (a) The epoch summary rides the state payload.
	if state.Epoch == nil {
		t.Fatal("state.epoch must be attached when the equity probe succeeds")
	}
	if !state.Epoch.EpochPnLUSDT.Equal(mustDec(t, "-1.0")) {
		t.Fatalf("epoch PnL = closed 1.5 + estimated −2.5, got %s", state.Epoch.EpochPnLUSDT)
	}
	if !state.Epoch.ClosedKnownUSDT.Equal(mustDec(t, "1.5")) {
		t.Fatalf("closed_known, got %s", state.Epoch.ClosedKnownUSDT)
	}
	if !state.Epoch.ClosedEstimatedUSDT.Equal(mustDec(t, "-2.5")) {
		t.Fatalf("closed_estimated, got %s", state.Epoch.ClosedEstimatedUSDT)
	}
	if state.Epoch.UnknownCount != 1 {
		t.Fatalf("unknown_count = 1 (NULL final, no telemetry), got %d", state.Epoch.UnknownCount)
	}

	// (b) The closed listing serves the same three finals with their
	// confidence legs. Locate by symbol: the settings scope is shared with
	// the other integration suites, so exact list length is not hermetic.
	bySymbol := make(map[string]ClosedBot, len(state.ClosedBots))
	for _, bot := range state.ClosedBots {
		bySymbol[bot.Symbol] = bot
	}
	known, ok := bySymbol["EQCE1_USDT_PERP"]
	if !ok {
		t.Fatal("settled bot missing from closed listing")
	}
	if known.RealizedPNLUSDT == nil || !known.RealizedPNLUSDT.Equal(mustDec(t, "1.5")) {
		t.Fatalf("settled bot must carry realized +1.5, got %v", known.RealizedPNLUSDT)
	}
	if known.EstimatedFinalUSDT != nil {
		t.Fatalf("settled bot must have no estimate leg, got %v", known.EstimatedFinalUSDT)
	}

	estimated, ok := bySymbol["EQCE2_USDT_PERP"]
	if !ok {
		t.Fatal("telemetry-estimated bot missing from closed listing")
	}
	if estimated.RealizedPNLUSDT != nil {
		t.Fatalf("refused settle must keep realized NULL (pre-2.0.88 COALESCE masked stop-losses as 0), got %v", estimated.RealizedPNLUSDT)
	}
	if estimated.EstimatedFinalUSDT == nil || !estimated.EstimatedFinalUSDT.Equal(mustDec(t, "-2.5")) {
		t.Fatalf("refused settle must carry the telemetry estimate −2.5, got %v", estimated.EstimatedFinalUSDT)
	}

	unknown, ok := bySymbol["EQCE3_USDT_PERP"]
	if !ok {
		t.Fatal("no-telemetry bot missing from closed listing")
	}
	if unknown.RealizedPNLUSDT != nil {
		t.Fatalf("unknown final must stay NULL, got %v", unknown.RealizedPNLUSDT)
	}
	if unknown.EstimatedFinalUSDT != nil {
		t.Fatalf("no telemetry → no invented estimate, got %v", unknown.EstimatedFinalUSDT)
	}
}
