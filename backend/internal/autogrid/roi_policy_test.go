package autogrid

import (
	"math"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// roiCalmNeutralInput — спокойная нейтральная сетка базового кейса (а):
// DayVolPct НЕ передан → OU-fallback оценка циклов (σ_day-калибровку гоняет
// TestAssessROIFivePercentPerDaySystematicallyNotFeasible). Спан 8%
// (v2.0.163 floor), 16 уровней → шаг 0.5%, OU-полужизнь 4 ч, горизонт 48 ч,
// капитал $100×4, RT-издержки 20 bps (fee 5 + slip 5 ×2 ноги), фандинг
// 1 bp/8ч, стресс-проход −$7 против медианной победы класса $25.
//
// Ручная арифметика (проверка (а)):
//   проходов   = 48/4 × 0.5                 = 6
//   циклов     = 6 × 16 × 0.5               = 48 уровневых round-trip
//   сбор       = 48 × (0.5%−0.2%) × $25     = $3.60
//   фандинг    = 0.5×$400 × 1bp × 3×2       = $0.12
//   винрейт    = 7/(25+7)                   = 0.21875
//   хвост      = 0.21875 × $7               = $1.53125
//   чистыми    = 3.60 − 0.12 − 1.53125      = $1.94875 ≥ 0.8×$2 → Feasible
func roiCalmNeutralInput() ROIAssessmentInput {
	return ROIAssessmentInput{
		Direction:       "NEUTRAL",
		Investment:      usdt(100),
		SpanPct:         8,
		GridNum:         16,
		GridStepPct:     0.5,
		Leverage:        4,
		FeeBps:          5,
		SlippageBps:     5,
		StressLossUSDT:  usdt(7),
		AvgWinUSDT:      usdt(25),
		FundingBps8h:    1,
		HoldHours:       48,
		OUHalfLifeHours: 4,
	}
}

func roiWantDecimal(t *testing.T, got decimal.Decimal, want float64) {
	t.Helper()
	if diff := math.Abs(got.InexactFloat64() - want); diff > 1e-9 {
		t.Fatalf("ожидалось %v, получено %s (расхождение %g)", want, got, diff)
	}
}

func TestRequiredWinRateBinaryBound(t *testing.T) {
	cases := []struct {
		stress, win float64
		want        float64
	}{
		{7, 25, 7.0 / 32.0},    // спокойный класс: 0.21875
		{33.5, 5, 33.5 / 38.5}, // SUI-класс: ~0.87
		{10, 10, 0.5},          // симметрия
		{0, 10, 0},             // хвоста нет — любой винрейт окупается
		{0, 0, 0},              // нет данных вовсе
		{5, 0, 1},              // побед нет — EV≥0 недостижим
	}
	for _, c := range cases {
		got := RequiredWinRate(ROIEconomics{
			TargetNetUSDT:  usdt(2),
			StressLossUSDT: usdt(c.stress),
			AvgWinUSDT:     usdt(c.win),
		})
		if diff := math.Abs(got.InexactFloat64() - c.want); diff > 1e-9 {
			t.Errorf("RequiredWinRate(стресс=%v, победа=%v) = %s, хочу %v", c.stress, c.win, got, c.want)
		}
	}
}

// (а) профиль 2% достижим на спокойной нейтральной сетке.
func TestAssessROICalmNeutralGridFeasible(t *testing.T) {
	got := AssessROI(roiCalmNeutralInput(), ROIProfileCalm2Pct)

	if !got.Feasible {
		t.Fatalf("спокойная сетка должна быть достижима, причина: %s", got.Reason)
	}
	if got.EstCycles != 48 {
		t.Errorf("EstCycles = %v, хочу 48 (48/4×0.5×16×0.5)", got.EstCycles)
	}
	roiWantDecimal(t, got.EstNetUSDT, 1.94875)
	roiWantDecimal(t, got.RequiredWinRate, 0.21875)
	if got.Profile.Name != ROIProfileCalm2Pct.Name || got.Profile.Pct != 2.0 {
		t.Errorf("профиль не вернулся: %+v", got.Profile)
	}
	for _, want := range []string{"1.95", "2.00", "достижима", "48.0"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("причина без числа %q: %s", want, got.Reason)
		}
	}
}

// (б) SUI-кейс: цель ~4.6% при стресс-проходе −33.5 — RequiredWinRate ~0.87
// выше потолка 0.75 → НЕ достижимо, с числами в причине.
func TestAssessROISUIStressCaseNotFeasible(t *testing.T) {
	in := ROIAssessmentInput{
		Direction:      "LONG",
		Investment:     usdt(100),
		SpanPct:        6,
		GridNum:        12,
		GridStepPct:    0.5,
		Leverage:       3,
		FeeBps:         5,
		SlippageBps:    5,
		StressLossUSDT: usdt(33.5),
		AvgWinUSDT:     usdt(5),
		FundingBps8h:   3,
		HoldHours:      48,
		RoomPctPct:     4,
	}
	got := AssessROI(in, ROIProfileAggressive5Pct)

	if got.Feasible {
		t.Fatalf("SUI-кейс не должен быть достижим: %+v", got)
	}
	if rwr := got.RequiredWinRate.InexactFloat64(); math.Abs(rwr-0.8701298701) > 1e-6 {
		t.Errorf("RequiredWinRate = %s, хочу ~0.87 (33.5/38.5)", got.RequiredWinRate)
	}
	// Хвост = 0.8701×33.5 ≈ 29.15 сжирает сбор 5.4−0.54 → чистыми ≈ −24.29.
	roiWantDecimal(t, got.EstNetUSDT, 5.4-0.54-33.5*33.5/38.5)
	if got.EstCycles != 4 { // 4% хода / 0.5% шаг × 0.5 лестницы
		t.Errorf("EstCycles = %v, хочу 4", got.EstCycles)
	}
	for _, want := range []string{"винрейт", "0.870", "0.75", "33.50", "5.00"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("причина без %q: %s", want, got.Reason)
		}
	}
}

// (в) фандинг 10 bps/8ч съедает цель: тот же спокойный кейс (а) падает ниже
// 80%-порога исключительно из-за стоимости удержания.
func TestAssessROIFundingEatsTheHarvest(t *testing.T) {
	calm := AssessROI(roiCalmNeutralInput(), ROIProfileCalm2Pct)
	if !calm.Feasible {
		t.Fatalf("базовый кейс (1 bp) должен быть достижим: %s", calm.Reason)
	}

	hotInput := roiCalmNeutralInput()
	hotInput.FundingBps8h = 10
	hot := AssessROI(hotInput, ROIProfileCalm2Pct)

	if hot.Feasible {
		t.Fatalf("фандинг 10 bps/8ч не съел цель: %s", hot.Reason)
	}
	// Разница чистых ровно как дополнительный фандинг: 0.5×400×9bp×6 ≈ 1.08.
	roiWantDecimal(t, calm.EstNetUSDT.Sub(hot.EstNetUSDT), 1.08)
	roiWantDecimal(t, hot.EstNetUSDT, 0.86875)
	// Винрейт не при чём — причина обязана указать на фандинг числом.
	roiWantDecimal(t, hot.RequiredWinRate, 0.21875)
	if !strings.Contains(hot.Reason, "фандинг") || !strings.Contains(hot.Reason, "1.20") {
		t.Errorf("причина должна называть фандинг с числом: %s", hot.Reason)
	}
}

// (г) плечо не меняет процент цели: TargetNet — функция капитала и профиля,
// не нотационала (investment×leverage).
func TestROITargetIndependentOfLeverage(t *testing.T) {
	cases := []struct {
		invest float64
		prof   ROIProfile
		want   float64
	}{
		{100, ROIProfileCalm2Pct, 2.0},
		{200, ROIProfileBase3_5Pct, 7.0},
		{50, ROIProfileAggressive5Pct, 2.5},
	}
	for _, c := range cases {
		roiWantDecimal(t, ROITargetNetUSDT(usdt(c.invest), c.prof), c.want)
	}
	if got := ROITargetNetUSDT(usdt(0), ROIProfileCalm2Pct); !got.IsZero() {
		t.Errorf("цель при нулевом капитале должна быть 0, получено %s", got)
	}

	// Через публичную оценку: цель в причине — 2.00 USDT при любом плече
	// (сам сбор и вердикт с плечом меняются, определение цели — нет).
	loInput := roiCalmNeutralInput()
	loInput.Leverage = 2
	lo := AssessROI(loInput, ROIProfileCalm2Pct)
	hiInput := roiCalmNeutralInput()
	hiInput.Leverage = 8
	hi := AssessROI(hiInput, ROIProfileCalm2Pct)
	if lo.Feasible {
		t.Fatalf("при плече 2 сбор 0.21 не должен покрывать порог 1.60: %s", lo.Reason)
	}
	if !hi.Feasible {
		t.Fatalf("при плече 8 сбор ~5.43 должен покрывать порог: %s", hi.Reason)
	}
	for name, reason := range map[string]string{"плечо 2": lo.Reason, "плечо 8": hi.Reason} {
		if !strings.Contains(reason, "2.00") {
			t.Errorf("%s: цель должна остаться 2.00 USDT, причина: %s", name, reason)
		}
	}
}

// Вырожденные входы: циклы/цель не оцениваются → всегда НЕ достижимо, с причиной.
func TestAssessROIDegenerateInputs(t *testing.T) {
	// Нет OU-оценки: циклов 0, сбор 0.
	noOU := roiCalmNeutralInput()
	noOU.OUHalfLifeHours = 0
	got := AssessROI(noOU, ROIProfileCalm2Pct)
	if got.Feasible || got.EstCycles != 0 {
		t.Fatalf("без OU-полужизни не может быть достижимо: %+v", got)
	}
	if !strings.Contains(got.Reason, "OU-полужизни") {
		t.Errorf("причина должна называть отсутствие OU-оценки: %s", got.Reason)
	}

	for _, tc := range []struct {
		name   string
		mutate func(in *ROIAssessmentInput)
		marker string
	}{
		{"нулевая маржа", func(in *ROIAssessmentInput) { in.Investment = usdt(0) }, "капитал"},
		{"нулевое плечо", func(in *ROIAssessmentInput) { in.Leverage = 0 }, "плечо"},
		{"уровней нет", func(in *ROIAssessmentInput) { in.GridNum = 0 }, "уровней"},
		{"горизонта нет", func(in *ROIAssessmentInput) { in.HoldHours = 0 }, "горизонт"},
	} {
		in := roiCalmNeutralInput()
		tc.mutate(&in)
		got := AssessROI(in, ROIProfileCalm2Pct)
		if got.Feasible {
			t.Errorf("%s: не должен быть достижим", tc.name)
		}
		if !strings.Contains(got.Reason, tc.marker) {
			t.Errorf("%s: причина без маркера %q: %s", tc.name, tc.marker, got.Reason)
		}
	}

	// Направленная сетка без хода до препятствия.
	noRoom := roiCalmNeutralInput()
	noRoom.Direction = "LONG"
	noRoom.RoomPctPct = 0
	got = AssessROI(noRoom, ROIProfileCalm2Pct)
	if got.Feasible || !strings.Contains(got.Reason, "препятствия") {
		t.Fatalf("LONG без хода не должен быть достижим: %s", got.Reason)
	}
}

// Фандинг без данных: FundingBps8h==0 — маркер «нет данных», а не «бесплатно»:
// тарифицируется консервативными 5 bps/8ч (0.15%/сутки на нотационал; типичная
// ставка 0.03-0.05%/сутки). Мёртвый телеметрический фид дорожает оценку.
func TestAssessROIFundingZeroMeansConservativeNotFree(t *testing.T) {
	noData := roiCalmNeutralInput()
	noData.FundingBps8h = 0
	explicit := roiCalmNeutralInput()
	explicit.FundingBps8h = 5

	got, ref := AssessROI(noData, ROIProfileCalm2Pct), AssessROI(explicit, ROIProfileCalm2Pct)
	if !got.EstNetUSDT.Equal(ref.EstNetUSDT) {
		t.Fatalf("0 на входе должен тарифицироваться как явные 5 bps/8ч: %s против %s",
			got.EstNetUSDT, ref.EstNetUSDT)
	}
	// 0.5×$400 × 0.05% × 3×2 суток = $0.60 — даже спокойная сетка честно
	// падает ниже порога 1.60: живая ставка фандинга обязана быть на входе.
	roiWantDecimal(t, got.EstNetUSDT, 3.6-0.6-1.53125)
	if got.Feasible {
		t.Fatalf("при консервативном фандинге спокойная сетка не должна проходить: %s", got.Reason)
	}
	if !strings.Contains(got.Reason, "0.60") {
		t.Errorf("причина должна называть фандинг числом 0.60: %s", got.Reason)
	}
}

// (3) Честность модуля: «2-5% за 24 ч» систематически недостижимо. Потолок
// практики живых сеток — 0.15-0.6%/день (r/Pionex); теория пересечений даёт
// k≈7.8 (arXiv 2506.11921: пересечения ∝ local time/шаг, n²/4 циклов
// диапазона), планировочные ярусы 2.0/2.5. Профили 3.5% и 5% за HoldHours=24
// отклоняются на разумной геометрии при любой дневной σ из разумного коридора.
func TestAssessROIFivePercentPerDaySystematicallyNotFeasible(t *testing.T) {
	for _, dayVol := range []float64{2, 3.5, 5} {
		for _, prof := range []ROIProfile{ROIProfileBase3_5Pct, ROIProfileAggressive5Pct} {
			in := roiCalmNeutralInput()
			in.DayVolPct = dayVol
			in.HoldHours = 24
			got := AssessROI(in, prof)
			if got.Feasible {
				t.Fatalf("σ_day=%.1f%%: профиль %.1f%% за 24ч не должен быть достижим: %s",
					dayVol, prof.Pct, got.Reason)
			}
		}
	}

	// Протокол чисел для σ_day=3.5%, профиль 3.5%/24ч: консервативный ярус
	// k=2.0 → 2.0×3.5/0.5×1 = 14 циклов, сбор $1.05, чистыми −$0.54 против
	// порога $2.80; оптимистичный ярус k=2.5 (17.5 циклов → −$0.28) назван.
	in := roiCalmNeutralInput()
	in.DayVolPct = 3.5
	in.HoldHours = 24
	got := AssessROI(in, ROIProfileBase3_5Pct)
	if got.EstCycles != 14 {
		t.Errorf("EstCycles = %v, хочу 14 (2.0×3.5/0.5×1 сутки, консервативный ярус)", got.EstCycles)
	}
	roiWantDecimal(t, got.EstNetUSDT, 1.05-0.06-1.53125)
	if !strings.Contains(got.Reason, "оптимистичный ярус k=2.5: -0.28") {
		t.Errorf("причина должна цитировать оптимистичный ярус k=2.5 числом: %s", got.Reason)
	}
}
