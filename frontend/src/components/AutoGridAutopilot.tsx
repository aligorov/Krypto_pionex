import { FormEvent, useCallback, useEffect, useMemo, useState } from 'react';
import { api, describeError, getCachedAutoGrid, setCachedAutoGrid } from '../api';
import type {
  Account,
  AutoGridPreset,
  AutoGridSettings,
  AutoGridState,
  EquityEpochSummary,
} from '../types';

interface Props {
  canOperate: boolean;
  accountsHref: () => void;
}

type Message = { kind: 'success' | 'danger'; text: string } | null;

// dec normalizes an edited decimal field: trims, accepts the Russian decimal
// comma and treats empty/invalid input as 0 instead of breaking the API call.
const dec = (value: unknown): string => {
  const text = String(value ?? '').trim().replace(',', '.');
  return text !== '' && Number.isFinite(Number(text)) ? text : '0';
};

const numberField = (value: string, fallback: number): number => {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : fallback;
};

export { describeError };

export default function AutoGridAutopilot({ canOperate, accountsHref }: Props) {
  const [state, setState] = useState<AutoGridState | null>(() => getCachedAutoGrid<AutoGridState>());
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [message, setMessage] = useState<Message>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState(false);
  const [presets, setPresets] = useState<AutoGridPreset[]>([]);
  const [busyPreset, setBusyPreset] = useState<string | null>(null);
  const [busyMode, setBusyMode] = useState<string | null>(null);
  // v2.0.88 «одна правда на экране»: the TOTAL PnL hero reads
  // state.epoch — the same AccountEquityEpoch summary every other tab
  // renders, carried by the /api/autogrid payload itself. No separate
  // polling loop: two fetches of one number is how the screen diverged
  // from itself in the first place.

  async function switchMode(mode: 'PAPER' | 'REAL') {
    if (mode === 'REAL' && !window.confirm(
      'Переключить автопилот в REAL? Новые боты будут открываться на настоящие деньги Pionex.',
    )) {
      return;
    }
    setBusyMode(mode);
    setMessage(null);
    try {
      await api('/api/autogrid/mode', { method: 'PUT', body: JSON.stringify({ mode }) });
      setMessage({ kind: 'success', text: `Режим изменён на ${mode}.` });
      await load();
    } catch (modeError) {
      setMessage({ kind: 'danger', text: describeError(modeError) });
    } finally {
      setBusyMode(null);
    }
  }

  const [clearingPaper, setClearingPaper] = useState(false);

  async function handleClearPaper() {
    if (!window.confirm('Сбросить всю историю и накопленный PnL симуляции (PAPER)? Все симуляционные боты будут удалены.')) {
      return;
    }
    setClearingPaper(true);
    setMessage(null);
    try {
      const res = await api<{ success: boolean; deletedCount: number }>('/api/autogrid/paper/clear', {
        method: 'POST',
        body: JSON.stringify({ includeRunning: true }),
      });
      setMessage({ kind: 'success', text: `История симуляции полностью очищена (${res.deletedCount} ботов удалено).` });
      await load();
    } catch (clearErr) {
      setMessage({ kind: 'danger', text: describeError(clearErr) });
    } finally {
      setClearingPaper(false);
    }
  }

  const load = useCallback(async () => {
    try {
      const result = await api<AutoGridState>('/api/autogrid');
      setState(result);
      setCachedAutoGrid(result);
    } catch (error) {
      if (!getCachedAutoGrid()) {
        setMessage({ kind: 'danger', text: describeError(error) });
      }
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  useEffect(() => {
    api<AutoGridPreset[]>('/api/autogrid/presets')
      .then(setPresets)
      .catch(() => setPresets([]));
  }, []);

  async function applyPreset(presetId: string) {
    setBusyPreset(presetId);
    setMessage(null);
    try {
      const result = await api<{ preset: AutoGridPreset }>(
        `/api/autogrid/presets/${presetId}/apply`,
        { method: 'POST' },
      );
      setMessage({ kind: 'success', text: `Преднастройка «${result.preset.title}» применена.` });
      await load();
    } catch (presetError) {
      setMessage({ kind: 'danger', text: describeError(presetError) });
    } finally {
      setBusyPreset(null);
    }
  }

  useEffect(() => {
    if (!editing) {
      api<Account[]>('/api/accounts')
        .then(setAccounts)
        .catch(() => setAccounts([]));
    }
  }, [editing]);

  const settings = state?.settings ?? null;
  const running = settings?.status === 'RUNNING' || settings?.status === 'STARTING';
  const pnl = state?.pnl;
  const exchange = state?.exchange;

  async function runAction(action: string) {
    if (!canOperate) return;
    if (action === 'emergency-stop' && !window.confirm(
      'Emergency Stop: включит kill switch и немедленно отменит все реальные гриды на Pionex. Продолжить?',
    )) {
      return;
    }
    setBusy(action);
    setMessage(null);
    try {
      const res = await api<{ success: boolean; message?: string; confirmationCode?: string }>(`/api/autogrid/actions/${action}`, {
        method: 'POST',
        body: JSON.stringify({ idempotencyKey: `ui-${action}-${Date.now()}` }),
      });
      if (state && state.settings) {
        let newStatus = state.settings.status;
        if (action === 'start') newStatus = 'RUNNING';
        if (action === 'stop') newStatus = 'STOPPED';
        if (action === 'emergency-stop') newStatus = 'EMERGENCY_STOPPED';
        setState({
          ...state,
          settings: { ...state.settings, status: newStatus, lastError: null },
        });
      }
      const code = res.confirmationCode;
      setMessage({
        kind: 'success',
        text: res.message || (code ? `Команда создана. Код: ${code}` : 'Действие успешно выполнено.'),
      });
      await load();
    } catch (error) {
      setMessage({ kind: 'danger', text: describeError(error) });
    } finally {
      setBusy(null);
    }
  }

  if (!state || !settings) {
    return (
      <div className="section-stack">
        {message && (
          <div className={`alert ${message.kind === 'success' ? 'success' : 'danger'}`}>
            <span>{message.text}</span>
            <button className="button secondary" style={{ marginLeft: 12, padding: '2px 8px' }} onClick={() => void load()}>Повторить</button>
          </div>
        )}
        <div className="empty-state">
          <div>Загрузка состояния автопилота…</div>
          <button className="button secondary" style={{ marginTop: 12 }} onClick={() => void load()}>Обновить сейчас</button>
        </div>
      </div>
    );
  }

  return (
    <div className="section-stack">
      {message && (
        <div className={`alert ${message.kind === 'success' ? 'success' : 'danger'}`}>
          <span>{message.text}</span>
          <button onClick={() => setMessage(null)}>×</button>
        </div>
      )}
      {settings.lastError && (
        <div className="alert danger">
          <span>Последняя ошибка автопилота: {settings.lastError}</span>
        </div>
      )}

      <div className={`kill-panel ${settings.status === 'RUNNING' ? '' : 'active'}`}>
        <div>
          <span className="eyebrow">AUTOGRID AUTOPILOT</span>
          <h2>
            {statusTitle(settings.status)} {settings.status === 'RUNNING' ? '🟢' : '⚪️'}
          </h2>
          <p>
            {settings.executionMode === 'REAL'
              ? 'РЕАЛЬНЫЙ режим: нативные futures-гриды Pionex, take-profit исполняет биржа.'
              : 'PAPER режим: полная симуляция цикла (скан → открытие → ведение → закрытие).'}
            {exchange?.connected
              ? ` Спот: ${Number(exchange.spotUsdtFree || 0).toFixed(2)} USDT свободно · ${Number(exchange.spotUsdtFrozen || 0).toFixed(2)} в ботах/ордерах · фьючерсы: ${Number(exchange.usdtFree || 0).toFixed(2)} USDT.`
              : exchange
                ? ' Баланс биржи недоступен: ' + (exchange.error ?? 'нет аккаунта')
                : ''}
          </p>
        </div>
        <div className="topbar-actions">
          {canOperate && (
            <>
              <button
                className="button primary"
                disabled={busy !== null || settings.status === 'RUNNING'}
                onClick={() => void runAction('start')}
              >
                {busy === 'start' ? 'Запуск…' : 'Запустить'}
              </button>
              <button
                className="button"
                disabled={busy !== null}
                onClick={() => void runAction('scan')}
              >
                {busy === 'scan' ? 'Скан…' : 'Скан сейчас'}
              </button>
              <button
                className="button"
                disabled={busy !== null || settings.status === 'STOPPED'}
                onClick={() => void runAction('stop')}
              >
                {busy === 'stop' ? 'Остановка…' : 'Остановить'}
              </button>
              <button
                className="button danger"
                disabled={busy !== null}
                onClick={() => void runAction('emergency-stop')}
              >
                Emergency Stop
              </button>
            </>
          )}
        </div>
      </div>

      {/* v2.0.88: одна REAL-правда на экране. Раньше здесь стояли «PnL REAL
          (всего)» и «REAL реализованный» — независимые суммы realized, в
          которых NULL-финалы стоп-лоссов считались нулём (+23.43 на экране
          при минусе по кошельку). Теперь единственная REAL-карточка —
          эпохальный агрегат ниже (тот же state.epoch, что на дашборде). */}
      <div className="metric-grid">
        <TotalPnlCard equity={state.epoch ?? null} />
        <Metric
          label={`Активных ботов (${state.activeBots.filter((bot) => bot.source === 'PAPER').length}P / ${state.activeBots.filter((bot) => bot.source === 'REAL').length}R)`}
          value={String(state.activeBots.length)}
        />
        <Metric
          label="PnL СИМУЛЯЦИЯ (всего)"
          value={`${pnl?.paper.totalUsdt ?? '0'} USDT`}
          tone={(Number(pnl?.paper.totalUsdt) || 0) >= 0 ? 'positive' : 'negative'}
          action={
            <button
              type="button"
              className="button secondary small"
              style={{ padding: '2px 8px', fontSize: '11px', lineHeight: '1.2' }}
              disabled={clearingPaper || !canOperate}
              onClick={handleClearPaper}
              title="Очистить историю симуляции и обнулить PnL"
            >
              {clearingPaper ? '…' : 'Очистить'}
            </button>
          }
        />
        <Metric
          label="Баланс биржи · спот (USDT)"
          value={exchange?.connected ? String(Number(exchange.spotUsdtFree || 0).toFixed(2)) : '—'}
          hint={exchange?.connected
            ? `в ботах/ордерах (спот): ${Number(exchange.spotUsdtFrozen || 0).toFixed(2)} · фьючерсы: ${Number(exchange.usdtFree || 0).toFixed(2)}`
            : exchange?.error}
        />
      </div>

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">EXECUTION MODE</span>
            <h3>Режим торговли</h3>
          </div>
          <span className="muted">отдельно от преднастроек · действует на новых ботов</span>
        </div>
        <div className="row-actions" style={{ justifyContent: 'flex-start' }}>
          <span className={`badge ${settings.executionMode === 'REAL' ? 'danger' : 'neutral'}`}>
            {settings.executionMode === 'REAL' ? 'REAL · настоящие деньги' : 'PAPER · симуляция'}
          </span>
          {canOperate && (
            <>
              <button
                className="button"
                disabled={busyMode !== null || settings.executionMode === 'PAPER'}
                onClick={() => void switchMode('PAPER')}
              >
                {busyMode === 'PAPER' ? '…' : 'PAPER'}
              </button>
              <button
                className="button danger"
                disabled={busyMode !== null || settings.executionMode === 'REAL'}
                onClick={() => void switchMode('REAL')}
              >
                {busyMode === 'REAL' ? '…' : 'REAL'}
              </button>
            </>
          )}
        </div>
        <p className="muted compact" style={{ marginBottom: 0 }}>
          Переключается в любой момент, даже при работающем автопилоте: открытые боты остаются
          в своём режиме, новый режим применяется к следующим открытиям. REAL проходит шлюзы
          (kill switch, real_grid_execution_enabled, real_native_grid) и требует верифицированный аккаунт.
        </p>
      </div>

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">MARKET-PHASE PRESETS</span>
            <h3>Преднастройки под фазу рынка</h3>
          </div>
          <span className="muted">
            {running ? 'Остановите автопилот, чтобы применить' : 'Режим, аккаунт и бюджет остаются вашими'}
          </span>
        </div>
        <div className="form-grid">
          {presets.map((preset) => (
            <div className="card-inset" key={preset.id} style={{ display: 'grid', gap: 10 }}>
              <div>
                <span className="badge neutral">{preset.phase}</span>
              </div>
              <div>
                <strong>{preset.title}</strong>
              </div>
              <p className="muted compact" style={{ margin: 0 }}>{preset.description}</p>
              <p className="compact" style={{ margin: 0 }}>
                <strong>Когда: </strong>{preset.whenToUse}
              </p>
              <p className="compact" style={{ margin: 0 }}>
                Плечо {preset.patch.leverage}x ·{' '}
                {preset.patch.pnlTargetUsdt !== undefined && preset.patch.maxLossUsdt !== undefined
                  ? `цель +${preset.patch.pnlTargetUsdt} / стоп −${preset.patch.maxLossUsdt} USDT · `
                  : 'динамические цели · '}
                вола {preset.patch.minVolatilityPct}–{preset.patch.maxVolatilityPct}% ·{' '}
                {preset.patch.maxAdjustmentsPerBot} сдвига
              </p>
              <button
                className="button"
                disabled={!canOperate || running || busyPreset !== null}
                onClick={() => void applyPreset(preset.id)}
              >
                {busyPreset === preset.id ? 'Применение…' : 'Применить'}
              </button>
            </div>
          ))}
        </div>
      </div>

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">SETTINGS</span>
            <h3>Параметры автопилота</h3>
          </div>
          <div className="topbar-actions">
            <button
              className="button"
              disabled={!canOperate}
              onClick={() => setEditing((value) => !value)}
            >
              {editing ? 'Отмена' : 'Изменить'}
            </button>
          </div>
        </div>
        <p className="muted">
          Изменения применяются к новым ботам сразу; открытые боты продолжают жить
          с параметрами, зафиксированными при их создании (цель, стоп, диапазон).
        </p>
        {editing ? (
          <SettingsForm
            settings={settings}
            accounts={accounts}
            onAccountsNeeded={accountsHref}
            onSaved={async () => {
              setEditing(false);
              await load();
            }}
          />
        ) : (
          <SettingsSummary settings={settings} />
        )}
      </div>
    </div>
  );
}

function Metric({
  label,
  value,
  tone,
  hint,
  action,
}: {
  label: React.ReactNode;
  value: string;
  tone?: string;
  hint?: string;
  action?: React.ReactNode;
}) {
  return (
    <div className="metric-card">
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
        <span>{label}</span>
        {action}
      </div>
      <strong className={tone}>{value}</strong>
      {hint && <small className="muted">{hint}</small>}
    </div>
  );
}

// TotalPnlCard is the operator's headline number, the same "Total PnL" the
// Pionex app shows: the epoch aggregate over bots (running grid profit +
// funding + floating, plus closed-of-epoch finals) — the account endpoints
// cannot see isolated-grid margins, so the bots themselves are the only
// truth. v2.0.88: fed from state.epoch so the autopilot hero, the dashboard
// card and the /equity endpoint are ONE number. The breakdown line splits
// the headline into closed / estimated / floating legs and names the
// unknown stop-finals explicitly — counted, never invented. A missing
// summary shows a neutral placeholder: no configured account or a failed
// probe is a data gap, not zero (v2.0.83).
function TotalPnlCard({ equity }: { equity: EquityEpochSummary | null }) {
  if (!equity) {
    return (
      <div className="metric-card metric-card-hero" style={{ gridColumn: 'span 2' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 }}>
          <span>TOTAL PnL</span>
          <span className="badge neutral">нет данных</span>
        </div>
        <strong className="muted">—</strong>
        <small className="muted">
          Агрегат недоступен: нет настроенного аккаунта или ошибка запроса
          (причина — в журнале, событие EQUITY_CAPTURE_FAILED).
        </small>
      </div>
    );
  }
  const fmt = (value: string) => {
    const num = Number(value) || 0;
    return `${num >= 0 ? '+' : ''}${num.toFixed(2)}`;
  };
  const epochPnl = Number(equity.epochPnlUsdt) || 0;
  const capturedAt = equity.capturedAt ? new Date(equity.capturedAt).toLocaleTimeString() : '—';
  return (
    <div className="metric-card metric-card-hero" style={{ gridColumn: 'span 2' }}>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 8 }}>
        <span>TOTAL PnL</span>
        <span className="badge neutral">снапшотов: {equity.snapshots}</span>
      </div>
      <strong className={epochPnl >= 0 ? 'positive' : 'negative'}>
        {epochPnl >= 0 ? '+' : ''}{epochPnl.toFixed(2)} USDT
      </strong>
      <small className="muted">
        Работают: {fmt(equity.runningPnlUsdt)} ({equity.runningBots}) · закрыты {fmt(equity.closedKnownUsdt)}{' '}
        (оценки {fmt(equity.closedEstimatedUsdt)}) · плавающий {fmt(equity.runningFloatingUsdt)} ·
        стоп-минусы неизвестны: {equity.unknownCount}
      </small>
      <small className="muted">
        в ботах {(Number(equity.runningInvestmentUsdt) || 0).toFixed(0)} USDT · снапшот {capturedAt}
      </small>
    </div>
  );
}

function statusTitle(status: string): string {
  switch (status) {
    case 'RUNNING':
      return 'Автопилот работает';
    case 'STARTING':
      return 'Запуск…';
    case 'EMERGENCY_STOPPED':
      return 'Аварийно остановлен';
    default:
      return 'Остановлен';
  }
}

function SettingsSummary({ settings }: { settings: AutoGridSettings }) {
  const rows: Array<[string, string]> = [
    ['Режим', settings.executionMode === 'REAL' ? 'REAL (нативные гриды)' : 'PAPER (симуляция)'],
    ['Бюджет на бота', `${settings.budgetUsdt} USDT`],
    ['Максимум ботов', String(settings.maxActiveBots)],
    ['Плечо', `${settings.leverage}x (${settings.adaptiveLeverageEnabled ? '🛡️ Адаптивное по ATR — защита от сквизов' : '⚡ Фиксированное на все пары'})`],
    ['Режим целей PnL', settings.pnlTargetMode === 'DYNAMIC'
      ? 'ДИНАМИЧЕСКИЙ — по волатильности пары (AI Kit → σ/ATR) и просадке, на каждого бота своя'
      : 'ФИКСИРОВАННЫЙ — одинаковая сумма на всех ботов'],
    ['Цель PnL на бота', settings.pnlTargetMode === 'DYNAMIC'
      ? `динамическая (${settings.pnlTargetUsdt !== '0' ? `фикс-фолбэк ${settings.pnlTargetUsdt}` : 'без фолбэка'})`
      : settings.pnlTargetUsdt !== '0' ? `${settings.pnlTargetUsdt} USDT (нативный profit_amount)` : 'выключена'],
    ['Макс. убыток на бота', settings.pnlTargetMode === 'DYNAMIC'
      ? `динамический (${settings.maxLossUsdt !== '0' ? `фикс-фолбэк ${settings.maxLossUsdt}` : 'без фолбэка'})`
      : settings.maxLossUsdt !== '0' ? `${settings.maxLossUsdt} USDT (stop-out)` : 'выключен'],
    ['Интервал ведения', `${settings.manageIntervalSeconds} c`],
    ['Буфер пробоя диапазона', `${settings.rangeBreakBufferPct}%`],
    ['Сдвигов сетки на бота', String(settings.maxAdjustmentsPerBot)],
    ['Pionex AI Kit', settings.aiKitEnabled ? 'включён (advisory для скана)' : 'выключен'],
    ['AI-автотюнинг', settings.aiAutotuneEnabled
      ? `включён, каждые ${settings.aiAutotuneIntervalSeconds} c${settings.lastAutotuneAt ? ' · ' + new Date(settings.lastAutotuneAt).toLocaleString() : ''}`
      : 'выключен'],
    ['Свечи / история', `${settings.candleInterval} × ${settings.lookbackCandles}`],
    ['Интервал скана', `${settings.scanIntervalSeconds} c`],
    ['Волатильность (мин–макс)', `${settings.minVolatilityPct}% – ${settings.maxVolatilityPct}%`],
    ['Мин. объём 24ч', `${settings.minVolume24h} USDT`],
    ['Стоп-лосс', settings.stopLossMode],
    ['Стоп-радар', settings.stopForecastMode === 'OFF' ? 'выключен' : settings.stopForecastMode],
    ['Радар: автозакрытие', settings.radarAutoCloseMode && settings.radarAutoCloseMode !== 'OFF'
      ? settings.radarAutoCloseMode
      : 'OFF'],
    ['Плотная сетка (geometric)', settings.densityGridEnabled ? 'да' : 'нет'],
    ['Адаптивное плечо', settings.adaptiveLeverageEnabled ? 'да' : 'нет'],
  ];
  return (
    <div>
      {rows.map(([label, value]) => (
        <div className="definition" key={label}>
          <span>{label}</span>
          <strong>{value}</strong>
        </div>
      ))}
    </div>
  );
}

function SettingsForm({
  settings,
  accounts,
  onAccountsNeeded,
  onSaved,
}: {
  settings: AutoGridSettings;
  accounts: Account[];
  onAccountsNeeded: () => void;
  onSaved: () => Promise<void>;
}) {
  const [form, setForm] = useState(() => ({ ...settings }));
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const field = useMemo(
    () => (key: keyof AutoGridSettings) => ({
      value: String(form[key] ?? ''),
      onChange: (event: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
        setForm((current) => ({ ...current, [key]: event.target.value })),
    }),
    [form],
  );

    async function fillFromAIKit() {
    setSaving(true);
    setError(null);
    try {
      const suggestion = await api<{
        sampled: Array<{ symbol: string; volatilityPct: number }>;
        suggested: Record<string, unknown>;
        notes: string[];
      }>('/api/autogrid/settings/ai-fill');
      setForm((current) => ({
        ...current,
        ...suggestion.suggested,
      }));
      setError(null);
      window.alert(
        'AI Kit заполнил поля из ' + suggestion.sampled.length + ' самых ликвидных пар:\n' +
          suggestion.notes.join('\n') +
          '\n\nПроверьте и нажмите «Сохранить настройки».',
      );
    } catch (fillError) {
      setError(describeError(fillError));
    } finally {
      setSaving(false);
    }
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    setError(null);
    try {
      await api('/api/autogrid/settings', {
        method: 'PUT',
        body: JSON.stringify({
          accountId: form.accountId ?? null,
          executionMode: form.executionMode,
          budgetUsdt: dec(form.budgetUsdt),
          minSharpe: dec(form.minSharpe),
          minEvPct: dec(form.minEvPct),
          minVolume24h: dec(form.minVolume24h),
          minVolatilityPct: dec(form.minVolatilityPct),
          maxVolatilityPct: dec(form.maxVolatilityPct),
          maxDrawdownPct: dec(form.maxDrawdownPct),
          minProfitFactor: dec(form.minProfitFactor),
          feeBps: dec(form.feeBps),
          slippageBps: dec(form.slippageBps),
          pnlTargetUsdt: dec(form.pnlTargetUsdt),
          maxLossUsdt: dec(form.maxLossUsdt),
          rangeBreakBufferPct: dec(form.rangeBreakBufferPct),
          stopLossMode: form.stopLossMode,
          candleInterval: form.candleInterval,
          scanMode: form.scanMode || 'TOP_K',
          pnlTargetMode: form.pnlTargetMode,
          smartPnlEnabled: !!form.smartPnlEnabled,
          adaptiveLeverageEnabled: !!form.adaptiveLeverageEnabled,
          densityGridEnabled: !!form.densityGridEnabled,
          stopForecastMode: form.stopForecastMode || 'OFF',
          radarAutoCloseMode: form.radarAutoCloseMode || 'OFF',
          aiKitEnabled: !!form.aiKitEnabled,
          aiAutotuneEnabled: !!form.aiAutotuneEnabled,
          maxActiveBots: numberField(String(form.maxActiveBots), 3),
          leverage: numberField(String(form.leverage), 5),
          lookbackCandles: numberField(String(form.lookbackCandles), 120),
          maxSymbolsPerScan: numberField(String(form.maxSymbolsPerScan), 20),
          scanIntervalSeconds: numberField(String(form.scanIntervalSeconds), 900),
          manageIntervalSeconds: numberField(String(form.manageIntervalSeconds), 60),
          maxAdjustmentsPerBot: numberField(String(form.maxAdjustmentsPerBot), 3),
          aiAutotuneIntervalSeconds: numberField(String(form.aiAutotuneIntervalSeconds ?? 3600), 3600),
        }),
      });
      await onSaved();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setSaving(false);
    }
  }

  return (
    <form className="form-stack" onSubmit={submit}>
      {error && <div className="alert danger">{error}</div>}
      {form.executionMode === 'REAL' && accounts.length === 0 && (
        <div className="alert danger">
          Для REAL-режима нужен верифицированный Pionex API-аккаунт.{' '}
          <button type="button" className="button small" onClick={onAccountsNeeded}>
            Перейти к аккаунтам
          </button>
        </div>
      )}
      <div className="form-grid">
        <label>
          Pionex аккаунт (для REAL)
          <select
            value={form.accountId ?? ''}
            onChange={(event) =>
              setForm((current) => ({ ...current, accountId: event.target.value || null }))}
          >
            <option value="">— не выбран —</option>
            {accounts.map((account) => (
              <option key={account.id} value={account.id}>
                {account.name} {account.isEnabled ? '' : '(отключён)'}
              </option>
            ))}
          </select>
        </label>
        <label>
          Бюджет на бота, USDT
          <input {...field('budgetUsdt')} inputMode="decimal" />
        </label>

        <label>
          Режим целей PnL
          <select
            value={form.pnlTargetMode}
            onChange={(event) =>
              setForm((current) => ({
                ...current,
                pnlTargetMode: event.target.value as AutoGridSettings['pnlTargetMode'],
              }))}
          >
            <option value="DYNAMIC">ДИНАМИЧЕСКИЙ (AI Kit / волатильность + ATR)</option>
            <option value="FIXED">ФИКСИРОВАННЫЙ (свои суммы)</option>
          </select>
        </label>
        <label>
          {form.pnlTargetMode === 'DYNAMIC' ? 'Фикс-фолбэк цели, USDT' : 'Цель PnL на бота, USDT'}
          <input {...field('pnlTargetUsdt')} inputMode="decimal" placeholder="12" />
        </label>
        <label>
          {form.pnlTargetMode === 'DYNAMIC' ? 'Фикс-фолбэк стопа, USDT' : 'Макс. убыток на бота, USDT'}
          <input {...field('maxLossUsdt')} inputMode="decimal" placeholder="8" />
        </label>
        <label>
          Интервал ведения, сек (мин 15)
          <input {...field('manageIntervalSeconds')} inputMode="numeric" />
        </label>
        <label>
          Интервал AI-автотюнинга, сек (мин 300)
          <input
            value={String(form.aiAutotuneIntervalSeconds ?? 3600)}
            onChange={(event) =>
              setForm((current) => ({ ...current, aiAutotuneIntervalSeconds: Number(event.target.value) || 3600 }))}
            inputMode="numeric"
          />
        </label>

        <label>
          Буфер пробоя диапазона, %
          <input {...field('rangeBreakBufferPct')} inputMode="decimal" />
        </label>
        <label>
          Сдвигов сетки на бота (0–10)
          <input {...field('maxAdjustmentsPerBot')} inputMode="numeric" />
        </label>
        <label>
          Максимум ботов
          <input {...field('maxActiveBots')} inputMode="numeric" />
        </label>

        <label>
          Режим плеча
          <select
            value={form.adaptiveLeverageEnabled ? 'ADAPTIVE' : 'FIXED'}
            onChange={(event) =>
              setForm((current) => ({
                ...current,
                adaptiveLeverageEnabled: event.target.value === 'ADAPTIVE',
              }))}
          >
            <option value="ADAPTIVE">🛡️ АДАПТИВНОЕ (ATR Risk Guard — защита от сквизов на волатильных парах)</option>
            <option value="FIXED">⚡ ФИКСИРОВАННОЕ (строго указанное плечо на все пары)</option>
          </select>
        </label>
        <label>
          {form.adaptiveLeverageEnabled ? 'Базовое / Макс. плечо' : 'Фиксированное плечо'}
          <input {...field('leverage')} inputMode="numeric" />
        </label>
        <label>
          Свечи
          <select {...field('candleInterval')}>
            {['5M', '15M', '30M', '60M', '4H', '1D'].map((interval) => (
              <option key={interval} value={interval}>{interval}</option>
            ))}
          </select>
        </label>
        <label>
          История, свечей
          <input {...field('lookbackCandles')} inputMode="numeric" />
        </label>

        <label>
          Интервал скана, сек
          <input {...field('scanIntervalSeconds')} inputMode="numeric" />
        </label>
        <label>
          Режим скана
          <select {...field('scanMode')}>
            <option value="TOP_K">⚡ Быстрый (топ по обороту, ~1 мин)</option>
            <option value="FULL">🔍 Полный (все пары, ~6-10 мин)</option>
          </select>
        </label>
        <label>
          Волатильность мин, %
          <input {...field('minVolatilityPct')} inputMode="decimal" />
        </label>
        <label>
          Волатильность макс, %
          <input {...field('maxVolatilityPct')} inputMode="decimal" />
        </label>

        <label>
          Мин. объём 24ч, USDT
          <input {...field('minVolume24h')} inputMode="decimal" />
        </label>
        <label>
          Мин. EV, %
          <input {...field('minEvPct')} inputMode="decimal" />
        </label>
        <label>
          Мин. Sharpe
          <input {...field('minSharpe')} inputMode="decimal" />
        </label>
      </div>

      <div>
        <div className="toggle-row">
          <div>
            <strong>AI-автотюнинг под рынок</strong>
            <span>
              Пока автопилот работает: периодически пересматривать параметры по AI Kit
              (полоса волатильности, просадка, плечо — шаг ограничен ±30%, плечо ±1)
              {settings.lastAutotuneAt
                ? ` · последняя подстройка: ${new Date(settings.lastAutotuneAt).toLocaleString()}`
                : ''}
              {settings.lastAutotuneNotes ? ` (${settings.lastAutotuneNotes})` : ''}
            </span>
          </div>
          <input
            type="checkbox"
            checked={form.aiAutotuneEnabled}
            onChange={(event) => setForm((current) => ({ ...current, aiAutotuneEnabled: event.target.checked }))}
          />
        </div>
        <div className="toggle-row">
          <div>
            <strong>Pionex AI Kit</strong>
            <span>Прогонять кандидатов скана через нативный AI Kit биржи (advisory)</span>
          </div>
          <input
            type="checkbox"
            checked={form.aiKitEnabled}
            onChange={(event) => setForm((current) => ({ ...current, aiKitEnabled: event.target.checked }))}
          />
        </div>

        <div className="toggle-row">
          <div>
            <strong>Плотная сетка (geometric)</strong>
            <span>Геометрическая сетка вместо арифметической</span>
          </div>
          <input
            type="checkbox"
            checked={form.densityGridEnabled}
            onChange={(event) => setForm((current) => ({ ...current, densityGridEnabled: event.target.checked }))}
          />
        </div>

        <div className="toggle-row">
          <div>
            <strong>Стоп-радар (прогноз стопа)</strong>
            <span>
              Скор риска 0–1 для каждого живого бота: дистанция до стопа, расширение
              волы, дрен альтов по флоту, каскад фандинг/OI/ликвидации.
              SHADOW — только телеметрия и Telegram-предупреждения; ACTIVE —
              превентивные ре-центры сетки при B3/B4 (только PAPER, 1 действие/бот/час).
            </span>
          </div>
          <select
            value={form.stopForecastMode || 'OFF'}
            onChange={(event) => setForm((current) => ({ ...current, stopForecastMode: event.target.value as AutoGridSettings['stopForecastMode'] }))}
          >
            <option value="OFF">Выключен</option>
            <option value="SHADOW">SHADOW — наблюдение</option>
            <option value="ACTIVE">ACTIVE — авто-ре-центры (PAPER)</option>
          </select>
        </div>
        <div className="toggle-row">
          <div>
            <strong>Радар: автозакрытие ботов</strong>
            <span>
              Закрывать бота по сигналу стоп-радара (требует ACTIVE). BAND3 —
              band 3+, бот под водой (floating&lt;0), сигнал держится 3 снапшота,
              возраст 30м+, кулдаун 1ч; STRICT — то же плюс дистанция до стопа
              &lt;0.5 ATR. Боты в плюсе не закрываются никогда. По умолчанию OFF —
              бэктест 09-04: весь плюс политики дал один бот (SNXXX).
            </span>
          </div>
          <select
            value={form.radarAutoCloseMode || 'OFF'}
            onChange={(event) => setForm((current) => ({ ...current, radarAutoCloseMode: event.target.value as AutoGridSettings['radarAutoCloseMode'] }))}
          >
            <option value="OFF">OFF — не закрывать</option>
            <option value="BAND3">BAND3 — band3 + под водой</option>
            <option value="STRICT">STRICT — + близко к стопу</option>
          </select>
        </div>
      </div>

      <div className="panel-actions">
        <div className="row-actions">
          <button
            type="button"
            className="button"
            disabled={saving}
            onClick={() => void fillFromAIKit()}
          >
            {saving ? 'Запрос AI Kit…' : 'Заполнить из AI Kit'}
          </button>
          <span className="muted">
            В DYNAMIC цель/стоп считаются на каждого бота при открытии: 0.5×эффективной волатильности
          (AI Kit пары → σ+ATR сканера) и 0.6×просадки, в % бюджета, с клампами 1.5–12% и 1–8%.
          Исполняются нативно на Pionex (profit_amount) и дублируются циклом ведения.
          </span>
        </div>
        <button className="button primary" type="submit" disabled={saving}>
          {saving ? 'Сохранение…' : 'Сохранить настройки'}
        </button>
      </div>
    </form>
  );
}
