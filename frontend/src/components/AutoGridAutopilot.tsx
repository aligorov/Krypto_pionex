import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, ApiError } from '../api';
import type {
  Account,
  AutoGridCandidate,
  AutoGridSettings,
  AutoGridState,
  PreparedCommand,
} from '../types';

interface Props {
  canOperate: boolean;
}

type Action = 'start' | 'scan' | 'stop' | 'emergency-stop';

export default function AutoGridAutopilot({ canOperate }: Props) {
  const [state, setState] = useState<AutoGridState | null>(null);
  const [settings, setSettings] = useState<AutoGridSettings | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [dirty, setDirty] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [notice, setNotice] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

  const load = useCallback(async () => {
    const [autoGridState, accountItems] = await Promise.all([
      api<AutoGridState>('/api/autogrid'),
      api<Account[]>('/api/accounts'),
    ]);
    setState(autoGridState);
    setSettings((current) => (dirty && current ? current : autoGridState.settings));
    setAccounts(accountItems);
  }, [dirty]);

  useEffect(() => {
    let active = true;
    const refresh = async () => {
      try {
        const [autoGridState, accountItems] = await Promise.all([
          api<AutoGridState>('/api/autogrid'),
          api<Account[]>('/api/accounts'),
        ]);
        if (!active) return;
        setState(autoGridState);
        setSettings((current) => (dirty && current ? current : autoGridState.settings));
        setAccounts(accountItems);
      } catch (error: unknown) {
        if (active) setNotice({ kind: 'danger', text: describeError(error) });
      }
    };
    void refresh();
    const timer = window.setInterval(() => void refresh(), 5000);
    return () => {
      active = false;
      window.clearInterval(timer);
    };
  }, [dirty]);

  const enabledAccounts = useMemo(
    () => accounts.filter((account) => account.isEnabled && account.hasReadPermission),
    [accounts],
  );

  function update<K extends keyof AutoGridSettings>(key: K, value: AutoGridSettings[K]) {
    setSettings((current) => (current ? { ...current, [key]: value } : current));
    setDirty(true);
  }

  async function saveSettings(): Promise<AutoGridSettings | null> {
    if (!settings) return null;
    setBusy('save');
    setNotice(null);
    try {
      const saved = await api<AutoGridSettings>('/api/autogrid/settings', {
        method: 'PUT',
        body: JSON.stringify(settingsPayload(settings)),
      });
      setSettings(saved);
      setDirty(false);
      setNotice({ kind: 'success', text: 'Настройки Автопилота сохранены в PostgreSQL.' });
      await load();
      return saved;
    } catch (error: unknown) {
      setNotice({ kind: 'danger', text: describeError(error) });
      return null;
    } finally {
      setBusy(null);
    }
  }

  async function runAction(action: Action) {
    if (!settings) return;
    if (action === 'start' && settings.executionMode === 'REAL') {
      const approved = window.confirm(
        'REAL-режим создаёт настоящие Pionex Futures Grid bots. Проверьте kill switch, лимиты, account и бюджет. Продолжить?',
      );
      if (!approved) return;
    }
    if (action === 'emergency-stop') {
      const approved = window.confirm(
        'Emergency Stop включит durable kill switch и отправит cancel всем активным REAL grid bots. Продолжить?',
      );
      if (!approved) return;
    }
    if (dirty && (action === 'start' || action === 'scan')) {
      const saved = await saveSettings();
      if (!saved) return;
    }
    setBusy(action);
    setNotice(null);
    try {
      const prepared = await api<PreparedCommand>(`/api/autogrid/actions/${action}`, {
        method: 'POST',
        body: JSON.stringify({ idempotencyKey: crypto.randomUUID() }),
      });
      let command = prepared.command;
      if (prepared.confirmationCode) {
        const entered = window.prompt(
          `Опасная команда требует второго шага. Введите код подтверждения: ${prepared.confirmationCode}`,
        );
        if (entered === null) {
          setNotice({ kind: 'danger', text: 'Команда подготовлена, но не подтверждена.' });
          return;
        }
        command = await api<PreparedCommand['command']>(
          `/api/control/commands/${command.id}/confirm`,
          {
            method: 'POST',
            body: JSON.stringify({ confirmationCode: entered.trim() }),
          },
        );
      }
      setNotice({
        kind: 'success',
        text: `Команда ${command.commandType} принята: ${command.status}. Worker подтвердит итог в PostgreSQL.`,
      });
      await delay(1200);
      await load();
    } catch (error: unknown) {
      setNotice({ kind: 'danger', text: describeError(error) });
      await load();
    } finally {
      setBusy(null);
    }
  }

  if (!state || !settings) {
    return <div className="panel"><div className="splash-inline"><span className="spinner" />Загружаю AutoGrid…</div></div>;
  }

  const running = state.settings.status === 'RUNNING' || state.settings.status === 'STARTING';
  const accepted = state.candidates.filter((candidate) => candidate.decision === 'ACCEPTED').length;

  return (
    <div className="section-stack">
      {notice && (
        <div className={`alert ${notice.kind}`}>
          <span>{notice.text}</span>
          <button type="button" onClick={() => setNotice(null)} aria-label="Закрыть">×</button>
        </div>
      )}

      <section className={`autopilot-hero ${state.settings.executionMode === 'REAL' ? 'real' : ''}`}>
        <div>
          <div className="autopilot-title">
            <span className="autopilot-icon">⚡</span>
            <div>
              <span className="eyebrow">PIONEX NATIVE FUTURES GRID</span>
              <h2>Auto-Grid Автопилот</h2>
            </div>
            <StatusBadge status={state.settings.status} />
          </div>
          <p>
            Pionex PERP scanner → модельные EV/Sharpe/Sortino/DD/PF → risk pre-flight →
            PAPER simulator или native Futures Grid lifecycle.
          </p>
        </div>
        <div className="autopilot-actions">
          <button
            className="button primary"
            type="button"
            disabled={!canOperate || running || busy !== null}
            onClick={() => void runAction('start')}
          >
            ▶ Запустить
          </button>
          <button
            className="button warning"
            type="button"
            disabled={!canOperate || busy !== null}
            onClick={() => void runAction('scan')}
          >
            ◉ Сканировать сейчас
          </button>
          <button
            className="button secondary"
            type="button"
            disabled={!canOperate || !running || busy !== null}
            onClick={() => void runAction('stop')}
          >
            ■ Остановить Автопилот
          </button>
          <button
            className="button danger"
            type="button"
            disabled={!canOperate || busy !== null}
            onClick={() => void runAction('emergency-stop')}
          >
            Emergency Stop
          </button>
        </div>
      </section>

      {state.settings.lastError && (
        <div className="alert danger">
          <span><strong>Последняя ошибка worker:</strong> {state.settings.lastError}</span>
        </div>
      )}

      <section className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">DURABLE SETTINGS</span>
            <h2>Параметры торговли</h2>
          </div>
          <span className={`badge ${settings.executionMode === 'REAL' ? 'danger' : 'success'}`}>
            MODE: {settings.executionMode}
          </span>
        </div>

        <div className="autogrid-settings">
          <label>
            Бюджет на 1 бота (USDT)
            <input
              type="number"
              min="1"
              step="0.01"
              disabled={running}
              value={settings.budgetUsdt}
              onChange={(event) => update('budgetUsdt', event.target.value)}
            />
          </label>
          <label>
            Макс. одновременно ботов
            <input
              type="number"
              min="1"
              max="20"
              disabled={running}
              value={settings.maxActiveBots}
              onChange={(event) => update('maxActiveBots', Number(event.target.value))}
            />
          </label>
          <label>
            Режим торговли
            <select
              disabled={running}
              value={settings.executionMode}
              onChange={(event) => update('executionMode', event.target.value as 'PAPER' | 'REAL')}
            >
              <option value="PAPER">PAPER — безопасная симуляция</option>
              <option value="REAL">REAL — Pionex native API</option>
            </select>
          </label>
          <label>
            Биржа / аккаунт
            <select
              disabled={running || settings.executionMode === 'PAPER'}
              value={settings.accountId ?? ''}
              onChange={(event) => update('accountId', event.target.value || null)}
            >
              <option value="">PIONEX — выберите аккаунт</option>
              {enabledAccounts.map((account) => (
                <option key={account.id} value={account.id}>
                  PIONEX — {account.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Стоп-лосс (защита)
            <select
              disabled={running}
              value={settings.stopLossMode}
              onChange={(event) => update('stopLossMode', event.target.value as 'NONE' | 'ADAPTIVE_ATR')}
            >
              <option value="ADAPTIVE_ATR">Адаптивный диапазон + native price stop</option>
              <option value="NONE">Выкл. (не рекомендуется)</option>
            </select>
          </label>
          <label>
            Умный фиксатор PnL
            <select
              disabled={running}
              value={settings.smartPnlEnabled ? 'on' : 'off'}
              onChange={(event) => update('smartPnlEnabled', event.target.value === 'on')}
            >
              <option value="on">Вкл. — native take-profit для LONG/SHORT</option>
              <option value="off">Выкл.</option>
            </select>
          </label>
          <label>
            Адаптивное плечо
            <select
              disabled={running}
              value={settings.adaptiveLeverageEnabled ? 'on' : 'off'}
              onChange={(event) => update('adaptiveLeverageEnabled', event.target.value === 'on')}
            >
              <option value="on">Вкл. — ограничение по volatility и risk</option>
              <option value="off">Выкл. — фиксированное плечо</option>
            </select>
          </label>
          <label>
            Максимальное плечо
            <input
              type="number"
              min="1"
              max="100"
              disabled={running}
              value={settings.leverage}
              onChange={(event) => update('leverage', Number(event.target.value))}
            />
          </label>
          <label>
            Сгущение сетки
            <select
              disabled={running}
              value={settings.densityGridEnabled ? 'on' : 'off'}
              onChange={(event) => update('densityGridEnabled', event.target.value === 'on')}
            >
              <option value="on">Вкл. — Pionex geometric grid</option>
              <option value="off">Выкл. — arithmetic grid</option>
            </select>
          </label>
          <label>
            Таймфрейм
            <select
              disabled={running}
              value={settings.candleInterval}
              onChange={(event) => update('candleInterval', event.target.value)}
            >
              {['5M', '15M', '30M', '60M', '4H', '8H', '12H', '1D'].map((interval) => (
                <option key={interval} value={interval}>{interval}</option>
              ))}
            </select>
          </label>
          <label>
            Свечей в модели
            <input
              type="number"
              min="30"
              max="500"
              disabled={running}
              value={settings.lookbackCandles}
              onChange={(event) => update('lookbackCandles', Number(event.target.value))}
            />
          </label>
          <label>
            Пар за один scan
            <input
              type="number"
              min="1"
              max="50"
              disabled={running}
              value={settings.maxSymbolsPerScan}
              onChange={(event) => update('maxSymbolsPerScan', Number(event.target.value))}
            />
          </label>
        </div>

        <details className="advanced-settings">
          <summary>Модельные пороги и предположения</summary>
          <div className="autogrid-settings advanced-grid">
            <NumberSetting label="Min EV, %" value={settings.minEvPct} onChange={(value) => update('minEvPct', value)} disabled={running} />
            <NumberSetting label="Min Sharpe" value={settings.minSharpe} onChange={(value) => update('minSharpe', value)} disabled={running} />
            <NumberSetting label="Min volume 24h, USDT" value={settings.minVolume24h} onChange={(value) => update('minVolume24h', value)} disabled={running} />
            <NumberSetting label="Min volatility, %" value={settings.minVolatilityPct} onChange={(value) => update('minVolatilityPct', value)} disabled={running} />
            <NumberSetting label="Max volatility, %" value={settings.maxVolatilityPct} onChange={(value) => update('maxVolatilityPct', value)} disabled={running} />
            <NumberSetting label="Max model DD, %" value={settings.maxDrawdownPct} onChange={(value) => update('maxDrawdownPct', value)} disabled={running} />
            <NumberSetting label="Min profit factor" value={settings.minProfitFactor} onChange={(value) => update('minProfitFactor', value)} disabled={running} />
            <NumberSetting label="Fee assumption, bps/fill" value={settings.feeBps} onChange={(value) => update('feeBps', value)} disabled={running} />
            <NumberSetting label="Slippage, bps/fill" value={settings.slippageBps} onChange={(value) => update('slippageBps', value)} disabled={running} />
            <label>
              Интервал scan, сек.
              <input
                type="number"
                min="60"
                max="86400"
                disabled={running}
                value={settings.scanIntervalSeconds}
                onChange={(event) => update('scanIntervalSeconds', Number(event.target.value))}
              />
            </label>
          </div>
        </details>

        <div className="panel-actions">
          <span className="compact">
            Fee/slippage — явные модельные допущения из БД, не обещание комиссии или доходности Pionex.
          </span>
          <button
            className="button primary"
            type="button"
            disabled={!canOperate || !dirty || running || busy !== null}
            onClick={() => void saveSettings()}
          >
            {busy === 'save' ? 'Сохраняю…' : 'Сохранить'}
          </button>
        </div>
      </section>

      <div className="autogrid-metrics">
        <Metric label="Активные grid bots" value={`${state.activeBots.length} / ${state.settings.maxActiveBots}`} />
        <Metric label="Принято кандидатов" value={`${accepted} / ${state.candidates.length}`} tone="positive" />
        <Metric label="Последний scan" value={state.lastScan?.status ?? 'Ещё не запускался'} />
        <Metric label="Режим исполнения" value={state.settings.executionMode} tone={state.settings.executionMode === 'REAL' ? 'negative' : 'positive'} />
      </div>

      <section className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">REMOTE LIFECYCLE / PAPER STATE</span>
            <h2>Активные сетки</h2>
          </div>
        </div>
        {state.activeBots.length === 0 ? (
          <div className="empty-state">Активных AutoGrid-ботов нет.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Источник</th>
                  <th>Remote BU Order ID</th>
                  <th>Символ</th>
                  <th>Направление</th>
                  <th>Диапазон</th>
                  <th>Сетки</th>
                  <th>Плечо</th>
                  <th>Бюджет</th>
                  <th>Статус</th>
                  <th>PnL</th>
                </tr>
              </thead>
              <tbody>
                {state.activeBots.map((bot) => (
                  <tr key={`${bot.source}:${bot.id}`}>
                    <td><span className={`badge ${bot.source === 'REAL' ? 'danger' : 'neutral'}`}>{bot.source}</span></td>
                    <td><code>{bot.buOrderId ?? 'paper/local'}</code><small>{bot.reconciliationState}</small></td>
                    <td><strong>{bot.symbol}</strong></td>
                    <td>{bot.direction}</td>
                    <td>{money(bot.lowerPrice)} — {money(bot.upperPrice)}</td>
                    <td>{bot.gridNum} · {bot.gridType}</td>
                    <td>{bot.leverage}×</td>
                    <td>{money(bot.quoteInvestment)} USDT</td>
                    <td><span className="badge success">{bot.status}</span></td>
                    <td>{bot.unrealizedPnlUsdt === null ? 'не получен' : `${money(bot.unrealizedPnlUsdt)} USDT`}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">LIVE PIONEX MARKET DATA · MODEL PROXY</span>
            <h2>Кандидаты сканера</h2>
            <p className="muted">
              Funding не подставляется фиктивно. Пустое значение означает, что метрика не включена в текущий официальный scan.
            </p>
          </div>
          {state.lastScan && (
            <div className="scan-meta">
              <strong>{state.lastScan.status}</strong>
              <span>{new Date(state.lastScan.startedAt).toLocaleString('ru-RU')}</span>
            </div>
          )}
        </div>
        {state.candidates.length === 0 ? (
          <div className="empty-state">Запустите scan, чтобы получить реальные Pionex PERP данные.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Символ</th>
                  <th>Цена / диапазон</th>
                  <th>Volatility</th>
                  <th>24h turnover</th>
                  <th>EV</th>
                  <th>Sharpe / Sortino</th>
                  <th>Max DD</th>
                  <th>Win / PF</th>
                  <th>Решение</th>
                </tr>
              </thead>
              <tbody>
                {state.candidates.map((candidate) => (
                  <CandidateRow key={candidate.id} candidate={candidate} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="model-warning">
        <strong>Условия отказа стратегии</strong>
        <span>
          Kill switch включён; credentials не верифицированы; REAL gates выключены; бюджет/плечо превышают risk limits;
          недостаточная ликвидность; volatility или max drawdown выше порога; EV/Sharpe/profit factor ниже порога;
          Pionex не вернул authoritative buOrderId; REST reconciliation не подтверждает remote state.
        </span>
      </section>
    </div>
  );
}

function CandidateRow({ candidate }: { candidate: AutoGridCandidate }) {
  return (
    <tr>
      <td>
        <strong>{candidate.symbol}</strong>
        <small>{candidate.recommendedTrend} · {candidate.recommendedLeverage}×</small>
      </td>
      <td>
        {money(candidate.currentPrice)}
        <small>{money(candidate.lowerPrice)} — {money(candidate.upperPrice)}</small>
      </td>
      <td>{number(candidate.volatilityPct, 2)}%</td>
      <td>{compactMoney(candidate.volume24h)}</td>
      <td className={Number(candidate.expectedValuePct) >= 0 ? 'positive' : 'negative'}>
        {number(candidate.expectedValuePct, 4)}%
      </td>
      <td>{number(candidate.sharpe, 2)} / {number(candidate.sortino, 2)}</td>
      <td>{number(candidate.maxDrawdownPct, 2)}%</td>
      <td>{number(candidate.winRatePct, 1)}% / {number(candidate.profitFactor, 2)}</td>
      <td>
        <span className={`badge ${candidate.decision === 'ACCEPTED' ? 'success' : 'danger'}`}>
          {candidate.decision}
        </span>
        {candidate.rejectionReason && <small>{candidate.rejectionReason}</small>}
      </td>
    </tr>
  );
}

function NumberSetting({
  label,
  value,
  onChange,
  disabled,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
  disabled: boolean;
}) {
  return (
    <label>
      {label}
      <input
        type="number"
        step="0.0001"
        disabled={disabled}
        value={value}
        onChange={(event) => onChange(event.target.value)}
      />
    </label>
  );
}

function StatusBadge({ status }: { status: AutoGridSettings['status'] }) {
  const tone = status === 'RUNNING' ? 'success' : status === 'EMERGENCY_STOPPED' ? 'danger' : 'warning';
  return <span className={`badge ${tone}`}>{status}</span>;
}

function Metric({ label, value, tone = '' }: { label: string; value: string; tone?: string }) {
  return <div className="metric-card"><span>{label}</span><strong className={tone}>{value}</strong></div>;
}

function settingsPayload(settings: AutoGridSettings): Omit<
  AutoGridSettings,
  'id' | 'status' | 'lastError' | 'lastStartedAt' | 'lastStoppedAt' | 'createdAt' | 'updatedAt'
> {
  return {
    accountId: settings.accountId,
    executionMode: settings.executionMode,
    budgetUsdt: settings.budgetUsdt,
    maxActiveBots: settings.maxActiveBots,
    leverage: settings.leverage,
    minSharpe: settings.minSharpe,
    minEvPct: settings.minEvPct,
    stopLossMode: settings.stopLossMode,
    smartPnlEnabled: settings.smartPnlEnabled,
    adaptiveLeverageEnabled: settings.adaptiveLeverageEnabled,
    densityGridEnabled: settings.densityGridEnabled,
    candleInterval: settings.candleInterval,
    lookbackCandles: settings.lookbackCandles,
    maxSymbolsPerScan: settings.maxSymbolsPerScan,
    scanIntervalSeconds: settings.scanIntervalSeconds,
    minVolume24h: settings.minVolume24h,
    minVolatilityPct: settings.minVolatilityPct,
    maxVolatilityPct: settings.maxVolatilityPct,
    maxDrawdownPct: settings.maxDrawdownPct,
    minProfitFactor: settings.minProfitFactor,
    feeBps: settings.feeBps,
    slippageBps: settings.slippageBps,
  };
}

function number(value: string, digits: number): string {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed.toFixed(digits) : '—';
}

function money(value: string): string {
  const parsed = Number(value);
  return Number.isFinite(parsed)
    ? parsed.toLocaleString('ru-RU', { maximumFractionDigits: 6 })
    : '—';
}

function compactMoney(value: string): string {
  const parsed = Number(value);
  return Number.isFinite(parsed)
    ? new Intl.NumberFormat('ru-RU', { notation: 'compact', maximumFractionDigits: 2 }).format(parsed)
    : '—';
}

function describeError(error: unknown): string {
  if (error instanceof ApiError || error instanceof Error) return error.message;
  return 'Неизвестная ошибка';
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}
