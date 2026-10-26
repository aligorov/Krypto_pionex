# Оценка Krypto_pionex: ошибки, результаты ботов и план восстановления

Дата: 5 октября 2026. Проверяемый commit: `2f4172082a4f0c74cf329a54c49cc479ffbc4729`. Работающий backend: `v2.0.190`, тот же commit.

## Короткий вывод

Проект имеет полезную инженерную основу, но положительное математическое ожидание торговли не подтверждено. В доступной истории боты теряют деньги; есть воспроизводимые ошибки модели исполнения и действующие дефекты аналитики решений. Увеличивать плечо, бюджет или число ботов сейчас — усиливать непроверенную систему.

Главная последовательность исправления: **достоверный учёт → безопасность последнего шага создания бота → единая модель исполнения → журнал решений и контрольная группа → проверка входов на новых данных → ограниченный REAL**. Простое ужесточение индикаторов или включение нынешнего backtest gate не решает проблему.

Это анализ и план, не реализация исправлений. Торговый код, PostgreSQL-настройки, ключи, флаги и живые боты не изменялись.

## 1. Что проверено и что остаётся неизвестным

- Graphify использован первым для навигации по проекту: 3 509 узлов, 8 646 связей, 165 сообществ, commit совпадает. Graphify MCP в этой сессии не предоставлен, поэтому использован установленный CLI. Структурный граф не доказывает полноту анализа и не заменяет чтение кода.
- Изучены scanner, shared entry chain, REAL/PAPER deploy, dynamic targets, stop/exit management, native lifecycle, Pionex HTTP-клиент, risk engine, equity/PnL, Python backtest, shadow/replay/gate quality, миграции и соответствующий frontend.
- Через read-only MCP прочитаны состояние работающей системы, история ботов, бэктестов, решений, теневых расчётов и журналы. Срезы получены приблизительно 20:15–20:24 UTC, то есть 23:15–23:24 МСК. Это несколько запросов, не единая транзакционная фотография; текущие счётчики могут расти.
- Проверены актуальные официальные определения полей и состояний [Pionex Futures Grid API](https://www.pionex.com/docs/api-docs/bot-api/futures-grid). Проверка не означает контрактную сертификацию всех API-вызовов проекта.
- Выполнены локальные проверки без выставления ордеров и без записи в рабочую БД.

| Проверка | Результат | Ограничение |
|---|---|---|
| `go test ./...`, backend | Пройдено | Интеграционные проверки БД пропускаются без отдельной тестовой БД |
| Пакет autogrid | 275 passed, 126 skipped, 0 failed | Это не 401 выполненная проверка |
| `go vet ./...` | Пройдено | Не обнаруживает семантику строк SQL и торговый edge |
| Frontend build | TypeScript/Vite: пройдено | Есть предупреждение о bundle >500 kB; не причина торговых потерь |
| Python | 16 существующих test-функций напрямую и 1 unittest: пройдено | Стандартный запуск pytest недоступен: модуль не установлен |
| Синтетические проверки модели | Два дефекта воспроизведены | Прохождение существующих тестов их не исключает |

Не получен независимый полный журнал биржевых fills, денежных переводов и cash-flow-adjusted equity. Поэтому ниже — анализ записей проекта, не заверенная выписка Pionex и не точная доходность счёта. Причины потерь разделены на наблюдаемые факты, доказанные дефекты кода и гипотезы, требующие нового эксперимента.

У risk engine сейчас нет собственных test-файлов. Аудит не является penetration test, проверкой всей инфраструктуры развёртывания или гарантией отсутствия остальных дефектов.

## 2. Текущее состояние

На последней проверке: БД исправна, активных аккаунтов 1, живых REAL grid-ботов 0, открытых pattern orders 0, pending commands 0. AutoGrid работает в `PAPER`; возможность REAL native grid включена, kill switch выключен. `backtest_gate=false`, последнее изменение флага — 1 октября, 17:13 UTC / 20:13 МСК.

В настройках AutoGrid: базовое плечо 3 с адаптацией, максимум 10 ботов, резерв 0%, DYNAMIC targets, STRICT radar, ADAPTIVE_ATR stop, candle 15M, сканирование 240 секунд, управление 45 секунд. Установлены `min_risk_reward=1.8`, `max_drawdown=8`, `min_profit_factor=1.05`. Эти названия не означают, что соответствующие ограничения одинаково применяются к окончательному биржевому боту и реальному счёту.

В risk settings: дневной лимит потерь 375 USDT, account exposure 10 000, symbol exposure 1 000, максимальное плечо 10, максимум 20 grid-ботов. Последняя доступная сумма финансирования native-ботов — около 642.82 USDT. Лимит 375 составляет около 58.3% этой суммы: это не выглядит защитным бюджетом для небольшого эксперимента. Сравнение не является исторической доходностью или расчётом futures margin: доступное финансирование из основного/Spot-аккаунта и маржа открытого futures-бота — разные величины.

Рекомендация до исправлений: оставаться в PAPER и не разрешать ручные REAL-запуски. Ничего из этого автоматически не переключалось.

## 3. Что произошло с реальными ботами

### 3.1. Выборка и неопределённость учёта

В `grid_bots` 465 REAL-попыток: 225 FAILED без remote ID, 228 STOPPED и 12 COMPLETED. Из 240 завершённых ботов с remote ID у 28 отсутствует сохранённый final PnL; известный результат есть у 212. Известные закрытия — 3 сентября–2 октября; 28 неизвестных относятся к ранней истории августа.

FAILED — не 225 установленных убыточных сделок. Среди исторических ошибок создания: 118 BOT_INTERNAL_ERROR (последняя 12 сентября), 70 balance insufficient (26 сентября), 5 top precision failures (30 сентября), 4 WTI symbol failures (27 сентября), 4 less-than-min-investment (29 сентября), а также maintenance/forbidden. Это отдельный operational failure rate: сначала валидировать доступность инструмента, размер/precision и резервацию средств. Старые ошибки могли быть исправлены последующими версиями; их наличие не доказывает текущую регрессию. Internal errors дополнительно требуют проверки uncertain outcome, как описано в F11.

Источники этих 212 результатов: 171 `unlock_identity` и 41 `telemetry_net_close`. Первый — восстановление результата через возвращённые средства, второй — локальная оценка по телеметрии. Ни один из этих ярлыков сам по себе не доказывает полную сверку всех денежных потоков с биржей.

| Метрика, 212 известных закрытий | Сохранённый final | По текущей net-формуле проекта |
|---|---:|---:|
| Суммарный PnL, USDT | −78.68 | −105.59 |
| Историческое среднее на бота, USDT | −0.371 | −0.498 |
| Win rate, включая нулевые результаты в знаменателе | 58.96% | 49.06% |
| Profit factor | 0.544 | 0.425 |
| Средний положительный результат, USDT | +0.752 | +0.752 |
| Средний отрицательный результат, USDT | −2.365 | −1.702 |

Net-колонка повторяет [текущий equity accounting](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/equity.go:166):

```text
project_net = stored_final − (fees_paid_usdt − closeCostUsdt)
```

Дополнительное вычитание для этих 212 строк — 26.9115 USDT. Нельзя просто вычесть все комиссии: стоимость закрытия может уже входить в final. Однако и нынешнюю net-формулу нельзя объявлять точным изменением кошелька без сверки исходных биржевых cash flows, entry fees, инвестиций/добавлений/выводов и funding. Неизвестные 28 результатов в таблицу не подставлены нулями.

**Практическое объяснение неудачи:** частые небольшие выигрыши перекрываются более крупными потерями. В сохранённом final средний проигрыш примерно в 3.15 раза больше среднего выигрыша. Win rate около 59% не спасает такую структуру выплат.

| Направление | Ботов | Сохранённый final, USDT | Project net, USDT | Net PF | Вывод |
|---|---:|---:|---:|---:|---|
| NEUTRAL | 190 | −48.64 | −73.33 | 0.503 | Основной объём стратегии убыточен |
| LONG | 12 | −31.92 | −32.94 | 0.043 | Самая плохая группа; выборка мала и потери концентрированы |
| SHORT | 10 | +1.88 | +0.68 | 1.360 | Недостаточно данных для признания положительного edge |

Последние 37 известных закрытий с 1 октября: final −37.38, project net −43.47, net PF 0.202. Это ухудшение наблюдаемого результата, но не причинное доказательство вреда конкретной последней правки.

### 3.2. Крупный эпизод 2 октября

За UTC-день 2 октября закрыто 18 ботов: final −41.03, project net −43.64 USDT.

| Бот | Направление | Investment / leverage | Final | Цель / max loss | Зафиксированный peak PnL |
|---|---|---|---:|---:|---:|
| #1545 PENGU | LONG | 150 / 4x | −23.72 | 70.18 / 45.02 | +0.94 |
| #1546 MSTRX | LONG | 75 / 4x | −8.48 | 25.20 / 16.13 | +2.19 |
| #1537 ZAMA | NEUTRAL | 150 / 4x | −7.05 | 3.00 / 30.00 | +1.05 |

Два LONG дали −32.20 USDT, около 78.5% отрицательного дневного final. Они запущены 2 октября около 09:18 UTC / 12:18 МСК, закрыты около 17:44 UTC / 20:44 МСК.

В [manage.go](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/manage.go:219) directional trailing вооружается только от 75% цели: для этих LONG это примерно 52.64 и 18.90 USDT. Зарегистрированные пики существенно ниже. Следовательно, именно этот trailing-механизм в данных эпизодах не мог включиться. Это не доказывает, что другой trailing обязательно дал бы прибыль; показывает несоответствие масштаба цели масштабу наблюдаемого движения.

Группировки по причине закрытия нельзя использовать как готовую стратегию: прибыльность `GRID_AGED_HALF_LIFE` и убыточность стопов частично обусловлены самим выбором групп по исходу. Нужен эксперимент «одинаковый вход, разные правила выхода», а не удаление стопов.

### 3.3. PAPER не подтверждает исправление

С 2 октября 28 завершённых PAPER: −5.88 USDT по сохранённым simulated outcomes. NEUTRAL: 22 бота, +19.10; LONG: 2, −12.83; SHORT: 4, −12.16. На момент среза ещё один PAPER работал; его результат не прибавлен к закрытым.

Положительные PAPER NEUTRAL — только гипотеза. Четыре отрицательных PAPER SHORT — дополнительная причина не делать вывод «достаточно торговать только SHORT». Модели PAPER и backtest сейчас различаются.

### 3.4. Метрики, которые нельзя честно дорисовать

- **Max drawdown счёта:** не установлен. Максимальное снижение накопленной суммы известных закрытых результатов составляет 78.68 USDT по final или 105.59 по project net; это не MTM drawdown счёта и не процент от первоначального капитала.
- **Sharpe/Sortino REAL:** не установлен. Equity snapshots меняли источник, отсутствует полная нормализация вводов/выводов; считать ratio по такой кривой некорректно.
- **Turnover REAL:** не установлен без executed notional fills. Сумма инвестиций в боты не является оборотом торговли.
- **Liquidity constraints:** ограничители в коде есть, но Python-проверка допускает нулевой объём, а свечной объём не доказывает исполнимость размера заявки в текущем стакане.
- **Expected value:** −0.371/−0.498 — исторические средние данной неоднородной выборки, не прогноз следующего бота.

## 4. Подтверждённые дефекты и риски реализации

Приоритет P1 — закрыть до возврата REAL; P2 — необходим для качественного исследования/эксплуатации. «Дефект доказан» не означает «доказано, сколько именно USDT он отнял».

### F01 · P1 · Shadow не связывается с решением: действующая SQL-ошибка

[shadow_portfolio.go:116](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/shadow_portfolio.go:116): `UPDATE … FROM … WHERE … WHERE sc.decision_id IS NULL`. Второй `WHERE` делает SQL недопустимым. Последние рабочие логи 5 октября подтверждают PostgreSQL `42601: syntax error at or near WHERE`.

Состояние БД: 1 466 shadow rows, **0 linked decision_id**. Ошибка не теоретическая. Исправление: корректный предикат в одном WHERE, устойчивый ключ связи с окончательным решением, интеграционный тест и отдельная метрика потерянных связей. Старые строки связывать только при однозначном совпадении, не угадывать.

### F02 · P1 · Shadow пишет legacy outcome, но не новое состояние

[shadow_portfolio.go:584](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/shadow_portfolio.go:584) записывает `simulated=true`, outcome и legacy-поля, но не закрывает новые `pos_state/calc_state`. Миграция задаёт `OPEN/PENDING`. [gate_quality.go:335](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/gate_quality.go:335) учитывает результаты с `calc_state=DONE` и закрытым position state.

В БД 1 290 уже simulated rows остаются `OPEN/PENDING`; 1 287 имеют результат. Получается расчёт без пригодного для новой аналитики outcome. Исправление: единая атомарная запись завершения с причиной, временем, состояниями, компонентами PnL и качеством данных; не менять состояние на DONE при неполном результате. Исторические записи должны сохранять метку proxy/ограничения модели.

### F03 · P1 · Журнал не позволяет воспроизвести полный вход

[entry_chain.go:552](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/entry_chain.go:552) пишет single-gate trace и по умолчанию INTERMEDIATE, но не configuration snapshot, направления и цену. [worker.go:3997](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:3997) также пишет одиночные поздние отказы без полного config/chain.

Срез после включения decision intelligence: 4 214 решений, все без config snapshot; 0 FINAL, 0 multigate traces. REAL — 2 597, из них 15 ALLOW, но тоже нет FINAL/full config. Во всех этих REAL-строках отсутствует direction_after, в 2 203 — price_at_decision.

Единственный replay RUN имеет DONE, но `full_chain_available=0`, `outcomes_available=0`, `chain_not_recorded=835`, overrides пуст. DONE означает завершение процесса, не доказательство воспроизводимости и не оценку новой стратегии.

Исправление: один FINAL decision после всех gate/mutations, полный trace с PASS/REJECT/NOT_EVALUATED и timestamps доступности данных; immutable DB config snapshot; итоговый ExecutionSpec; связь с bot/shadow. Промежуточные записи не выдавать за окончательный допуск.

### F04 · P1 · Положительный pair EV ошибочно используется как доказательство прибыльности бота

[backtest.py:677](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:677): EV, win rate и CI рассчитываются по завершённым grid-парам/событиям. Убыток незакрытого инвентаря, funding и часть входных затрат не представлены как такие же результаты. `final_equity` при обычном окончании окна сохраняет MTM открытой позиции, без обязательного ликвидационного закрытия и его затрат.

Из 597 DONE результатов v173: **108 имеют positive EV при negative OOS return**, 105 — win rate 100% при negative OOS return. У 10 одновременно sample_sufficient=true, positive CI и OOS return между −1% и 0%. Пример XPL/30M: OOS −0.7589%, pair EV +0.0043, positive CI, sample sufficient. Это пример противоречия метрик, не доказательство фактического запуска именно этого кандидата.

Go gate [backtest.go:26](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/backtest.go:26) допускает OOS выше −1%, поэтому такой результат не блокируется только из-за отрицательной итоговой доходности. Сам gate сейчас выключен.

Исправление: отдельно `grid_pair_ev` и `bot_net_outcome`; admission должен опираться на итоговую прибыль после всех legs/costs и на устойчивость по независимым эпизодам. CI по автокоррелированным grid-парам не заменяет CI по независимым ботам/дням. Просто включать нынешний gate преждевременно.

### F05 · P1 · «Exact OOS» оценивает свежие параметры на прошлом

[backtest.py:588](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:588) применяет текущие `deployed_params` к хвосту исторических свечей после первого train+purge. Если эти параметры выбраны из последних свечей того же диапазона, оценка не является forward OOS относительно момента выбора кандидата.

Исторические walk-forward folds действительно derive параметры из train, но это другая политика/геометрия; они не превращают проверку свежего directional-кандидата в честный OOS.

Исправление: для исследовательского walk-forward выбирать всю политику только по прошлому каждого fold; для live-кандидата заморозить spec в момент решения и считать будущий outcome. Старый exact replay назвать historical scenario, не доказательством отсутствия look-ahead.

### F06 · P1 · Backtest, PAPER и native execution описывают разные боты

- [BacktestDeployParams](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/backtest.go:92) не передаёт grid type, actual lower/upper stop prices, neutral upper stop и полный TP/exit policy.
- [worker.go:3370](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:3370) передаёт stop distance относительно entry; [backtest.py:264](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:264) повторно откладывает эту дистанцию от lower/upper. Entry=100, lower=96, intended stop=94 → переданные 6% дают Python stop 90.24, а не 94.
- Python grid spacing arithmetic; native может быть geometric. Python NEUTRAL использует BUY/SELL long-like механику и не моделирует отдельно short inventory выше середины и neutral upper stop.
- [Python:251](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:251) стартует LONG/SHORT с половины notional, [PAPER:486](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/manage.go:486) — с полного. Реальный начальный объём native нужно измерять по ответам Pionex, не выбирать одну модель только по комментарию к коду.

Pionex явно различает arithmetic/geometric, long/short/no_trend, `lossStop` и `lossStopHigh`, а также цены и суммы в stop types. [Официальная схема](https://www.pionex.com/docs/api-docs/bot-api/futures-grid).

Исправление: один versioned ExecutionSpec для live/PAPER/backtest/shadow, с явными единицами, абсолютными стопами и моделью initial inventory; контрактные fixtures с биржевыми наблюдениями. OHLCV proxy всё равно не станет точной копией очереди исполнения биржи.

### F07 · P1 · Кэш «exact params» не проверяет значительную часть params

[matchesDeployParams](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/backtest.go:334) сравнивает range с допуском 0.2%, levels и leverage при наличии положительного значения. Не сравнивает direction, investment, stop, fees/slippage и grid type. Поэтому подходящий по range LONG-result может обслужить SHORT-запрос; отсутствующее плечо не всегда делает кэш непригодным.

Исправление: canonical spec hash + data window/as-of + execution model version + fee/funding model version. Approximate geometry допустима как отдельный sensitivity scenario, не exact validation. Любой недостающий обязательный параметр означает cache miss.

### F08 · P1 · Двойное списание opening maker fee

[backtest.py:346](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:346) списывает fee при открытии lot, а при закрытии снова включает entry fee в fee и realized. Аналогично для SHORT.

Воспроизведение: один закрытый lot, maker fee 0.0002, taker/slippage/funding=0. Получается `fees_paid=0.00604` вместо 0.00404; realized=0.19396 вместо 0.19596. Это пессимистическая, не оптимистическая ошибка, но она нарушает accounting identity и сравнимость моделей.

Исправление: однократное списание каждого execution fee; отдельно PnL пары и cash ledger; тест обеих сторон и partial fills. Синтетическая проверка приложена ниже.

### F09 · P1 · Нулевой объём проходит liquidity gate

[backtest.py:757](/Users/aleksey/Documents/Krypto_pionex/quant-worker/engine/backtest.py:757) начинает с `liquidity_ok=true` и проверяет размер только при `avg_candle_vol>0`. Volume не ограничивает fills в движке.

500 синтетических свечей с volume=0 дают `liquidity_ok=true`, reason=OK и 3 270 round trips. Отсутствующие данные трактуются как возможность исполнения.

Исправление: EMPTY/STALE/INVALID liquidity data — отдельный отказ/unknown; обязательны реальный стакан Pionex, лимит участия, влияние на цену, spread и модель частичного исполнения. Свечная volume-check остаётся только грубой предварительной оценкой.

### F10 · P1 · Последний risk/kill guard расположен слишком рано

[worker.go:3129](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:3129) проверяет risk до backtest. При cache miss [backtest.go:443](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/backtest.go:443) ждёт до 75 секунд на timeframe; основной и два соседних могут последовательно задержать путь примерно на 225 секунд.

[LifecycleManager.CreateGridBot](/Users/aleksey/Documents/Krypto_pionex/backend/internal/grid/lifecycle.go:82) непосредственно перед create не перечитывает durable risk/kill. Transactional duplicate guard ограничен account+symbol, не общей атомарной account-risk reservation. Возможны устаревший допуск после включения kill switch и одновременное превышение общего бюджета разными символами. Такие потери/обход kill в истории не доказаны; опасный путь есть в текущем коде. При выключенном backtest длительное ожидание этой ветки не происходит, но архитектурная проблема финального guard остаётся.

Исправление: account-scoped admission/reservation, повторное чтение risk/config прямо перед submit, freshness prices/book, учёт pending+unknown intents. Kill update и admission должны иметь определённый сериализуемый порядок; сетевой вызов не держит длинную DB-транзакцию. Если kill включён после уже отправленного запроса, результат обязательно reconcile, а не считать запрос несуществующим.

### F11 · P1 · Структурированный 5xx/internal error ошибочно может стать authoritative FAILED

[client.go:683](/Users/aleksey/Documents/Krypto_pionex/backend/internal/pionex/client.go:683) помечает invalid JSON mutation/5xx как OutcomeUnknown, но [client.go:712](/Users/aleksey/Documents/Krypto_pionex/backend/internal/pionex/client.go:712) возвращает валидный error envelope без этой метки. [lifecycle.go:207](/Users/aleksey/Documents/Krypto_pionex/backend/internal/grid/lifecycle.go:207) тогда выбирает FAILED_AUTHORITATIVE.

Официальная документация описывает BOT_INTERNAL_ERROR как system/RPC/network/downstream problem; она не гарантирует отсутствие побочного эффекта create. Поэтому освобождать guard и повторять создание без reconciliation небезопасно. [Pionex error definitions](https://www.pionex.com/docs/api-docs/bot-api/futures-grid).

Исправление: явно различить authoritative rejection и uncertain mutation outcome; 5xx/timeout/internal ambiguity → SUBMISSION_UNKNOWN до сверки. При неоднозначном поиске matching remote bot требовать операторского решения. Нельзя придумывать client-order-id или биржевую idempotency, если её нет в документации. Исторические дубли этой проверкой не установлены.

### F12 · P1 · Фактическая выплата не соответствует сохранённому R:R

[neutral_tp_doctrine.go:24](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/neutral_tp_doctrine.go:24) ограничивает dynamic NEUTRAL TP до 2% investment, floor=1 USDT. [worker.go:3313](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:3313) применяет cap после расчёта stress loss; [worker.go:3612](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:3612) сохраняет прежний adaptive R:R.

В 95 NEUTRAL-ботах с 28 сентября у 60 `pnl_target/max_loss<0.2`; минимум 0.08, средний сохранённый `risk_reward_ratio≈2.87`. Эти числа описывают разные величины. Для примера target=1.5 и max loss=15 отношение выплаты 0.1: в бинарном TP/SL-примере без комиссий нужно около 90.9% успешных исходов.

Это не означает, что каждый бот обязательно выходит строго по этим двум суммам. Но проверка min R:R=1.8 не защищает фактическое распределение конечного net PnL. Исправление: после всех geometry/TP/SL mutations заново вычислять payout/stress и admission; отличать price-distance R:R, target/loss ratio и empirical EV. Не «исправлять» ratio механическим удалением стопа или дальнейшим отодвиганием TP.

### F13 · P1 · Daily loss не является лимитом падения equity

[risk/engine.go:351](/Users/aleksey/Documents/Krypto_pionex/backend/internal/risk/engine.go:351) суммирует отрицательный сохранённый realized за последние 24 часа у STOPPED/LIQUIDATED/FAILED. Нет unrealized MTM, полного net-fee учёта, неизвестных finals и COMPLETED. Большая плавающая потеря не расходует этот лимит до закрытия.

Исправление: разделить cash realized-loss budget, intraday equity drawdown и scenario loss открытого+pending портфеля. Блокировать новые входы при UNKNOWN account state, а не считать неопределённый риск нулём. Global/account scope каждого лимита определить явно.

### F14 · P2 · Gate-quality API смешивает снимки разных окон

[decision_intelligence_api.go:354](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/decision_intelligence_api.go:354) выбирает все rows с `window_kind`, без выбора последнего `window_start`. UI показывает повторные gate/regime без явного различения дат. Окна перекрываются; их суммы не независимы.

Исправление: отдельно latest coherent snapshot и explicit historical series, обязательные window bounds/computed_at/quality. Данных в самом свежем 7D-срезе недостаточно: completed outcomes=0, calibration pairs=0, regime=UNKNOWN. Процент заблокированных входов не является денежной ценностью фильтра.

### F15 · P2 · Missing telemetry иногда превращается в нейтральный сигнал

[GetFearGreed](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/gates.go:109) при stale/error возвращает 50,nil; [vol-expansion check](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/entry_gates.go:122) при отсутствии свечей возвращает false,0. Funding average берёт все наблюдения за 10 минут, а не по одному свежему наблюдению каждого источника, что может менять вес источников в зависимости от частоты записи.

Liquidation health реализован аккуратнее — есть отдельная проверка транспортного здоровья. Распространить этот контракт на прочую telemetry: value, observed_at, available_at, freshness, quality, fallback_used. Optional feed может не запрещать торговлю, но деградация обязана быть явно видимой; обязательный feed должен закрывать admission. Внешние данные не становятся ценой исполнения Pionex.

### F16 · P2 · Несколько моделей метрик и hard-coded runtime policy

[scanner.go:370](/Users/aleksey/Documents/Krypto_pionex/backend/internal/marketdata/scanner.go:370) оценивает candle crossings через OHLC travel, затем строит условные returns. Его EV/Sharpe/Sortino/PF — эвристики ранжирования, не метрики реально исполняемого бота. Go backtest использует hard-coded DD=15%, OOS floor=−1%; в DB AutoGrid max drawdown=8%. Они действуют на разные объекты и не дают единого потолка риска.

Исправление: переименовать proxy features; торговую policy и threshold versions хранить в PostgreSQL и снимать в FINAL decision. Алгоритмические константы могут оставаться в коде, но изменяемые ограничения допуска/риска не должны скрываться в разных пакетах. ENV для runtime-policy не добавлять.

## 5. Модель уже расходилась с историческими результатами

В существующем [model-vs-real-calibration.csv](/Users/aleksey/Documents/Krypto_pionex/docs/model-vs-real-calibration.csv) 157 пар: sum actual −50.4217, sum model −12.4682 USDT; MAE≈0.7993 USDT/бот, средний bias модели +0.2417; совпадение знака ≈73.9%.

Модельный суммарный результат менее отрицательный примерно на 37.95 USDT. Ошибка масштаба около 0.80 USDT сопоставима/больше средней положительной выплаты около 0.75. Это аргумент против ранжирования по маленькой разнице модельного EV без uncertainty margin. CSV — исторический артефакт, не новая out-of-sample калибровка v2.0.190.

## 6. Что в проекте стоит сохранить

- PostgreSQL control plane, durable intents, audit/events и раздельные PAPER/REAL данные.
- Pionex-only execution, symbol validation, создание native futures grid через официальный endpoint.
- Сохранение remote ID до объявления RUNNING и обработку отсутствующего ID как uncertain submission.
- Отсутствие автоматических повторов mutating HTTP после обычного transport failure, clock sync и rate-limit механизмы — основу сохранить, дополнить uncertainty classification и проверкой общего лимита по IP.
- Fee gates, stress checks, kill switch и разнообразные защитные exit-проверки — проверять совместно, а не удалять ради красивого backtest.
- Уже имеющиеся понятия episode, replay, coverage и calibration. Основная проблема — незавершённая проводка данных, не отсутствие ещё одного индикатора.

Архитектурная проблема — несколько независимо изменяющихся описаний одного бота: scanner proxy, Python backtest, Go PAPER, shadow и native payload. Дополнительные exemptions и локальные правки targets усложняют причинную проверку качества входов.

## 7. Как действительно улучшать качество входов

### 7.1. Два архитектурных варианта

**A. Рекомендуемый: сохранить Go control plane, добавить единый ExecutionSpec и один окончательный admission.** Python остаётся расчётным worker. Все модели принимают один snapshot/spec, а live lifecycle потребляет проверенный admission token/reservation. Наименьший объём изменений, остаётся existing durable lifecycle, легче проверить rollback.

**B. Единый simulation service для PAPER/backtest/shadow.** Имеет смысл после выравнивания spec и сверки с Pionex. Снижает будущий drift, но добавляет service dependency, versioning и latency. Переписывать проект в микросервисы до исправления F01–F13 не требуется.

В обоих вариантах config/risk/kill/credentials — PostgreSQL; не вводить runtime ENV. ExecutionSpec содержит direction, grid type, диапазон, levels, investment, extra margin, leverage, точные stop types/values, upper stop, TP, exit policy, fees/slippage/funding assumptions, timestamps/data versions, model/code/config versions. Pionex values и внешняя gate telemetry разделены.

### 7.2. Порядок допуска

1. Проверить Pionex symbol/status, timestamps, price/book freshness, spread, depth, min investment/precision; отделить unavailable symbol от плохого signal.
2. Выбрать market regime и направление по заранее зафиксированной policy. NEUTRAL, LONG, SHORT анализировать раздельно; не смешивать их доходность.
3. Построить итоговую геометрию, initial inventory, защиту и размер; после последней модификации пересчитать costs/stress/payout.
4. Рассчитать распределение **итогового net результата бота**, а не прибыль отдельной grid-пары; проверить модельную ошибку, lower confidence bound и сценарий выхода за диапазон.
5. Размер выбрать по допустимому убытку и портфельной корреляции; investment — результат этого расчёта, не исходное желание полностью занять бюджет.
6. Провести final account-scoped admission с новой DB risk/config и свежей ценой/стаканом; сохранить FINAL trace, spec и reservation до create.
7. После ответа Pionex сверить фактические params/position, persisted remote ID и состояния. Любой unknown intent занимает риск до reconciliation.

Не добавлять новые индикаторы до честной оценки существующих. Проверять сначала простую baseline policy и небольшое число заранее выбранных альтернатив: directional exemptions on/off, neutral exit policy, size/risk policy. Смена нескольких правил одновременно делает выводы о причине неудачи невозможными.

### 7.3. Эксперимент вместо подгонки

- Заморозить code/config/spec version до начала периода. Изменение policy начинает новый cohort.
- Единица наблюдения — entry episode, а не каждое повторение scan и не grid fill. Повторы одного символа/движения не являются независимыми подтверждениями.
- Логировать admitted control и rejected shadow. Для late gate тень должна получить итоговую предполагаемую geometry, а не только scanner mesh.
- Для gate сравнивать outcome без него и с ним на сопоставимых episode/regime/direction; учитывать недозахват, CAP_REACHED, stale data и selection bias. Не выдавать unavailable outcome за ноль.
- Оценивать EV, PF, drawdown, cost-adjusted turnover, win rate, Sharpe/Sortino и worst regime совместно. При малом числе независимых outcomes ответ — «недостаточно данных».
- CI строить с учётом time/symbol clusters и model calibration; sample size выводить из желаемой точности, не объявлять 8 эффективных пар доказательством edge.
- Walk-forward policy learning проводить только на прошлых данных; holdout не использовать для выбора threshold. При многочисленных пробах учитывать риск overfitting. [Bailey et al., The Probability of Backtest Overfitting](https://www.davidhbailey.com/dhbpapers/backtest-prob.pdf).
- Прогрессировать от synthetic/contract fixtures к shadow/PAPER, затем к малому REAL canary. Совпадение PAPER и backtest — необходимая проверка модели, не доказательство биржевой прибыли.

### 7.4. Какие фильтры полезны, а какие мешают

Сейчас достоверного ответа нет: журнал не содержит full chain/config, shadow outcomes не связаны и не закрыты в новом контракте. Массовый отказ MACRO/RV или отсутствие новых ботов сами по себе не доказывают, что эти gates вредны. Отключать их ради большего числа запусков без такой проверки — неподтверждённая гипотеза.

После исправлений оценивать `gate_value = expected portfolio net outcome with gate − without gate` на сопоставимых независимых эпизодах, отдельно по regime/direction и с учётом удержания капитала. Поздние gates оценивать среди кандидатов, прошедших предшествующие; для оценки остальных нужны воспроизводимые counterfactual features/spec, а не сегодняшние данные. При unavailable outcome не назначать фильтру ни прибыль, ни убыток.

Directional exemptions — отдельный кандидат на контрольный эксперимент: в небольшой REAL-группе с 28 сентября `dirTrendLong=true` наблюдается 7 исходов с final −31.63 USDT. Это тревожный сигнал, но не статистическое доказательство вреда exemption: размер, актив, рынок и последующие exits могли отличаться. Проверять exemption on/off на будущих сопоставимых эпизодах, не расширять его применение по старой выборке.

## 8. План исправления — без изменения живой системы на этом этапе

| Этап | Работа и основные места | Критерий завершения |
|---|---|---|
| 1. Учёт | equity/settlement, ledger; cash-flow reconciliation с Pionex; 28 missing finals оставить UNKNOWN до фактов | Каждая итоговая строка имеет источник, качество, компоненты и сверенную identity; оценки явно отделены |
| 2. Финальная безопасность | lifecycle/risk/client: F10, F11, F13; account reservation, durable kill sequencing | Включение kill во время ожидания запрещает ещё не отправленный create; concurrent symbols не обходят limit; uncertain POST не повторяется |
| 3. Исходные данные аналитики | shadow/entry chain/API: F01–F03, F14; новые integration tests | Ни одной SQL linkage error; calculated outcomes имеют корректные terminal states; FINAL содержит весь spec/config/trace; replayable outcomes >0 |
| 4. Единая модель | F06–F09; shared spec, fees, funding, stops, inventory, volume; новый model version/cache invalidation | LONG/SHORT/NEUTRAL, arithmetic/geometric, обе stop-границы и partial fills проходят identity/parity fixtures |
| 5. Честные метрики | F04, F05, F12, F16; separate pair vs bot metrics, terminal exit costs, as-of OOS | Убыточный net-бот не допускается только из-за pair EV/100% pair WR; actual payout пересчитан после всех mutations |
| 6. Контрольный эксперимент | baseline, frozen versions, coverage and calibration; F15 | Достаточно независимых future outcomes и coverage; profitable conclusion не зависит от смены threshold на holdout |
| 7. Ограниченный REAL | Только после предыдущих acceptance criteria, отдельное согласование risk budget | Малый canary, автоматически ограниченные drawdown/correlation/unknowns; заранее заданные rollback conditions |

Этапы 2 и 3 могут выполняться параллельно. Учёт и model-spec должны быть согласованы до этапа 5. Полностью восстановить исторический config/trace невозможно, если значения не записаны: такие периоды исключаются из replay, а не реконструируются с сегодняшними настройками.

Согласовать срок хранения с горизонтом эксперимента: [housekeeping](/Users/aleksey/Documents/Krypto_pionex/backend/internal/autogrid/worker.go:7634) удаляет entry decisions через 14 дней. Для длительного walk-forward/calibration нужны сохранённые immutable snapshots и outcomes либо отдельный архив; policy хранения задаётся в DB. Не отключать очистку всей БД без оценки объёма.

### Обязательные regression/acceptance tests

- SQL shadow linkage на отдельной PostgreSQL БД с реальными миграциями, однозначным/неоднозначным decision match и coverage ledger.
- Shadow lifecycle: successful, missing/stale candles, funding unavailable, horizon end, TP/SL; terminal states и PnL parts согласованы.
- Decision FINAL: все gate steps, direction, effective prices/spec, available_at, immutable DB config; round-trip replay воспроизводит baseline.
- Fee accounting: entry/exit ровно один раз; partial fills, short covers, funding payment/receipt, terminal inventory close.
- Абсолютные lower/upper stops и совпадение geometric levels; LONG/SHORT initial inventory сверяется с наблюдаемыми Pionex fixtures.
- Cache miss при изменении direction, stop, upper stop, TP/exit policy, size, fee/funding, grid type, версии данных/модели.
- Zero/absent/stale volume/book не превращается в liquidity OK; execution size соответствует depth/participation constraints.
- Kill during wait; two symbols racing reservation; process crash после POST, до записи ID; valid JSON 5xx и timeout; reconnect/desync и stale account snapshot.
- Bot net loss + positive pair EV не даёт прибыльного verdict. Реальный итог учитывает slippage/fees/funding, включая отрицательный funding sign в исходных данных Pionex.
- Gate quality latest snapshot не складывает overlapping windows; UNKNOWN outcomes/config не считаются доказательством.

## 9. Формулы и рабочие условия отказа

### 9.1. Метрики

```text
BotPnL_net = realised_position_pnl + grid_pnl + unrealised_at_horizon
             + funding_received − funding_paid − execution_fees − other_costs
```

Компоненты должны быть взаимоисключающими: если grid profit или returned-cash final уже net-of-fees, их нельзя повторно складывать/вычитать. При обязательном закрытии на горизонте unrealised заменяется фактической/модельной liquidation leg с затратами.

```text
EV = mean(net_outcome_per_independent_episode)
PF = sum(positive net outcomes) / abs(sum(negative net outcomes))
WinRate = count(net > 0) / count(valid terminal outcomes)
Binary break-even win probability = L / (G + L)                 # без costs
Binary break-even with constant outcome friction C = (L+C)/(G+L)
Drawdown_t = 1 − cash_flow_adjusted_equity_t / running_peak_t
Sharpe = mean(excess period return) / std(period return) * sqrt(periods/year)
Sortino = mean(return − MAR) / sqrt(mean(min(return − MAR,0)^2))
          * sqrt(periods/year)
Turnover = sum(abs(executed_notional)) / mean(allocated_equity)
```

Для Sharpe/Sortino фиксировать период, annualization и MAR; не считать при невалидном знаменателе/короткой выборке. Equity return нормализовать на реальные внешние вводы/выводы, а не только закрытия ботов.

### 9.2. Размер вместо произвольного плеча

```text
R_bot <= min(account_loss_budget_remaining, symbol_risk_budget_remaining,
             portfolio_stress_budget_remaining)
stress_loss(spec, investment, adverse_paths, exit_costs) <= R_bot
```

На практике investment решается по stress-loss модели с лимитами биржи; нельзя предполагать точную линейность при min order/levels, funding, gaps и liquidation. Если допустимый размер ниже Pionex minimum — отказ от входа, а не повышение плеча ради минимальной заявки. Лимиты выбирать в долях проверенной account equity и переводить в USDT в DB, с явным максимумом. Резерв и correlated risk необходимы даже при max_active_bots.

### 9.3. Когда стратегия не должна запускаться или должна откатываться

- Kill enabled, превышен equity/daily/stress budget, не завершён reconciliation либо account state неизвестен.
- Pionex symbol недоступен, maintenance, precision/min investment не пройдены, clock/price/book stale, disconnect/desync.
- Liquidity/depth/size, spread, estimated slippage или grid edge после затрат не допускают размер.
- Нет правильной FINAL geometry/config или spec не соответствует проверенной модели/cache.
- Lower net EV confidence bound не превышает заранее заданный uncertainty/cost hurdle; нет нужной мощности выборки.
- NEUTRAL при подтверждённом выходе из mean-reverting regime; LONG/SHORT при отсутствии соответствующего заранее определённого directional signal. Не применять нейтральные Spot Grid AI параметры к Futures Grid.
- Ухудшение model-vs-real residual, net PF/EV или worst-regime drawdown относительно заранее заданного canary budget.
- Телеметрия обязательного gate умерла; optional telemetry деградировала без явной индикации/допустимой DB fallback policy.

Все перечисленные условия должны иметь наблюдаемый reason code и DB policy. Они ограничивают ущерб; не гарантируют прибыль и исполнение стопа по заданной цене.

## 10. Риски и окончательный вывод

**Модельные:** OHLC path ambiguity, queue/partial fills, funding proxy, различие simulation/live, look-ahead, selection bias, повторные scans, многократная настройка threshold, маленькие directional выборки, неизвестные finals и конфигурации, смена code/regime.

**Рыночные:** correlated crypto move, trend break, gap/slippage, spread widening, thin books, sudden funding, liquidation, Pionex maintenance/API uncertainty. Local manage 45 секунд не является гарантией мгновенного выхода; native stop тоже не гарантирует цену исполнения. Дополнительно учитывать доступность деривативов в юрисдикции пользователя, правила биржи и налоговый учёт — без предположений о конкретных правовых условиях.

**Итог:** сейчас не доказана «плохая единственная монета» или «единственный лишний фильтр». Доказаны отрицательная структура результатов, несовпадающие модели и сломанный контур измерения gate quality. Главный путь к лучшим входам — сначала сделать результат и допуск воспроизводимыми, затем проверить простую frozen policy на будущих данных. Потенциал улучшения есть в снижении крупных directional потерь, согласовании actual payout/risk и отказе от низколиквидных/неподтверждённых входов; величина будущей прибыли пока неизвестна.

## 11. Приложения для воспроизведения

- [Read-only SQL](/Users/aleksey/Documents/Krypto_pionex/docs/project-assessment-2026-10-05.sql): метрики final/project net, источники/unknowns, payout, decisions/shadow, backtest contradiction и рабочие WARN. Запросы не меняют runtime-настройки.
- [Синтетическая проверка Python-модели](/Users/aleksey/Documents/Krypto_pionex/scripts/audit-quant-model.py): не подключается к бирже/БД, ничего не изменяет; печатает обнаружение двух текущих дефектов.

```sh
cd /Users/aleksey/Documents/Krypto_pionex
python3 scripts/audit-quant-model.py
```

Нулевой exit code этого диагностического скрипта означает успешный расчёт диагностики, не отсутствие дефектов: смотреть `defect_detected` в JSON. После исправления модели значения должны стать false, а корректность должна проверяться нормальными regression tests.

Проверка приложений: все 11 SQL statements отдельно выполнены через read-only MCP без ошибок; диагностический скрипт запущен, оба `defect_detected=true`. Эти проверки произведены чуть позже исходных срезов; выросшие счётчики решений не меняют финансовую таблицу закрытий.
