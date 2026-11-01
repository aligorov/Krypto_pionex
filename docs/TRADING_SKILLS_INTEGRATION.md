# Интеграция Trading Skills в Krypto_pionex

Репозиторий `trading_skills` (AI-powered market analysis & MCP server) успешно клонирован, настроен и адаптирован для использования в рамках проекта **Krypto_pionex**.

---

## 1. Структура размещения

1. **Автономный репозиторий**:  
   `/Users/aleksey/Documents/trading_skills` — независимый проект с собственным окружением, тестами и Git-историей.
2. **Локальный модуль проекта**:  
   `/Users/aleksey/Documents/Krypto_pionex/trading_skills` — изолирован через `.gitignore`, содержит виртуальное окружение `.venv` (Python 3.13 + uv), все зависимости и библиотеку `trading_skills`.
3. **Workspace Skills (Antigravity/Agent)**:  
   `/Users/aleksey/Documents/Krypto_pionex/.agents/skills/` — набор скиллов для агента Antigravity (`technical-analysis`, `risk-assessment`, `scanner-bullish`, `price-correlation` и др.).
4. **Утилита аналитики**:  
   `/Users/aleksey/Documents/Krypto_pionex/scripts/market-analytics.py` — единый CLI для быстрого анализа криптовалютных пар и рыночной телеметрии.

---

## 2. Соответствие правилам проекта (AGENTS.md)

- **Execution & Prices (Правило 1)**:  
  Все торговые операции, ордера, сетки и учет балансов остаются исключительно на стороне Pionex API (`pcrypto-bot` MCP и Go backend).  
  Инструменты `trading_skills` используются **только как read-only телеметрия рынка** (market telemetry, гейты качества входов, фильтры волатильности, расчет корреляций, отслеживание макро-событий).
- **Zero-ENV Policy (Правило 2)**:  
  Настройки бота и лимиты риска не зависят от .env, всё ядро управления хранится в PostgreSQL.
- **Поддержка символов Pionex**:  
  Реализована сквозная нормализация символов (`BTC_USDT`, `BTC_USDT_PERP`, `ETH_USDT` → `BTC-USD`, `ETH-USD`), что позволяет использовать привычный синтаксис Pionex во всех аналитических функциях и MCP-инструментах.

---

## 3. Быстрый запуск и использование

### Через CLI `scripts/market-analytics.py`

Запуск через локальный venv:
```bash
# Получить котировку и 24ч статистику
trading_skills/.venv/bin/python scripts/market-analytics.py quote BTC_USDT

# Технические индикаторы (RSI, MACD с кроссоверами, Bollinger Bands, EMA9/21, SMA, ATR, ADX)
trading_skills/.venv/bin/python scripts/market-analytics.py technicals BTC_USDT --period 1mo

# Оценка рисков (годовая волатильность, Sharpe ratio, VaR 95%/99%, Max Drawdown)
trading_skills/.venv/bin/python scripts/market-analytics.py risk BTC_USDT --period 6mo

# Матрица корреляций активов (для диверсификации и контроля совместного риска)
trading_skills/.venv/bin/python scripts/market-analytics.py correlation BTC_USDT ETH_USDT SOL_USDT

# Скринер бычьего моментума с ранжированием по скорингу
trading_skills/.venv/bin/python scripts/market-analytics.py scan BTC_USDT ETH_USDT SOL_USDT DOGE_USDT AVAX_USDT --top 3
```

---

## 4. Доступные Workspace Skills для Antigravity

В директории `.agents/skills/` активированы:
- **`technical-analysis`**: расчет RSI, MACD (включая кроссоверы вверх/вниз и сколько баров назад они произошли), EMA9/21, полос Боллинджера, ADX, ATR.
- **`risk-assessment`**: квантификация риска, расчет Sharpe ratio, Value-at-Risk и просадки.
- **`scanner-bullish`**: многофакторный скрининг и ранжирование активов.
- **`calendar` / `earnings-calendar`**: экономический календарь ForexFactory (решения ФРС по ставке, CPI, NFP), предотвращающий вход в позиции перед макро-штормами.
- **`markdown-to-pdf`**: генерация PDF-отчетов аудита и результатов бэктестов.

---

## 5. Подключение MCP-сервера

В конфигурацию `~/.gemini/config/mcp_config.json` добавлен сервер `trading-skills`:
```json
{
  "mcpServers": {
    "trading-skills": {
      "command": "/Users/aleksey/Documents/Krypto_pionex/trading_skills/.venv/bin/trading-skills-mcp"
    }
  }
}
```
Он предоставляет 23 инструмента для прямого вызова ассистентом (`technical_indicators`, `price_correlation`, `risk_assessment`, `stock_quote` и др.).
