package autogrid

import (
	"context"
	"strings"
)

// symbolSectors maps base currencies to correlated clusters (fleet audit
// 2026-08-21: 6 of 10 bots were semis/AI-correlated with no cap anywhere in
// the codebase; a single −5% semis day could fire 6-8 protective closes
// simultaneously and then arm the circuit breaker against the re-entry
// window). The list is deliberately conservative — unmapped symbols are
// uncapped. Extend it as clusters are observed; leveraged-token perps
// (SOXSX/SOXLX) carry roughly their multiple in effective sector beta.
var symbolSectors = map[string]string{
	// Semiconductors + the leveraged semis tokens.
	"AMDX": "semis", "COHRX": "semis", "AXTIX": "semis", "TSMX": "semis",
	"DRAMX": "semis", "MUX": "semis", "EWYX": "semis", "SNDKX": "semis",
	"INTCX": "semis", "NVDA": "semis", "MU": "semis", "WOLF": "semis",
	"AVGO": "semis", "ASML": "semis", "TSM": "semis",
	"SOXSX": "semis", "SOXLX": "semis",
	// AI compute / GPU cloud.
	"CRWVX": "ai_compute", "NBISX": "ai_compute", "IRENX": "ai_compute",
}

// maxBotsPerSector caps simultaneous RUNNING bots sharing one sector — 3 of
// 10 slots ≈ the ~30% cluster ceiling from the fleet audit.
const maxBotsPerSector = 3

// sectorForSymbol returns the sector tag for a Pionex PERP symbol, or ""
// when the symbol is unmapped.
func sectorForSymbol(symbol string) string {
	base := strings.ToUpper(strings.TrimSpace(symbol))
	base = strings.TrimSuffix(base, "_PERP")
	base = strings.TrimSuffix(base, ".PERP")
	base = strings.TrimSuffix(base, "_USDT")
	return symbolSectors[base]
}

// sectorBotCount counts RUNNING paper bots whose symbol maps to the sector.
func (worker *Worker) sectorBotCount(ctx context.Context, settingsID string, sector string) int {
	symbols := make([]string, 0, len(symbolSectors))
	for base, s := range symbolSectors {
		if s == sector {
			symbols = append(symbols, base+"_USDT_PERP")
		}
	}
	if len(symbols) == 0 {
		return 0
	}
	var count int
	if err := worker.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM paper_grid_bots
		WHERE settings_id = $1 AND status = 'RUNNING'
		  AND symbol = ANY($2)
	`, settingsID, symbols).Scan(&count); err != nil {
		return 0
	}
	return count
}
