package autogrid

import (
	"math"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
)

// roiCalmNeutralInput — спокойная нейтральная сетка базового кейса (а):
// σ_day 4% при спане 8% (16 уровней × шаг 0.5%) — геометрия самосогласована
// с доктриной спана v2.0.183 (span = max(8%, 2σ)), горизонт 48 ч, капитал
// $100×4, RT-издержки 20 bps (fee 5 + slip 5 ×2 ноги), фандинг 1 bp/8ч,
// стресс-проход −$7 (7% капитала — внутри 10%-го барьера).
//
// Ручная арифметика (проверка (а)), local-time модель ½(σ/шаг)²×утилизация:
//
//	циклов/сут = 1.0 × ½ × (4/0.5)²          = 32
//	циклов     = 32 × 2 суток                = 64 уровневых round-trip
//	сбор       = 64 × (0.5%−0.2%) × $25      = $4.80
//	фандинг    = 0.5×$400 × 1bp × 3×2        = $0.12
//	чистыми    = 4.80 − 0.12                 = $4.68
//	хвост      = 0.076 × $7                  = $0.532
//	риск-скор. = 4.68 − 0.532                = $4.148 ≥ 0.8×$2 → Feasible
//	винрейт    = 7/(7+4.68)                  = 0.599315 (контекст, не барьер)
//	полоса     = p25 0.45 → $2.04−0.53 = $1.51; p75 1.75 → $8.28−0.53 = $7.75
//
// Стресс-проход входит в вердикт вероятностно (p×L), а не барьером L ≤ 3G:
// медиана отношения стопа к прибыли у наших 96 прибыльных закрытий 19.8.
func roiCalmNeutralInput() ROIAssessmentInput {
	return ROIAssessmentInput{
		Direction:      "NEUTRAL",
		Investment:     usdt(100),
		SpanPct:        8,
		GridNum:        16,
		GridStepPct:    0.5,
		Leverage:       4,
		FeeBps:         5,
		SlippageBps:    5,
		StressLossUSDT: usdt(7),
		FundingBps8h:   1,
		HoldHours:      48,
		DayVolPct:      4,
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
	if got.EstCycles != 64 {
		t.Errorf("EstCycles = %v, хочу 64 (½×(4/0.5)²×2 суток)", got.EstCycles)
	}
	roiWantDecimal(t, got.EstNetUSDT, 4.68)
	roiWantDecimal(t, got.RiskAdjustedUSDT, 4.68-0.532)
	roiWantDecimal(t, got.RequiredWinRate, 7.0/(7.0+4.68))
	if got.Profile.Name != ROIProfileCalm2Pct.Name || got.Profile.Pct != 2.0 {
		t.Errorf("профиль не вернулся: %+v", got.Profile)
	}
	for _, want := range []string{"4.68", "0.53", "4.15", "2.00", "достижима", "64.0", "0.599", "1.51…7.75"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("причина без числа %q: %s", want, got.Reason)
		}
	}
}

// (б) SUI-кейс: стресс-проход −33.5 на капитал $100 при цели 5% — хвост
// 0.076×33.5 = $2.55 съедает $4.86 сбора до $2.31, то есть ниже 0.8×$5.00 =
// $4.00 → НЕ достижимо, с числами в причине.
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
		FundingBps8h:   3,
		HoldHours:      48,
		RoomPctPct:     4,
	}
	got := AssessROI(in, ROIProfileAggressive5Pct)

	if got.Feasible {
		t.Fatalf("SUI-кейс не должен быть достижим: %+v", got)
	}
	// Сбор 4%×0.5×$300 − RT 0.2%×$300 = $5.40, фандинг $300×3bp×6 = $0.54 →
	// чистыми $4.86; хвост 0.076×$33.5 = $2.546 → $2.314 против порога $4.00.
	roiWantDecimal(t, got.EstNetUSDT, 5.4-0.54)
	roiWantDecimal(t, got.RiskAdjustedUSDT, 5.4-0.54-0.076*33.5)
	// Контекст журнала: граница безубыточности 33.5/(33.5+4.86) = 0.873.
	roiWantDecimal(t, got.RequiredWinRate, 33.5/(33.5+5.4-0.54))
	if got.EstCycles != 4 { // 4% хода / 0.5% шаг × 0.5 лестницы
		t.Errorf("EstCycles = %v, хочу 4", got.EstCycles)
	}
	for _, want := range []string{"33.50", "2.55", "2.31", "4.00", "7.6%"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("причина без %q: %s", want, got.Reason)
		}
	}
	// У LONG/SHORT утилизация на лестницу не действует: полосы в причине нет.
	if strings.Contains(got.Reason, "полоса p25") {
		t.Errorf("направленная сетка не должна цитировать полосу дисперсии: %s", got.Reason)
	}
}

// (в) фандинг съедает цель: тот же спокойный кейс (а) при ставке 50 bps/8ч
// (0.5%/8ч — реальный уровень сквиза) уходит в минус исключительно из-за
// стоимости удержания.
func TestAssessROIFundingEatsTheHarvest(t *testing.T) {
	calm := AssessROI(roiCalmNeutralInput(), ROIProfileCalm2Pct)
	if !calm.Feasible {
		t.Fatalf("базовый кейс (1 bp) должен быть достижим: %s", calm.Reason)
	}

	hotInput := roiCalmNeutralInput()
	hotInput.FundingBps8h = 50
	hot := AssessROI(hotInput, ROIProfileCalm2Pct)

	if hot.Feasible {
		t.Fatalf("фандинг 50 bps/8ч не съел цель: %s", hot.Reason)
	}
	// Разница чистых ровно как дополнительный фандинг: 0.5×400×49bp×6 = 5.88.
	roiWantDecimal(t, calm.EstNetUSDT.Sub(hot.EstNetUSDT), 5.88)
	// Фандинг 0.5×$400×50bp×6 = $6.00 против сбора $4.80 → −$1.20.
	roiWantDecimal(t, hot.EstNetUSDT, -1.20)
	// Отрицательный чистый сбор не покрывает стресс-проход вовсе: граница
	// безубыточности вырождается в 1, а причина обязана назвать фандинг числом.
	if hot.RequiredWinRate.InexactFloat64() != 1.0 {
		t.Errorf("RequiredWinRate = %s, хочу 1 (сбор ≤ 0 не покрывает стоп)", hot.RequiredWinRate)
	}
	if !strings.Contains(hot.Reason, "фандинг") || !strings.Contains(hot.Reason, "6.00") {
		t.Errorf("причина должна называть фандинг с числом: %s", hot.Reason)
	}
	// Вердикт даёт именно фандинг: хвост тот же ($0.532), риск-скорректированный
	// сбор −1.73 против порога 1.60.
	roiWantDecimal(t, hot.RiskAdjustedUSDT, -1.20-0.532)
}

// Знак фандинга: ставка SIGNED — положительная означает «лонги платят
// шортам», поэтому SHORT её получает, LONG платит, NEUTRAL тарифицируется
// как плательщик (его инвентарь ниже середины диапазона длинный).
func TestAssessROIFundingSignFollowsDirection(t *testing.T) {
	base := roiCalmNeutralInput()
	base.Direction = "LONG"
	base.RoomPctPct = 4
	base.FundingBps8h = 10

	longSide := AssessROI(base, ROIProfileCalm2Pct)
	shortInput := base
	shortInput.Direction = "SHORT"
	shortSide := AssessROI(shortInput, ROIProfileCalm2Pct)

	// Нотционал $400, инвентарь полный: 400×10bp×6 = $2.40 — шорт получает,
	// лонг платит, разница ровно 2×$2.40.
	roiWantDecimal(t, shortSide.EstNetUSDT.Sub(longSide.EstNetUSDT), 4.80)
	if !strings.Contains(shortSide.Reason, "доход шорта") {
		t.Errorf("причина шорта должна называть фандинг доходом: %s", shortSide.Reason)
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
	// Плечо 2: notional $200, per-level $12.5, сбор 64×0.3%×12.5 = $2.40,
	// фандинг $0.06 → $2.34, минус хвост $0.532 → $1.808 ≥ $1.60;
	// плечо 8: $9.60 − $0.24 − $0.532 = $8.828.
	loInput := roiCalmNeutralInput()
	loInput.Leverage = 2
	lo := AssessROI(loInput, ROIProfileCalm2Pct)
	hiInput := roiCalmNeutralInput()
	hiInput.Leverage = 8
	hi := AssessROI(hiInput, ROIProfileCalm2Pct)
	if !lo.Feasible {
		t.Fatalf("при плече 2 риск-скорректированный сбор 1.81 покрывает порог 1.60: %s", lo.Reason)
	}
	if !hi.Feasible {
		t.Fatalf("при плече 8 чистый сбор 9.36 должен покрывать порог: %s", hi.Reason)
	}
	roiWantDecimal(t, lo.EstNetUSDT, 2.40-0.06)
	roiWantDecimal(t, hi.EstNetUSDT, 9.60-0.24)
	for name, reason := range map[string]string{"плечо 2": lo.Reason, "плечо 8": hi.Reason} {
		if !strings.Contains(reason, "2.00") {
			t.Errorf("%s: цель должна остаться 2.00 USDT, причина: %s", name, reason)
		}
	}
}

// Вырожденные входы: циклы/цель не оцениваются → всегда НЕ достижимо, с причиной.
func TestAssessROIDegenerateInputs(t *testing.T) {
	// Нет дневной σ: модель циклов квадратична по σ, fallback удалён (v2.0.197)
	// — циклов 0, сбор 0, fail-closed.
	noVol := roiCalmNeutralInput()
	noVol.DayVolPct = 0
	got := AssessROI(noVol, ROIProfileCalm2Pct)
	if got.Feasible || got.EstCycles != 0 {
		t.Fatalf("без дневной σ не может быть достижимо: %+v", got)
	}
	if !strings.Contains(got.Reason, "дневная σ") {
		t.Errorf("причина должна называть отсутствие σ: %s", got.Reason)
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
// ставка 0.03-0.05%/сутки). Мёртвый телеметрический фид дорожает оценку и
// обязан быть назван в причине числом.
func TestAssessROIFundingZeroMeansConservativeNotFree(t *testing.T) {
	noData := roiCalmNeutralInput()
	noData.FundingBps8h = 0
	explicit := roiCalmNeutralInput()
	explicit.FundingBps8h = 5
	live := roiCalmNeutralInput() // 1 bp/8ч — базовый кейс (а)

	got := AssessROI(noData, ROIProfileCalm2Pct)
	ref := AssessROI(explicit, ROIProfileCalm2Pct)
	base := AssessROI(live, ROIProfileCalm2Pct)
	if !got.EstNetUSDT.Equal(ref.EstNetUSDT) {
		t.Fatalf("0 на входе должен тарифицироваться как явные 5 bps/8ч: %s против %s",
			got.EstNetUSDT, ref.EstNetUSDT)
	}
	// 0.5×$400 × 0.05% × 3×2 суток = $0.60 → чистыми 4.80−0.60 = $4.20, то
	// есть на $0.48 хуже живой ставки 1 bp ($4.68).
	roiWantDecimal(t, got.EstNetUSDT, 4.8-0.6)
	roiWantDecimal(t, base.EstNetUSDT.Sub(got.EstNetUSDT), 0.48)
	// Контекст журнала: граница безубыточности 7/(7+4.20) = 0.625.
	roiWantDecimal(t, got.RequiredWinRate, 0.625)
	if !strings.Contains(got.Reason, "0.60") {
		t.Errorf("причина должна называть фандинг числом 0.60: %s", got.Reason)
	}
}

// (3) Честность модуля: «2-5% за 24 ч» — не планировочная норма. На
// самосогласованной геометрии (шаг 0.5%, спан 8%, капитал $100×4) local-time
// модель даёт сбор $0.54/сутки при σ_day 2% и $1.78/сутки при σ_day 3.5% —
// профили 3.5% и 5% за 24 ч недостижимы. Профиль 5% недостижим и на верхней
// границе волатильного коридора: σ_day 5% → $3.69 − хвост $0.53 = $3.16 <
// порога $4.00.
//
// σ_day 5% с профилем 3.5% — единственный достижимый агрессивный случай, и он
// держится ровно на медиане: нижняя квартиль полосы ($1.10) ниже порога
// ($2.80). То есть «3.5% за сутки» возможно только как медианный исход
// волатильной пары, не как обещание — поэтому воркер деплоит исключительно
// профиль calm_2pct на горизонте 48 ч.
func TestAssessROIFivePercentPerDaySystematicallyNotFeasible(t *testing.T) {
	for _, tc := range []struct {
		dayVol float64
		prof   ROIProfile
	}{
		{2, ROIProfileBase3_5Pct}, {2, ROIProfileAggressive5Pct},
		{3.5, ROIProfileBase3_5Pct}, {3.5, ROIProfileAggressive5Pct},
		{5, ROIProfileAggressive5Pct},
	} {
		in := roiCalmNeutralInput()
		in.DayVolPct = tc.dayVol
		in.HoldHours = 24
		got := AssessROI(in, tc.prof)
		if got.Feasible {
			t.Fatalf("σ_day=%.1f%%: профиль %.1f%% за 24ч не должен быть достижим: %s",
				tc.dayVol, tc.prof.Pct, got.Reason)
		}
	}

	// Протокол чисел для σ_day=3.5%, профиль 3.5%/24ч: циклов = ½×(3.5/0.5)² =
	// 24.5, сбор 24.5×0.3%×$25 = $1.8375, фандинг 0.5×$400×1bp×3 = $0.06 →
	// чистыми $1.7775, минус хвост $0.532 → $1.2455 против порога $2.80;
	// полоса p25–p75 = $0.23…$2.62.
	in := roiCalmNeutralInput()
	in.DayVolPct = 3.5
	in.HoldHours = 24
	got := AssessROI(in, ROIProfileBase3_5Pct)
	if got.EstCycles != 24.5 {
		t.Errorf("EstCycles = %v, хочу 24.5 (½×(3.5/0.5)²×1 сутки)", got.EstCycles)
	}
	roiWantDecimal(t, got.EstNetUSDT, 1.8375-0.06)
	if !strings.Contains(got.Reason, "полоса p25–p75: 0.23…2.62 USDT") {
		t.Errorf("причина должна цитировать полосу дисперсии числами: %s", got.Reason)
	}

	// Верхняя граница коридора: σ_day 5%, профиль 3.5%/24ч — достижимо на
	// медиане ($3.69 − $0.53 = $3.16 ≥ $2.80), но p25 полосы ($1.10) порога не
	// покрывает: запас держится ровно на медиане модели.
	edge := roiCalmNeutralInput()
	edge.DayVolPct = 5
	edge.HoldHours = 24
	gotEdge := AssessROI(edge, ROIProfileBase3_5Pct)
	if !gotEdge.Feasible {
		t.Fatalf("σ_day=5%%/24ч на профиле 3.5%% модель считает достижимым: %s", gotEdge.Reason)
	}
	roiWantDecimal(t, gotEdge.EstNetUSDT, 3.75-0.06)
	roiWantDecimal(t, gotEdge.RiskAdjustedUSDT, 3.75-0.06-0.532)
	if !strings.Contains(gotEdge.Reason, "полоса p25–p75: 1.10…5.97 USDT") {
		t.Errorf("причина должна показывать, что запас держится на медиане: %s", gotEdge.Reason)
	}
}
