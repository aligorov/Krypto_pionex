package market

import (
	"time"
)

type MarketSession string

const (
	SessionAsia       MarketSession = "ASIA"       // 00:00 - 08:00 UTC (Tokyo, Singapore, Hong Kong) - Grid Paradise (Mean Reversion)
	SessionLondon     MarketSession = "LONDON"     // 07:00 - 15:30 UTC (London, Frankfurt) - Liquidity Sweeps, Trend Formation
	SessionNewYork    MarketSession = "NEW_YORK"   // 13:30 - 21:00 UTC (New York) - High Volatility, Macro Impulses
	SessionTransition MarketSession = "TRANSITION" // 21:00 - 00:00 UTC - Quiet Roll
)

type SessionInfo struct {
	CurrentSession     MarketSession `json:"currentSession"`
	UTCHour            int           `json:"utcHour"`
	IsMacroNewsWindow  bool          `json:"isMacroNewsWindow"`
	RecommendedMaxBots int           `json:"recommendedMaxBots"`
	MaxLeverageLimit   int           `json:"maxLeverageLimit"`
	GridSpacingMult    float64       `json:"gridSpacingMult"`
	AntiHuntBufferMult float64       `json:"antiHuntBufferMult"`
	Description        string        `json:"description"`
}

// GetCurrentSession returns the active global trading session and its risk profile.
func GetCurrentSession(now time.Time) SessionInfo {
	utc := now.UTC()
	hour := utc.Hour()
	minute := utc.Minute()

	// High impact US macro news window: 13:20 to 14:15 UTC (CPI, PPI, NFP, FOMC releases at 13:30/14:00)
	isMacroNews := (hour == 13 && minute >= 20) || (hour == 14 && minute <= 15)

	switch {
	case hour >= 0 && hour < 8:
		return SessionInfo{
			CurrentSession:     SessionAsia,
			UTCHour:            hour,
			IsMacroNewsWindow:  false,
			RecommendedMaxBots: 30,
			MaxLeverageLimit:   3,
			GridSpacingMult:    1.0, // Standard / tight scalping spacing
			AntiHuntBufferMult: 1.2,
			Description:        "Азиатская сессия: спокойный боковик, идеальное время для 25-30 сеток",
		}
	case hour >= 8 && hour < 13:
		return SessionInfo{
			CurrentSession:     SessionLondon,
			UTCHour:            hour,
			IsMacroNewsWindow:  false,
			RecommendedMaxBots: 25,
			MaxLeverageLimit:   3,
			GridSpacingMult:    1.2, // Slight expansion for liquidity sweeps
			AntiHuntBufferMult: 1.5,
			Description:        "Лондонская сессия: умеренная волатильность, расширенные буферы стопа",
		}
	case hour >= 13 && hour < 21:
		maxLev := 2
		if isMacroNews {
			maxLev = 2
		}
		return SessionInfo{
			CurrentSession:     SessionNewYork,
			UTCHour:            hour,
			IsMacroNewsWindow:  isMacroNews,
			RecommendedMaxBots: 20,
			MaxLeverageLimit:   maxLev,
			GridSpacingMult:    1.5, // Wider spacing to absorb American volatility
			AntiHuntBufferMult: 2.0,
			Description:        "Американская сессия: высокая волатильность, защита стопов и умеренное плечо",
		}
	default:
		return SessionInfo{
			CurrentSession:     SessionTransition,
			UTCHour:            hour,
			IsMacroNewsWindow:  false,
			RecommendedMaxBots: 25,
			MaxLeverageLimit:   3,
			GridSpacingMult:    1.0,
			AntiHuntBufferMult: 1.3,
			Description:        "Переходный период: постепенное затухание американских импульсов",
		}
	}
}
