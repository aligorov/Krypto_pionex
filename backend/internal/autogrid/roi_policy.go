package autogrid

import (
	"fmt"
	"strings"

	"github.com/aligorov/pionex-bot/backend/internal/marketdata"
	"github.com/shopspring/decimal"
)

// ───────────────────────── Wave C: политика ROI ─────────────────────────
//
// Модуль волны C плана «2-5% за 24-72 ч»: ДО создания бота проверить, что
// ЧИСТАЯ цель профиля (2% / 3.5% / 5% капитала за горизонт 24-72 ч) достижима
// на ФИНАЛЬНОЙ геометрии сетки — той, что уйдёт в нативный
// /api/v1/bot/orders/futuresGrid/create. Оценка чистая по построению: из
// сбора вычитаются RT-комиссия на каждый цикл, фандинг на средний инвентарь
// (при отсутствии данных о ставке — консервативные 0.15%/сутки, не ноль) и
// стресс-хвост (ожидание полного прохода до стопа при требуемом винрейте).
//
// Модуль НЕ принимает решений: AssessROI возвращает оценку (Feasible +
// Reason с числами), deploy отклоняет/пускает вызывающий воркер. Пороги —
// именованные константы ниже с доктринальными комментариями. Всё в decimal,
// ни БД, ни ENV (Zero-ENV Runtime Policy) — чистая функция над числами,
// которые вызывающий уже вывел из Pionex-источников.
const (
	// feasibleNetShareOfTarget — порог достижимости: ожидаемый чистый сбор
	// должен покрывать ≥80% цели профиля. 80%, а не 100%: оценка циклов —
	// среднее модели с большой дисперсией (фактическое число завершённых
	// проходов за 24-72 ч гуляет в разы); требовать ровно 100% — резать
	// спокойные сетки на шуме собственной оценки. 20% запаса — цена
	// модельного оптимизма; меньше — самообман.
	feasibleNetShareOfTarget = 0.8

	// maxAcceptableRequiredWinRate — потолок бинарного требуемого винрейта:
	// L/(G+L) ≤ 0.75 ⟺ медианная победа класса ≥ 1/3 стресс-прохода. Если
	// для EV≥0 класс должен побеждать чаще 75%, у сетки нет запаса
	// прочности — один-два стресс-прохода до стопа съедают дюжину побед
	// (класс SUI: цель 4.6% при стрессе −33.5 требовал винрейт ~0.87).
	maxAcceptableRequiredWinRate = 0.75

	// neutralCycleUtilization — коэффициент утилизации OU-циклов (fallback
	// путь без σ_day): не каждая полужизнь даёт завершённый проход цены
	// через сетку (трендовые срывы, неполные развороты, затишья).
	neutralCycleUtilization = 0.5

	// neutralSweepLevelShare — доля уровней, завершающих round-trip за один
	// проход: вход в середине диапазона (доктрина деплоя), разворот
	// отрабатывает половину сетки — та же полу-нотациональная ось правды,
	// что в stressInventoryLossUSDT и neutralGridPaperPNL (нейтральная сетка
	// с входом в середине держит половину нотационала на сторону).
	neutralSweepLevelShare = 0.5

	// directionalLadderShare — доля лестницы в направленном сборе: LONG/SHORT
	// собирает ход до структурного препятствия в среднем половиной лестницы
	// (часть уровней не успевает завершиться до препятствия/цели).
	directionalLadderShare = 0.5

	// neutralAvgInventoryShare — средний инвентарь нейтральной сетки за
	// горизонт: сетка циклирует вокруг полузагруженного состояния.
	neutralAvgInventoryShare = 0.5

	// directionalAvgInventoryShare — средний инвентарь направленной сетки:
	// полный нотационал с самого входа (v2.0.97 exchange truth — та же ось,
	// что в stressInventoryLossUSDT: LONG/SHORT открывает ПОЛНЫЙ нотационал).
	directionalAvgInventoryShare = 1.0

	// fundingPeriodsPerDay — фандинг Pionex платится каждые 8 часов.
	fundingPeriodsPerDay = 3.0

	// hoursPerDay — перевод HoldHours в сутки (циклы/день, ставка фандинга).
	hoursPerDay = 24.0
)

// ───────────── Калибровка циклов и фандинга (ресёрч волны C) ─────────────
//
// Циклы/день ≈ k × σ_day% / шаг%. Обоснование: arXiv 2506.11921 — число
// пересечений уровня пропорционально local time, делённому на шаг, а
// диапазон из n шагов даёт ~n²/4 полных циклов, что задаёт теоретический
// потолок k ≈ 7.8 циклов/день на единицу (σ_day/шаг). Практика живых сеток
// (r/Pionex: сбор 0.15–0.6%/день) сидит в ~3× ниже потолка, отсюда
// планировочные ярусы 2.5/2.0.
const (
	// cycleRateKOptimistic — оптимистичный ярус оценки циклов (k=2.5):
	// цитируется в Reason как верхняя граница, вердиктом НЕ управляет.
	cycleRateKOptimistic = 2.5

	// cycleRateKConservative — консервативный ярус (k=2.0): ПО НЕМУ считаются
	// EstCycles/EstNetUSDT и порог Feasible — спокойное планирование.
	cycleRateKConservative = 2.0
)

// fundingFallbackBps8h — стоимость удержания при ОТСУТСТВИИ данных о
// фандинге (FundingBps8h==0 на входе): типичная ставка 0.03–0.05%/сутки на
// нотационал, консервативная оценка 0.15%/сутки = 5 bps/8ч (×3 периода).
// Ноль на входе — маркер «нет данных», а не «удержание бесплатно»: мёртвый
// телеметрический фид должен ДОРОЖАТЬ оценку, а не занулять издержку
// (AGENTS: a dead telemetry feed must degrade loudly).
const fundingFallbackBps8h = 5.0

// ROIProfile — целевой профиль волны C: имя + чистая цель в % капитала.
type ROIProfile struct {
	Name string
	Pct  float64
}

// Три профиля плана «2-5% за 24-72 ч».
var (
	ROIProfileCalm2Pct       = ROIProfile{Name: "calm_2pct", Pct: 2.0}
	ROIProfileBase3_5Pct     = ROIProfile{Name: "base_3_5pct", Pct: 3.5}
	ROIProfileAggressive5Pct = ROIProfile{Name: "aggressive_5pct", Pct: 5.0}
)

// ROIEconomics — экономика класса бота: всё в decimal, издержки включены.
type ROIEconomics struct {
	// TargetNetUSDT — цель = капитал×профиль (2%/3.5%/5%). Контекстное поле
	// для вызывающего; бинарная граница RequiredWinRate от него не зависит.
	TargetNetUSDT decimal.Decimal
	// StressLossUSDT — полный стресс-проход до стопа в USDT: ГОТОВОЕ число
	// из stressGeometry/stressInventoryLossUSDT, здесь не пересчитывается.
	StressLossUSDT decimal.Decimal
	// AvgWinUSDT — медианная историческая победа класса (параметр).
	AvgWinUSDT decimal.Decimal
}

// RequiredWinRate — бинарная нижняя граница побед для EV≥0:
// p×G − (1−p)×L ≥ 0  ⟺  p ≥ L/(G+L).
// Вырожденные случаи: стресс ≤ 0 → 0 (хвоста нет — окупается любой винрейт);
// победа ≤ 0 при стрессе > 0 → 1 (EV≥0 недостижим ни при каком винрейте < 1 —
// потолок 0.75 в AssessROI такое отвергнет).
func RequiredWinRate(e ROIEconomics) decimal.Decimal {
	loss, win := e.StressLossUSDT, e.AvgWinUSDT
	if !loss.IsPositive() {
		return decimal.Zero
	}
	if !win.IsPositive() {
		return decimal.NewFromInt(1)
	}
	denom := win.Add(loss)
	if !denom.IsPositive() {
		return decimal.Zero
	}
	return loss.Div(denom)
}

// ROIAssessmentInput — финальная геометрия сетки + экономика класса на
// момент проверки (до нативного create).
type ROIAssessmentInput struct {
	// Direction — NEUTRAL|LONG|SHORT. Как в stressInventoryLossUSDT: всё,
	// кроме LONG/SHORT, трактуется как нейтраль.
	Direction string

	Investment  decimal.Decimal // капитал бота (внесённая маржа)
	SpanPct     float64         // финальный спан сетки, % (контекст вызова; математика — по шагу)
	GridNum     int
	GridStepPct float64         // шаг уровня, %
	Leverage    int

	// FeeBps/SlippageBps — издержки на ногу; RT-комиссия = 2×(fee+slip),
	// единая ось правды с marketdata.RoundTripCostPct.
	FeeBps, SlippageBps float64

	StressLossUSDT  decimal.Decimal // готовый стресс-проход до стопа (из stressGeometry)
	AvgWinUSDT      decimal.Decimal // медианная историческая победа класса
	FundingBps8h    float64         // текущая ставка фандинга, bps за 8 ч; 0 = нет данных → консервативные 5 bps/8ч
	HoldHours       float64         // горизонт проверки 24-72 ч
	DayVolPct       float64         // NEUTRAL: дневная σ пары, %; >0 → циклы по калибровке k×σ/шаг (ярусы 2.0/2.5)
	OUHalfLifeHours float64         // NEUTRAL fallback: OU-полужизнь, ч (при DayVolPct ≤ 0; ≤0 — оценки нет)
	RoomPctPct      float64         // LONG/SHORT: ход до структурного препятствия, %
}

// ROIAssessment — оценка достижимости цели (НЕ решение: deploy решает воркер).
type ROIAssessment struct {
	Feasible bool
	Reason   string // человекочитаемо, по-русски, с числами
	// EstCycles — КОНСЕРВАТИВНЫЙ ярус (k=2.0 при калибровке по σ_day)
	// ожидаемого числа завершённых сеточных циклов (уровневых round-trip)
	// за HoldHours: NEUTRAL = k×σ_day/шаг×сутки (или OU-fallback
	// проходы×утилизация×полсетки); LONG/SHORT = (ход/шаг)×доля лестницы.
	EstCycles float64
	// EstNetUSDT — ожидаемый ЧИСТЫЙ сбор консервативного яруса:
	// циклы×(шаг−RT)×нотационал/уровень − фандинг − стресс-хвост (для
	// LONG/SHORT — ход×лестница×нотационал − RT на вход+выход).
	// Оптимистичный ярус (k=2.5) цитируется только в Reason.
	EstNetUSDT      decimal.Decimal
	RequiredWinRate decimal.Decimal
	Profile         ROIProfile
}

// ROITargetNetUSDT — чистая цель профиля: капитал×Pct/100. СОЗНАТЕЛЬНО не
// функция плеча: цель волны C — чистая доходность на ВНЕСЁННЫЙ капитал;
// плечо масштабирует сбор и риск, но не определение цели (тест модуля (г)).
func ROITargetNetUSDT(investment decimal.Decimal, profile ROIProfile) decimal.Decimal {
	if !investment.IsPositive() {
		return decimal.Zero
	}
	return investment.Mul(decimal.NewFromFloat(profile.Pct)).Div(decimal.NewFromInt(100))
}

// AssessROI — главный вход политики. Оценка достижимости чистой цели на
// финальной сетке; решениЙ не принимает. Feasible считается по
// КОНСЕРВАТИВНОМУ ярусу (k=2.0), оптимистичный (k=2.5) — только контекст.
//
// NEUTRAL, оценка циклов (в порядке приоритета):
//  1. DayVolPct > 0 — калибровка ресёрча волны C: циклы/день = k×σ_day/шаг
//     (arXiv 2506.11921: пересечения ∝ local time/шаг, n²/4 циклов
//     диапазона, потолок k≈7.8; практический дисконт ~3× → ярусы 2.5/2.0);
//  2. OUHalfLifeHours > 0 — OU-fallback: проходы = HoldHours/HL×утилизация
//     0.5, циклов = проходы×gridNum×0.5 (полсетки на проход);
//  3. иначе — вырожденный случай: сбор 0, Feasible=false с причиной.
// Сбор за цикл = (шаг − RT) на per-level нотационале
// (investment×leverage/gridNum).
//
// LONG/SHORT: сбор = ход до препятствия × доля лестницы 0.5 × нотационал −
// RT-комиссия на вход+выход полного нотационала; циклы = ход/шаг×0.5.
//
// Издержки всегда: RT-комиссия на каждый цикл (в шаге для NEUTRAL, отдельной
// ногой для LONG/SHORT); фандинг FundingBps8h×3×HoldHours/24 на средний
// инвентарь (0.5×нотационала для NEUTRAL, полный для LONG/SHORT), а при
// FundingBps8h==0 (нет данных) — консервативные 5 bps/8ч (0.15%/сутки), не
// ноль; стресс-хвост = RequiredWinRate×StressLoss — ожидание полного прохода
// до стопа при требуемом винрейте.
//
// Feasible = EstNetUSDT(консервативно) ≥ TargetNetUSDT×0.8 И
// RequiredWinRate ≤ 0.75; вырожденная геометрия (нет σ_day и OU-оценки,
// шаг/ход ≤ 0 и т.п.) — всегда Feasible=false с причиной. Иначе причина
// содержит числа.
func AssessROI(in ROIAssessmentInput, profile ROIProfile) ROIAssessment {
	a := ROIAssessment{Profile: profile}

	rwr := RequiredWinRate(ROIEconomics{
		TargetNetUSDT:  ROITargetNetUSDT(in.Investment, profile),
		StressLossUSDT: in.StressLossUSDT,
		AvgWinUSDT:     in.AvgWinUSDT,
	})
	a.RequiredWinRate = rwr

	degenerate := func(reason string) ROIAssessment {
		a.Feasible = false
		a.Reason = reason
		return a
	}
	if !in.Investment.IsPositive() {
		return degenerate(fmt.Sprintf("некорректный капитал %s USDT (≤0): цель не определена", in.Investment))
	}
	if in.Leverage < 1 {
		return degenerate(fmt.Sprintf("некорректное плечо %d (<1)", in.Leverage))
	}
	if in.GridNum < 1 {
		return degenerate(fmt.Sprintf("некорректное число уровней %d (<1)", in.GridNum))
	}
	if in.HoldHours <= 0 {
		return degenerate(fmt.Sprintf("некорректный горизонт проверки %.1f ч (≤0)", in.HoldHours))
	}

	notional := in.Investment.Mul(decimal.NewFromInt(int64(in.Leverage)))
	perLevel := notional.Div(decimal.NewFromInt(int64(in.GridNum)))
	rtPct := marketdata.RoundTripCostPct(in.FeeBps, in.SlippageBps) // % на цикл, обе ноги

	// Два яруса сбора: консервативный (вердикт) и оптимистичный (контекст).
	var harvestCons, harvestOpt decimal.Decimal
	var avgInventory decimal.Decimal
	var notes []string

	switch strings.ToUpper(strings.TrimSpace(in.Direction)) {
	case "LONG", "SHORT":
		if in.GridStepPct > 0 {
			a.EstCycles = in.RoomPctPct / in.GridStepPct * directionalLadderShare
		} else {
			notes = append(notes, fmt.Sprintf("шаг сетки %.2f%% (≤0): циклы лестницы не оценены", in.GridStepPct))
		}
		// Сбор: ход до препятствия × доля лестницы × нотационал, минус RT
		// на вход+выход (полный нотационал открывается и закрывается).
		gross := decimal.NewFromFloat(in.RoomPctPct).Div(decimal.NewFromInt(100)).
			Mul(decimal.NewFromFloat(directionalLadderShare)).Mul(notional)
		rtCost := decimal.NewFromFloat(rtPct).Div(decimal.NewFromInt(100)).Mul(notional)
		harvestCons = gross.Sub(rtCost)
		harvestOpt = harvestCons
		avgInventory = notional.Mul(decimal.NewFromFloat(directionalAvgInventoryShare))
		if in.RoomPctPct <= 0 {
			notes = append(notes, fmt.Sprintf("ход до структурного препятствия %.2f%% (≤0): собирать нечего", in.RoomPctPct))
		}
	default: // NEUTRAL — как stressInventoryLossUSDT: всё кроме LONG/SHORT
		avgInventory = notional.Mul(decimal.NewFromFloat(neutralAvgInventoryShare))
		// Сбор за цикл = (шаг − RT) на per-level нотционале.
		stepNetPct := decimal.NewFromFloat(in.GridStepPct).Sub(decimal.NewFromFloat(rtPct))
		cycleHarvest := func(cycles float64) decimal.Decimal {
			return decimal.NewFromFloat(cycles).
				Mul(stepNetPct).Div(decimal.NewFromInt(100)).Mul(perLevel)
		}
		switch {
		case in.GridStepPct <= 0:
			notes = append(notes, fmt.Sprintf("шаг сетки %.2f%% (≤0)", in.GridStepPct))
		case in.DayVolPct > 0:
			// Калибровка ресёрша волны C: циклы/день = k×σ_day/шаг.
			days := in.HoldHours / hoursPerDay
			a.EstCycles = cycleRateKConservative * in.DayVolPct / in.GridStepPct * days
			cyclesOpt := cycleRateKOptimistic * in.DayVolPct / in.GridStepPct * days
			harvestCons = cycleHarvest(a.EstCycles)
			harvestOpt = cycleHarvest(cyclesOpt)
		case in.OUHalfLifeHours > 0:
			// OU-fallback без σ_day: собственной калибровки нет — ярусы
			// совпадают (эвристика уже консервативна: утилизация×полсетки).
			sweeps := in.HoldHours / in.OUHalfLifeHours * neutralCycleUtilization
			a.EstCycles = sweeps * float64(in.GridNum) * neutralSweepLevelShare
			harvestCons = cycleHarvest(a.EstCycles)
			harvestOpt = harvestCons
		default:
			notes = append(notes, "нет ни дневной σ (DayVolPct ≤0), ни OU-полужизни (≤0): завершённые циклы не оцениваются, сбор 0")
		}
	}

	// Фандинг: ставка × 3 периода/сутки × горизонт, на средний инвентарь.
	// FundingBps8h==0 — «нет данных», не «бесплатно»: консервативные 5 bps/8ч.
	fundingBps := in.FundingBps8h
	if fundingBps == 0 {
		fundingBps = fundingFallbackBps8h
	}
	funding := avgInventory.
		Mul(decimal.NewFromFloat(fundingBps)).Div(decimal.NewFromInt(10000)).
		Mul(decimal.NewFromFloat(fundingPeriodsPerDay * in.HoldHours / hoursPerDay))

	// Стресс-хвост: ожидание полного прохода до стопа при требуемом винрейте.
	tail := rwr.Mul(in.StressLossUSDT)

	a.EstNetUSDT = harvestCons.Sub(funding).Sub(tail)
	netOpt := harvestOpt.Sub(funding).Sub(tail)

	target := ROITargetNetUSDT(in.Investment, profile)
	netOK := a.EstNetUSDT.GreaterThanOrEqual(target.Mul(decimal.NewFromFloat(feasibleNetShareOfTarget)))
	rwrOK := rwr.LessThanOrEqual(decimal.NewFromFloat(maxAcceptableRequiredWinRate))
	a.Feasible = netOK && rwrOK && len(notes) == 0

	// Оптимистичный ярус цитируем только когда он отличается от вердиктного.
	optimisticNote := ""
	if !harvestOpt.Equal(harvestCons) {
		optimisticNote = fmt.Sprintf(", оптимистичный ярус k=%.1f: %s USDT",
			cycleRateKOptimistic, netOpt.StringFixed(2))
	}

	if a.Feasible {
		a.Reason = fmt.Sprintf(
			"цель достижима: чистый сбор %s USDT ≥ %.0f%% порога цели %s USDT (профиль %s %.1f%%), требуемый винрейт %s ≤ %.2f: циклов %.1f, фандинг −%s, стресс-хвост −%s%s",
			a.EstNetUSDT.StringFixed(2), feasibleNetShareOfTarget*100, target.StringFixed(2),
			profile.Name, profile.Pct, rwr.StringFixed(3), maxAcceptableRequiredWinRate,
			a.EstCycles, funding.StringFixed(2), tail.StringFixed(2), optimisticNote)
		return a
	}

	var parts []string
	parts = append(parts, notes...)
	if !rwrOK {
		parts = append(parts, fmt.Sprintf(
			"требуемый винрейт %s выше потолка %.2f: стресс-проход −%s USDT против медианной победы класса %s USDT — для EV≥0 нужно побеждать чаще, чем допускает потолок",
			rwr.StringFixed(3), maxAcceptableRequiredWinRate,
			in.StressLossUSDT.StringFixed(2), in.AvgWinUSDT.StringFixed(2)))
	}
	if !netOK {
		parts = append(parts, fmt.Sprintf(
			"ожидаемый чистый сбор %s USDT ниже %.0f%% порога цели %s USDT (профиль %s %.1f%%): циклов %.1f, сбор %s, фандинг −%s, стресс-хвост −%s%s",
			a.EstNetUSDT.StringFixed(2), feasibleNetShareOfTarget*100, target.StringFixed(2),
			profile.Name, profile.Pct, a.EstCycles,
			harvestCons.StringFixed(2), funding.StringFixed(2), tail.StringFixed(2), optimisticNote))
	}
	a.Reason = strings.Join(parts, "; ")
	return a
}
