package autogrid

import (
	"context"
	"errors"

	"github.com/shopspring/decimal"
)

// Preset is a researched starting point for one market phase. Presets never
// touch executionMode, accountId or budget — the operator keeps manual
// control of those. Applying a preset goes through the normal settings
// validation and durable risk gates (autopilot must be STOPPED).
type Preset struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Phase       string      `json:"phase"`
	Description string      `json:"description"`
	WhenToUse   string      `json:"whenToUse"`
	Patch       PresetPatch `json:"patch"`
}

type PresetPatch struct {
	MaxActiveBots           *int             `json:"maxActiveBots,omitempty"`
	Leverage                *int             `json:"leverage,omitempty"`
	MinSharpe               *decimal.Decimal `json:"minSharpe,omitempty"`
	MinEVPct                *decimal.Decimal `json:"minEvPct,omitempty"`
	StopLossMode            *string          `json:"stopLossMode,omitempty"`
	AdaptiveLeverageEnabled *bool            `json:"adaptiveLeverageEnabled,omitempty"`
	DensityGridEnabled      *bool            `json:"densityGridEnabled,omitempty"`
	CandleInterval          *string          `json:"candleInterval,omitempty"`
	LookbackCandles         *int             `json:"lookbackCandles,omitempty"`
	MaxSymbolsPerScan       *int             `json:"maxSymbolsPerScan,omitempty"`
	ScanIntervalSeconds     *int             `json:"scanIntervalSeconds,omitempty"`
	MinVolume24h            *decimal.Decimal `json:"minVolume24h,omitempty"`
	MinVolatilityPct        *decimal.Decimal `json:"minVolatilityPct,omitempty"`
	MaxVolatilityPct        *decimal.Decimal `json:"maxVolatilityPct,omitempty"`
	MaxDrawdownPct          *decimal.Decimal `json:"maxDrawdownPct,omitempty"`
	MinProfitFactor         *decimal.Decimal `json:"minProfitFactor,omitempty"`
	PnLTargetUSDT           *decimal.Decimal `json:"pnlTargetUsdt,omitempty"`
	MaxLossUSDT             *decimal.Decimal `json:"maxLossUsdt,omitempty"`
	PnLTargetMode           *string          `json:"pnlTargetMode,omitempty"`
	ManageIntervalSeconds   *int             `json:"manageIntervalSeconds,omitempty"`
	RangeBreakBufferPct     *decimal.Decimal `json:"rangeBreakBufferPct,omitempty"`
	MaxAdjustmentsPerBot    *int             `json:"maxAdjustmentsPerBot,omitempty"`
	AIKitEnabled            *bool            `json:"aiKitEnabled,omitempty"`
	AIAutotuneEnabled       *bool            `json:"aiAutotuneEnabled,omitempty"`
	AIAutotuneInterval      *int             `json:"aiAutotuneIntervalSeconds,omitempty"`
}

func decimalPtr(value string) *decimal.Decimal {
	result := decimal.RequireFromString(value)
	return &result
}

func intPtr(value int) *int {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

// MarketPhasePresets follows documented best practice: neutral grids harvest
// ranging markets, directional grids ride confirmed trends, volatile phases
// need wider ranges with low leverage and hard stops.
func MarketPhasePresets() []Preset {
	return []Preset{
		{
			ID:    "flat_harvester",
			Title: "Флэт-харвест",
			Phase: "Боковик / консолидация",
			Description: "Нейтральные гриды с узкой волатильностной полосой и частыми мелкими фиксациями. " +
				"Диапазон строится от поддержки/сопротивления, плечо низкое, стоп адаптивный — классическая " +
				"настройка для рынка, который «пилит» в коридоре.",
			WhenToUse: "Когда детектор режима на сканах выдаёт преимущественно RANGE, а ADX низкий. " +
				"Основной режим заработка гридов — держите его включённым по умолчанию в консолидации.",
			Patch: PresetPatch{
				MaxActiveBots: intPtr(3), Leverage: intPtr(2),
				MinSharpe: decimalPtr("0.2"), MinEVPct: decimalPtr("0"),
				StopLossMode: stringPtr("ADAPTIVE_ATR"), AdaptiveLeverageEnabled: boolPtr(true),
				DensityGridEnabled: boolPtr(false), CandleInterval: stringPtr("30M"),
				LookbackCandles: intPtr(192), MaxSymbolsPerScan: intPtr(15),
				ScanIntervalSeconds: intPtr(900), MinVolume24h: decimalPtr("500000"),
				MinVolatilityPct: decimalPtr("1.5"), MaxVolatilityPct: decimalPtr("12"),
				MaxDrawdownPct: decimalPtr("15"), MinProfitFactor: decimalPtr("1.05"),
				PnLTargetMode:         stringPtr("DYNAMIC"),
				ManageIntervalSeconds: intPtr(60), RangeBreakBufferPct: decimalPtr("1"),
				MaxAdjustmentsPerBot: intPtr(2), AIKitEnabled: boolPtr(true),
				AIAutotuneEnabled: boolPtr(true),
			},
		},
		{
			ID:    "trend_rider",
			Title: "Тренд-райдер",
			Phase: "Восходящий тренд",
			Description: "Сканер предпочитает long-гриды (вход из нижней трети диапазона), диапазон шире, " +
				"сдвиги сетки следуют за трендом (до 4 сдвигов), буфер пробоя увеличен — даём ходу развернуться. " +
				"Цель PnL на бота выше, стоп компенсирует редкие развороты.",
			WhenToUse: "Когда рынок подтверждённо растёт: EMA20>EMA50, ADX выше 22, скан выдаёт TREND_UP. " +
				"Не включайте при перекупленности перед сильным сопротивлением.",
			Patch: PresetPatch{
				MaxActiveBots: intPtr(3), Leverage: intPtr(3),
				MinSharpe: decimalPtr("0.25"), MinEVPct: decimalPtr("0"),
				StopLossMode: stringPtr("ADAPTIVE_ATR"), AdaptiveLeverageEnabled: boolPtr(true),
				DensityGridEnabled: boolPtr(false), CandleInterval: stringPtr("60M"),
				LookbackCandles: intPtr(120), MaxSymbolsPerScan: intPtr(15),
				ScanIntervalSeconds: intPtr(900), MinVolume24h: decimalPtr("500000"),
				MinVolatilityPct: decimalPtr("2"), MaxVolatilityPct: decimalPtr("40"),
				MaxDrawdownPct: decimalPtr("18"), MinProfitFactor: decimalPtr("1.05"),
				PnLTargetMode:         stringPtr("DYNAMIC"),
				ManageIntervalSeconds: intPtr(60), RangeBreakBufferPct: decimalPtr("1.5"),
				MaxAdjustmentsPerBot: intPtr(4), AIKitEnabled: boolPtr(true),
				AIAutotuneEnabled: boolPtr(true),
			},
		},
		{
			ID:    "bear_shield",
			Title: "Бэр-щит",
			Phase: "Нисходящий тренд",
			Description: "Short-гриды за падающим рынком с строгим контролем убытка: цель ниже, стоп ближе, " +
				"ведение чаще (45 c), сдвигов мало — при развороте вверх быстро закрываемся.",
			WhenToUse: "Подтверждённое падение (TREND_DOWN на сканах, EMA20<EMA50, ADX>22). " +
				"Осторожно в конце затяжного падения — отскоки бьют по short-гридам.",
			Patch: PresetPatch{
				MaxActiveBots: intPtr(2), Leverage: intPtr(2),
				MinSharpe: decimalPtr("0.25"), MinEVPct: decimalPtr("0"),
				StopLossMode: stringPtr("ADAPTIVE_ATR"), AdaptiveLeverageEnabled: boolPtr(true),
				DensityGridEnabled: boolPtr(false), CandleInterval: stringPtr("60M"),
				LookbackCandles: intPtr(120), MaxSymbolsPerScan: intPtr(12),
				ScanIntervalSeconds: intPtr(900), MinVolume24h: decimalPtr("500000"),
				MinVolatilityPct: decimalPtr("2"), MaxVolatilityPct: decimalPtr("40"),
				MaxDrawdownPct: decimalPtr("15"), MinProfitFactor: decimalPtr("1.05"),
				PnLTargetMode:         stringPtr("DYNAMIC"),
				ManageIntervalSeconds: intPtr(45), RangeBreakBufferPct: decimalPtr("1"),
				MaxAdjustmentsPerBot: intPtr(2), AIKitEnabled: boolPtr(true),
				AIAutotuneEnabled: boolPtr(true),
			},
		},
		{
			ID:    "turbulence",
			Title: "Турбулентность",
			Phase: "Высокая волатильность / прорывы",
			Description: "Широкие диапазоны (вола 6–35%), геометрическая сетка, плечо 1x без адаптива, " +
				"увеличенный буфер пробоя — не дёргаемся на шуме. Цель и стоп крупнее под масштаб движений.",
			WhenToUse: "Новости, кризисы, резкие expansion-фазы: ATR и волатильность скана аномально высокие. " +
				"Никакого плеча — wide-range neutral гриды на выживание и сбор движения.",
			Patch: PresetPatch{
				MaxActiveBots: intPtr(2), Leverage: intPtr(1),
				MinSharpe: decimalPtr("0.15"), MinEVPct: decimalPtr("0"),
				StopLossMode: stringPtr("ADAPTIVE_ATR"), AdaptiveLeverageEnabled: boolPtr(false),
				DensityGridEnabled: boolPtr(true), CandleInterval: stringPtr("60M"),
				LookbackCandles: intPtr(120), MaxSymbolsPerScan: intPtr(10),
				ScanIntervalSeconds: intPtr(600), MinVolume24h: decimalPtr("1000000"),
				MinVolatilityPct: decimalPtr("6"), MaxVolatilityPct: decimalPtr("45"),
				MaxDrawdownPct: decimalPtr("20"), MinProfitFactor: decimalPtr("1.05"),
				PnLTargetMode:         stringPtr("DYNAMIC"),
				ManageIntervalSeconds: intPtr(45), RangeBreakBufferPct: decimalPtr("2"),
				MaxAdjustmentsPerBot: intPtr(3), AIKitEnabled: boolPtr(true),
			},
		},
		{
			ID:    "sandbox",
			Title: "Песочница (строгая)",
			Phase: "Проверка стратегии",
			Description: "Жёсткие фильтры качества (Sharpe≥1.0, EV>0, PF≥1.1), 1–2 бота, плечо 1x, " +
				"узкая вола. Максимум избирательности — чтобы в PAPER увидеть только сильнейшие сетапы.",
			WhenToUse: "Перед переходом на REAL: прогоните цикл скан→открытие→закрытие и убедитесь, что " +
				"отбраковка работает и закрытия идут с прибылью.",
			Patch: PresetPatch{
				MaxActiveBots: intPtr(2), Leverage: intPtr(1),
				MinSharpe: decimalPtr("1.0"), MinEVPct: decimalPtr("0.05"),
				StopLossMode: stringPtr("ADAPTIVE_ATR"), AdaptiveLeverageEnabled: boolPtr(true),
				DensityGridEnabled: boolPtr(false), CandleInterval: stringPtr("60M"),
				LookbackCandles: intPtr(120), MaxSymbolsPerScan: intPtr(10),
				ScanIntervalSeconds: intPtr(900), MinVolume24h: decimalPtr("1000000"),
				MinVolatilityPct: decimalPtr("2"), MaxVolatilityPct: decimalPtr("12"),
				MaxDrawdownPct: decimalPtr("12"), MinProfitFactor: decimalPtr("1.1"),
				PnLTargetMode:         stringPtr("DYNAMIC"),
				ManageIntervalSeconds: intPtr(60), RangeBreakBufferPct: decimalPtr("1"),
				MaxAdjustmentsPerBot: intPtr(2), AIKitEnabled: boolPtr(true),
				AIAutotuneEnabled: boolPtr(true),
			},
		},
	}
}

// mergePreset overlays a preset patch onto the current settings while keeping
// the operator-managed fields (mode, account, budget) untouched.
func mergePreset(current Settings, preset Preset) UpdateSettingsInput {
	input := UpdateSettingsInput{
		AccountID:               current.AccountID,
		ExecutionMode:           current.ExecutionMode,
		BudgetUSDT:              current.BudgetUSDT,
		MaxActiveBots:           current.MaxActiveBots,
		Leverage:                current.Leverage,
		MinSharpe:               current.MinSharpe,
		MinEVPct:                current.MinEVPct,
		StopLossMode:            current.StopLossMode,
		SmartPNLEnabled:         current.SmartPNLEnabled,
		AdaptiveLeverageEnabled: current.AdaptiveLeverageEnabled,
		DensityGridEnabled:      current.DensityGridEnabled,
		CandleInterval:          current.CandleInterval,
		LookbackCandles:         current.LookbackCandles,
		MaxSymbolsPerScan:       current.MaxSymbolsPerScan,
		ScanIntervalSeconds:     current.ScanIntervalSeconds,
		MinVolume24h:            current.MinVolume24h,
		MinVolatilityPct:        current.MinVolatilityPct,
		MaxVolatilityPct:        current.MaxVolatilityPct,
		MaxDrawdownPct:          current.MaxDrawdownPct,
		MinProfitFactor:         current.MinProfitFactor,
		FeeBps:                  current.FeeBps,
		SlippageBps:             current.SlippageBps,
		PnLTargetMode:           current.PnLTargetMode,
		PnLTargetUSDT:           current.PnLTargetUSDT,
		MaxLossUSDT:             current.MaxLossUSDT,
		ManageIntervalSeconds:   current.ManageIntervalSeconds,
		RangeBreakBufferPct:     current.RangeBreakBufferPct,
		MaxAdjustmentsPerBot:    current.MaxAdjustmentsPerBot,
		AIKitEnabled:            current.AIKitEnabled,
		AIAutotuneEnabled:       current.AIAutotuneEnabled,
		AIAutotuneInterval:      current.AIAutotuneInterval,
	}
	patch := preset.Patch
	if patch.MaxActiveBots != nil {
		input.MaxActiveBots = *patch.MaxActiveBots
	}
	if patch.Leverage != nil {
		input.Leverage = *patch.Leverage
	}
	if patch.MinSharpe != nil {
		input.MinSharpe = *patch.MinSharpe
	}
	if patch.MinEVPct != nil {
		input.MinEVPct = *patch.MinEVPct
	}
	if patch.StopLossMode != nil {
		input.StopLossMode = *patch.StopLossMode
	}
	if patch.AdaptiveLeverageEnabled != nil {
		input.AdaptiveLeverageEnabled = *patch.AdaptiveLeverageEnabled
	}
	if patch.DensityGridEnabled != nil {
		input.DensityGridEnabled = *patch.DensityGridEnabled
	}
	if patch.CandleInterval != nil {
		input.CandleInterval = *patch.CandleInterval
	}
	if patch.LookbackCandles != nil {
		input.LookbackCandles = *patch.LookbackCandles
	}
	if patch.MaxSymbolsPerScan != nil {
		input.MaxSymbolsPerScan = *patch.MaxSymbolsPerScan
	}
	if patch.ScanIntervalSeconds != nil {
		input.ScanIntervalSeconds = *patch.ScanIntervalSeconds
	}
	if patch.MinVolume24h != nil {
		input.MinVolume24h = *patch.MinVolume24h
	}
	if patch.MinVolatilityPct != nil {
		input.MinVolatilityPct = *patch.MinVolatilityPct
	}
	if patch.MaxVolatilityPct != nil {
		input.MaxVolatilityPct = *patch.MaxVolatilityPct
	}
	if patch.MaxDrawdownPct != nil {
		input.MaxDrawdownPct = *patch.MaxDrawdownPct
	}
	if patch.MinProfitFactor != nil {
		input.MinProfitFactor = *patch.MinProfitFactor
	}
	if patch.PnLTargetMode != nil {
		input.PnLTargetMode = *patch.PnLTargetMode
	}
	if patch.PnLTargetUSDT != nil {
		input.PnLTargetUSDT = *patch.PnLTargetUSDT
	}
	if patch.MaxLossUSDT != nil {
		input.MaxLossUSDT = *patch.MaxLossUSDT
	}
	if patch.ManageIntervalSeconds != nil {
		input.ManageIntervalSeconds = *patch.ManageIntervalSeconds
	}
	if patch.RangeBreakBufferPct != nil {
		input.RangeBreakBufferPct = *patch.RangeBreakBufferPct
	}
	if patch.MaxAdjustmentsPerBot != nil {
		input.MaxAdjustmentsPerBot = *patch.MaxAdjustmentsPerBot
	}
	if patch.AIKitEnabled != nil {
		input.AIKitEnabled = *patch.AIKitEnabled
	}
	if patch.AIAutotuneEnabled != nil {
		input.AIAutotuneEnabled = *patch.AIAutotuneEnabled
	}
	if patch.AIAutotuneInterval != nil {
		input.AIAutotuneInterval = *patch.AIAutotuneInterval
	}
	return input
}

// ApplyPreset applies a market-phase preset through the standard settings
// pipeline: STOPPED-only, validated, durable risk gates enforced.
func (s *Service) ApplyPreset(ctx context.Context, presetID string) (*Settings, *Preset, error) {
	var selected *Preset
	for index, preset := range MarketPhasePresets() {
		if preset.ID == presetID {
			selected = &MarketPhasePresets()[index]
			break
		}
	}
	if selected == nil {
		return nil, nil, errors.New("unknown preset " + presetID)
	}
	current, err := s.GetSettings(ctx)
	if err != nil {
		return nil, nil, err
	}
	updated, err := s.UpdateSettings(ctx, mergePreset(*current, *selected))
	if err != nil {
		return nil, selected, err
	}
	return updated, selected, nil
}
