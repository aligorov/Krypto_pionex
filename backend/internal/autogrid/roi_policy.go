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
// сбора вычитаются RT-комиссия на каждый цикл и фандинг на средний инвентарь
// (при отсутствии данных о ставке — консервативные 0.15%/сутки, не ноль).
// Риск полного прохода до стопа входит в вердикт вероятностно: из сбора
// вычитается p×L, где p — доля стрессовых закрытий, измеренная по нашему
// журналу (stressTailProbability), а L — готовый стресс-проход геометрии.
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

	// stressTailProbability — вероятность стрессовой ветки (полный проход до
	// стопа) на ЖИЗНЕННЫЙ ЦИКЛ бота: 12 из 158 закрытых REAL-ботов нашего
	// журнала (testdata/calib_real_history, v2.0.97–v2.0.195) закрылись хуже
	// −3% внесённого капитала → 0.076. Медиана этого хвоста −7.08% капитала,
	// экстремум −15.8% (PENGU, STOP_LOSS_PRICE_BREACH, −23.72 USDT); весь флот
	// при этом −50.44 USDT (EV −0.43% на USDT капитала) — убыток сделан
	// хвостом, а не средним циклом.
	//
	// v2.0.197 (ресёрч «как правильно»): риск в вердикте — вероятностная
	// величина p×L, а не отношение худшего случая к среднему. Прежний барьер
	// «требуемый винрейт L/(G+L) ≤ 0.75» (⟺ L ≤ 3G) нашим же журналом
	// опровергается: у 96 прибыльных закрытий медиана отношения
	// спроектированного стопа к реализованной прибыли 19.8 (p25 10.4, p75
	// 32.8), то есть барьер 3:1 отверг бы 96% наших собственных победителей.
	// Кап «L ≤ 10% капитала» тоже не калибруется: по корзинам L/капитал
	// среднее pnl% равно −0.40% (0–10%, n=81), +0.25% (10–20%, n=29), −0.28%
	// (20–30%, n=47) — узкий стоп не даёт лучшего исхода, а единственный
	// катастрофический исход (−15.8%) пришёлся на L = 90% капитала, который
	// и так отсекает v2.0.139 tranche2MaxLossCap. Дублировать тот кап другим
	// числом — рассинхрон доктрины, поэтому здесь остаётся только p×L.
	stressTailProbability = 0.076

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

// ───────────── Калибровка циклов: local time, а не линейный k ─────────────
//
// v2.0.197 (ресёрч «как правильно»): прежняя модель циклов/день = k×σ_day/шаг
// (ярусы k=2.5/2.0) не выдержала ни теоретической, ни эмпирической проверки.
//   - Теория. Число пересечений уровня броуновским движением растёт как
//     локальное время (Lévy), то есть как σ²/δ², а не как σ/δ: линейная
//     зависимость от волатильности при фиксированном шаге не имеет под собой
//     модели. Для диапазона из n = span/шаг шагов полное число round-trip до
//     выхода из диапазона — n²/8 (gambler's ruin: E₀ = n²/4 шагов, Chen 2025,
//     arXiv 2506.11921), что при span = 2σ_day (доктрина спана v2.0.183)
//     совпадает с ½(σ_day/шаг)² на сутки. Ссылка на arXiv 2506.11921 в старом
//     комментарии была ложной атрибуцией: в статье нет ни local time, ни
//     «потолка k≈7.8»; цифра практики «сбор 0.15–0.6%/день» восходила к
//     непроверяемой маркетинговой странице.
//   - Эмпирика. Прогон обеих моделей по нашим 158 закрытым REAL-ботам
//     (testdata/calib_real_history, 96 прибыльных закрытий): медиана отношения
//     «реализованный сбор / модель» у линейной модели 2.42 (систематическое
//     занижение в ~2.4×), у квадратичной local-time модели 1.02 — медианно
//     несмещённая (p25 0.45, p75 1.75, p90 3.00, mean 1.46).
//
// Поэтому циклы = утилизация × ½(σ_day/шаг)² × сутки, утилизация = 1.0
// (медианно несмещённая по нашей же истории), а p25/p75 цитируются в Reason
// как полоса дисперсии, не как «оптимистичный ярус».
const (
	// cycleUtilizationMedian — утилизация вердикта: калибрована по медиане
	// отношения «реализовано/модель» = 1.02 на 96 прибыльных закрытиях.
	cycleUtilizationMedian = 1.0

	// cycleUtilizationP25 / cycleUtilizationP75 — квартили того же отношения:
	// полоса дисперсии для Reason. Вердиктом НЕ управляют (для запаса
	// служит feasibleNetShareOfTarget).
	cycleUtilizationP25 = 0.45
	cycleUtilizationP75 = 1.75
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

// ROIEconomics — экономика бинарного исхода «победа G / стресс-проход L».
type ROIEconomics struct {
	// StressLossUSDT — полный стресс-проход до стопа в USDT (L): ГОТОВОЕ
	// число из stressGeometry/stressInventoryLossUSDT, здесь не
	// пересчитывается.
	StressLossUSDT decimal.Decimal
	// AvgWinUSDT — победа (G). В AssessROI это собственный чистый сбор
	// геометрии за горизонт; отдельной «медианы побед класса» модуль не
	// знает и не требует (см. ROIAssessment.RequiredWinRate).
	AvgWinUSDT decimal.Decimal
}

// RequiredWinRate — бинарная нижняя граница побед для EV≥0:
// p×G − (1−p)×L ≥ 0  ⟺  p ≥ L/(G+L).
// Вырожденные случаи: стресс ≤ 0 → 0 (хвоста нет — окупается любой винрейт);
// победа ≤ 0 при стрессе > 0 → 1 (EV≥0 недостижим ни при каком винрейте < 1).
//
// v2.0.197: граница осталась КОНТЕКСТОМ (журнал деплоя пишет ключ
// required_win_rate), но вердиктом больше не управляет — см.
// maxStressLossShareOfCapital.
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
	GridStepPct float64 // шаг уровня, %
	Leverage    int

	// FeeBps/SlippageBps — издержки на ногу; RT-комиссия = 2×(fee+slip),
	// единая ось правды с marketdata.RoundTripCostPct.
	FeeBps, SlippageBps float64

	StressLossUSDT decimal.Decimal // готовый стресс-проход до стопа (из stressGeometry)
	FundingBps8h   float64         // текущая ставка фандинга, bps за 8 ч; 0 = нет данных → консервативные 5 bps/8ч
	HoldHours      float64         // горизонт проверки 24-72 ч
	// DayVolPct — дневная σ пары, %. ОБЯЗАТЕЛЬНА для NEUTRAL: модель циклов
	// квадратична по σ (local time), поэтому без σ оценки нет вовсе, а
	// fallback на OU-полужизнь удалён (см. AssessROI — v2.0.197).
	DayVolPct  float64
	RoomPctPct float64 // LONG/SHORT: ход до структурного препятствия, %
}

// ROIAssessment — оценка достижимости цели (НЕ решение: deploy решает воркер).
type ROIAssessment struct {
	Feasible bool
	Reason   string // человекочитаемо, по-русски, с числами
	// EstCycles — ожидаемое число завершённых сеточных циклов (уровневых
	// round-trip) за HoldHours при медианной утилизации:
	// NEUTRAL = ½(σ_day/шаг)²×утилизация×сутки (local-time модель, см.
	// константы cycleUtilization*); LONG/SHORT = (ход/шаг)×доля лестницы.
	EstCycles float64
	// EstNetUSDT — ожидаемый ЧИСТЫЙ сбор медианной утилизации ДО вычета
	// хвоста: циклы×(шаг−RT)×нотационал/уровень − фандинг (для LONG/SHORT —
	// ход×лестница×нотационал − RT на вход+выход). Полоса p25–p75 того же
	// сбора цитируется в Reason.
	EstNetUSDT decimal.Decimal
	// RiskAdjustedUSDT — число, по которому стоит вердикт:
	// EstNetUSDT − stressTailProbability×StressLossUSDT (см. EstNetUSDT и
	// stressTailProbability — почему вероятностно, а не отношением L/G).
	RiskAdjustedUSDT decimal.Decimal
	// RequiredWinRate — требуемая частота побед для EV≥0: L/(G+L), где
	// G — СОБСТВЕННЫЙ чистый сбор этой геометрии за горизонт, L — полный
	// стресс-проход до стопа. G ≤ 0 (сбор не покрывает издержек) даёт 1.
	// КОНТЕКСТ для журнала деплоя, вердиктом не управляет (v2.0.197).
	//
	// v2.0.196 (аудит): раньше поле считалось от «медианной победы класса»,
	// которую вызывающий передавал отдельно, — и оба места передачи
	// (REAL и PAPER) подставляли замороженный литерал 0.48 USDT. Он не
	// масштабируется ни с капиталом, ни с целью: граница L/(0.48+L) ≤ 0.75
	// требует стресс-прохода ≤ 1.44 USDT, то есть гейт отклонял практически
	// любого кандидата на любом реальном бюджете (прод-разбор v2.0.195
	// сам цитировал 78% для бота с L≈31 — это число получается именно при
	// G≈8.8, то есть от сбора, а не от 0.48). Единственная честная победа
	// класса, доступная модулю, — его же оценка сбора за горизонт, поэтому
	// вход AvgWinUSDT удалён, а отдельное «покрытие стопа» (LossCoverWinRate,
	// тот же порог 0.75 от EstNet) слито с этим полем: две одинаковые
	// границы с разными G не давали ни запаса, ни внятной причины отказа.
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
// финальной сетке; решениЙ не принимает.
//
// NEUTRAL, циклы: ½(σ_day/шаг)² × утилизация × сутки (local-time модель,
// медианно несмещённая по нашим 96 прибыльным закрытиям; полоса p25–p75 —
// контекст в Reason). σ_day ≤ 0 — оценки нет вовсе (Feasible=false с
// причиной): v2.0.197 удалила fallback на OU-полужизнь, потому что
// «проходы × gridNum × 0.5» не был калиброван ни по теории, ни по журналу,
// а невалидированная модель в гейте деплоя хуже честного отказа.
//
// Сбор за цикл = (шаг − RT) на per-level нотационале
// (investment×leverage/gridNum).
//
// LONG/SHORT: сбор = ход до препятствия × доля лестницы 0.5 × нотационал −
// RT-комиссия на вход+выход полного нотационала; циклы = ход/шаг×0.5.
//
// Издержки всегда: RT-комиссия на каждый цикл (в шаге для NEUTRAL, отдельной
// ногой для LONG/SHORT); фандинг FundingBps8h×3×HoldHours/24 на средний
// инвентарь (0.5×нотационала для NEUTRAL, полный для LONG/SHORT) СО ЗНАКОМ
// ставки — положительная ставка означает «лонги платят шортам», поэтому
// SHORT её получает; при FundingBps8h==0 (нет данных) — консервативные
// 5 bps/8ч (0.15%/сутки) как стоимость, не ноль.
//
// Стресс-проход до стопа входит в вердикт вероятностно: RiskAdjustedUSDT =
// EstNetUSDT − p×L, p = stressTailProbability (доля стрессовых закрытий,
// измеренная по нашему журналу). Прежние варианты были либо арифметически
// неверны, либо не калибруемы: «минус rwr×L» умножал величину убытка на
// вероятность ПОБЕДЫ, «минус (1−rwr)×L» на границе тождественно обнулял EV
// класса, а барьер L ≤ 3G нашим же журналом опровергается (медиана отношения
// стопа к прибыли у победителей 19.8 — см. stressTailProbability).
//
// Feasible = RiskAdjustedUSDT ≥ TargetNetUSDT×0.8; вырожденная геометрия
// (σ_day ≤ 0, шаг/ход ≤ 0 и т.п.) — всегда Feasible=false с причиной. Иначе
// причина содержит числа. Кап худшего исхода здесь СОЗНАТЕЛЬНО не дублирует
// v2.0.139 tranche2MaxLossCap — тот уже отсекает геометрию, концентрирующую
// риск выше бюджета слота.
func AssessROI(in ROIAssessmentInput, profile ROIProfile) ROIAssessment {
	a := ROIAssessment{Profile: profile}

	// Граница безубыточности L/(G+L) считается от собственного чистого сбора
	// геометрии, поэтому зависит от оценки и пересчитывается в каждом выходе
	// (в вырожденном случае сбор нулевой → граница 1 при живом стрессе).
	winRateBound := func(net decimal.Decimal) decimal.Decimal {
		return RequiredWinRate(ROIEconomics{
			StressLossUSDT: in.StressLossUSDT,
			AvgWinUSDT:     net,
		})
	}
	a.RequiredWinRate = winRateBound(decimal.Zero)

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

	// Сбор медианной утилизации (вердикт) и полоса p25–p75 (контекст в Reason).
	var harvest, harvestLow, harvestHigh decimal.Decimal
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
		harvest = gross.Sub(rtCost)
		harvestLow, harvestHigh = harvest, harvest
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
			// Local-time модель: циклы/сутки = утилизация × ½(σ_day/шаг)².
			// См. константы cycleUtilization* — почему квадрат, а не k×σ/шаг.
			days := in.HoldHours / hoursPerDay
			quad := func(u float64) float64 {
				ratio := in.DayVolPct / in.GridStepPct
				return u * 0.5 * ratio * ratio * days
			}
			a.EstCycles = quad(cycleUtilizationMedian)
			harvest = cycleHarvest(a.EstCycles)
			harvestLow = cycleHarvest(quad(cycleUtilizationP25))
			harvestHigh = cycleHarvest(quad(cycleUtilizationP75))
		default:
			// v2.0.197: OU-fallback удалён — он не был калиброван ни по
			// теории, ни по журналу. Без σ_day честного ответа нет.
			notes = append(notes, fmt.Sprintf(
				"дневная σ пары %.4f%% (≤0): завершённые циклы не оцениваются, сбор 0", in.DayVolPct))
		}
	}

	// Фандинг: ставка × 3 периода/сутки × горизонт, на средний инвентарь.
	// FundingBps8h==0 — «нет данных», не «бесплатно»: консервативные 5 bps/8ч.
	// Знак: ставка SIGNED (конвенция fundingAccrual — положительная означает
	// «лонги платят шортам»), поэтому SHORT на положительной ставке фандинг
	// ПОЛУЧАЕТ; прежнее слепое вычитание стоимости занижало сбор шортов и
	// завышало его на отрицательной ставке. NEUTRAL тарифицируется как
	// плательщик: его инвентарь ниже середины диапазона длинный (та же
	// конвенция, что fundingPays в paper-маркировке).
	fundingBps := in.FundingBps8h
	fundingKnown := fundingBps != 0
	if !fundingKnown {
		fundingBps = fundingFallbackBps8h
	}
	costBps := fundingBps
	if fundingKnown && strings.ToUpper(strings.TrimSpace(in.Direction)) == "SHORT" {
		costBps = -fundingBps
	}
	funding := avgInventory.
		Mul(decimal.NewFromFloat(costBps)).Div(decimal.NewFromInt(10000)).
		Mul(decimal.NewFromFloat(fundingPeriodsPerDay * in.HoldHours / hoursPerDay))

	// Чистая оценка горизонта = сбор − удержание.
	//
	// v2.0.197: хвост вычитается вероятностно — p×L, где p измерено по нашему
	// журналу (stressTailProbability), а L — готовый стресс-проход геометрии.
	// Прежние варианты: «минус rwr×L» умножал убыток на вероятность ПОБЕДЫ,
	// «минус (1−rwr)×L» на границе тождественно обнулял EV класса, а барьер
	// L ≤ 3G отвергал 96% наших собственных прибыльных закрытий (медиана
	// отношения стопа к прибыли у победителей 19.8).
	a.EstNetUSDT = harvest.Sub(funding)
	tail := in.StressLossUSDT.Mul(decimal.NewFromFloat(stressTailProbability))
	if tail.IsNegative() {
		tail = decimal.Zero
	}
	a.RiskAdjustedUSDT = a.EstNetUSDT.Sub(tail)
	netLow := harvestLow.Sub(funding).Sub(tail)
	netHigh := harvestHigh.Sub(funding).Sub(tail)

	target := ROITargetNetUSDT(in.Investment, profile)
	threshold := target.Mul(decimal.NewFromFloat(feasibleNetShareOfTarget))
	netOK := a.RiskAdjustedUSDT.GreaterThanOrEqual(threshold)
	// Контекст журнала: граница безубыточности бинарного исхода L/(G+L).
	a.RequiredWinRate = winRateBound(a.EstNetUSDT)
	rwr := a.RequiredWinRate
	a.Feasible = netOK && len(notes) == 0

	// Полосу p25–p75 цитируем только когда она не вырождается в точку
	// (LONG/SHORT: утилизация на лестницу не действует).
	bandNote := ""
	if !harvestLow.Equal(harvestHigh) {
		bandNote = fmt.Sprintf(", полоса p25–p75: %s…%s USDT",
			netLow.StringFixed(2), netHigh.StringFixed(2))
	}
	fundingNote := fmt.Sprintf("фандинг −%s", funding.StringFixed(2))
	if funding.IsNegative() {
		fundingNote = fmt.Sprintf("фандинг +%s (доход шорта)", funding.Abs().StringFixed(2))
	}
	tailNote := fmt.Sprintf("хвост −%s (%.1f%% × стресс %s)",
		tail.StringFixed(2), stressTailProbability*100, in.StressLossUSDT.StringFixed(2))

	if a.Feasible {
		a.Reason = fmt.Sprintf(
			"цель достижима: сбор %s USDT, %s → риск-скорректировано %s USDT ≥ порога %s USDT (%.0f%% цели %s, профиль %s %.1f%%): циклов %.1f, %s, требуемый винрейт %s%s",
			a.EstNetUSDT.StringFixed(2), tailNote, a.RiskAdjustedUSDT.StringFixed(2),
			threshold.StringFixed(2), feasibleNetShareOfTarget*100, target.StringFixed(2),
			profile.Name, profile.Pct, a.EstCycles, fundingNote, rwr.StringFixed(3), bandNote)
		return a
	}

	var parts []string
	parts = append(parts, notes...)
	if !netOK {
		parts = append(parts, fmt.Sprintf(
			"риск-скорректированный сбор %s USDT ниже порога %s USDT (%.0f%% цели %s, профиль %s %.1f%%): чистый сбор %s, %s, циклов %.1f, сбор %s, %s%s",
			a.RiskAdjustedUSDT.StringFixed(2), threshold.StringFixed(2),
			feasibleNetShareOfTarget*100, target.StringFixed(2),
			profile.Name, profile.Pct, a.EstNetUSDT.StringFixed(2), tailNote, a.EstCycles,
			harvest.StringFixed(2), fundingNote, bandNote))
	}
	a.Reason = strings.Join(parts, "; ")
	return a
}
