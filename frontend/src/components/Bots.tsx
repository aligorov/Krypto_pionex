import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { describeError } from './AutoGridAutopilot';
import { CandlestickChart } from './CandlestickChart';
import type { AIKitResponse, AutoGridClosedBot, AutoGridBot, AutoGridState } from '../types';

interface Props {
  canOperate: boolean;
}

const closableStatuses = ['RUNNING', 'PENDING_SUBMISSION', 'SUBMISSION_UNKNOWN', 'STOP_REQUESTED'];

export default function Bots({ canOperate }: Props) {
  const [state, setState] = useState<AutoGridState | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selectedBotForChart, setSelectedBotForChart] = useState<AutoGridBot | null>(null);

  const load = useCallback(async () => {
    try {
      setState(await api<AutoGridState>('/api/autogrid'));
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 3000);
    return () => window.clearInterval(timer);
  }, [load]);

  if (!state) {
    return <div className="empty-state">{error ?? 'Загрузка ботов…'}</div>;
  }

  return (
    <div className="section-stack">
      {error && (
        <div className="alert danger">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      {selectedBotForChart && (
        <div
          style={{
            position: 'fixed',
            inset: 0,
            backgroundColor: 'rgba(0, 0, 0, 0.75)',
            zIndex: 1000,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
            padding: '24px',
          }}
          onClick={() => setSelectedBotForChart(null)}
        >
          <div
            style={{
              width: '1000px',
              maxWidth: '95vw',
              height: '600px',
              maxHeight: '90vh',
              borderRadius: '12px',
              overflow: 'hidden',
              boxShadow: '0 25px 50px -12px rgba(0, 0, 0, 0.5)',
            }}
            onClick={(e) => e.stopPropagation()}
          >
            <CandlestickChart
              symbol={selectedBotForChart.symbol}
              lowerPrice={Number(selectedBotForChart.lowerPrice)}
              upperPrice={Number(selectedBotForChart.upperPrice)}
              direction={selectedBotForChart.direction}
              gridCount={selectedBotForChart.gridNum}
              onClose={() => setSelectedBotForChart(null)}
            />
          </div>
        </div>
      )}

      {canOperate && <ManualDeployPanel onDeployed={load} state={state} />}

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">ACTIVE BOTS</span>
            <h3>Активные боты ({state.activeBots.length})</h3>
          </div>
          <div style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
            <span className="muted">PnL обновляется циклом reconcile</span>
            <button className="button secondary" style={{ padding: '4px 10px' }} onClick={() => void load()}>
              🔄 Обновить
            </button>
          </div>
        </div>
        {state.activeBots.length === 0 ? (
          <div className="empty-state">Активных ботов нет — запустите автопилот или сделайте скан.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Символ</th>
                  <th>Источник</th>
                  <th>Статус</th>
                  <th>Направление</th>
                  <th>Диапазон</th>
                  <th>Плечо</th>
                  <th>Инвест.</th>
                  <th>Реализ. PnL</th>
                  <th>Нереализ. PnL</th>
                  <th>Всего</th>
                  <th>Цель / стоп</th>
                  <th>Сдвиги</th>
                  {canOperate && <th></th>}
                </tr>
              </thead>
              <tbody>
                {state.activeBots.map((bot) => (
                  <BotRow
                    key={bot.id}
                    bot={bot}
                    canOperate={canOperate}
                    onClosed={load}
                    onOpenChart={() => setSelectedBotForChart(bot)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
        {canOperate && (
          <div className="panel-actions">
            <span className="muted">
              «Закрыть» ставит durable stop-intent; reconcile отправляет нативный cancel на Pionex и
              подтверждает терминальный статус удалённо.
            </span>
          </div>
        )}
      </div>

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">CLOSED BOTS</span>
            <h3>Закрытые боты ({state.closedBots.length})</h3>
          </div>
          <span className="muted">
            PAPER: {state.pnl.paper.closedBots} закр. / {state.pnl.paper.realizedUsdt} USDT · REAL:{' '}
            {state.pnl.real.closedBots} закр. / {state.pnl.real.realizedUsdt} USDT (прибыльных{' '}
            {state.pnl.real.profitable})
          </span>
        </div>
        {state.closedBots.length === 0 ? (
          <div className="empty-state">Закрытых ботов пока нет.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Символ</th>
                  <th>Источник</th>
                  <th>Причина закрытия</th>
                  <th>Направление</th>
                  <th>Инвест.</th>
                  <th>Итоговый PnL</th>
                  <th>Закрыт</th>
                </tr>
              </thead>
              <tbody>
                {state.closedBots.map((bot) => (
                  <ClosedBotRow key={`${bot.source}-${bot.id}`} bot={bot} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}

function BotRow({
  bot,
  canOperate,
  onClosed,
  onOpenChart,
}: {
  bot: AutoGridBot;
  canOperate: boolean;
  onClosed: () => Promise<void>;
  onOpenChart: () => void;
}) {
  const [closing, setClosing] = useState(false);
  const [adjusting, setAdjusting] = useState(false);
  const [adjustMode, setAdjustMode] = useState<'invest_in' | 'adjust_params'>('invest_in');
  const [quoteInvestment, setQuoteInvestment] = useState('');
  const [lower, setLower] = useState(bot.lowerPrice);
  const [upper, setUpper] = useState(bot.upperPrice);
  const [row, setRow] = useState(String(bot.gridNum));
  const [adjustError, setAdjustError] = useState<string | null>(null);
  const realized = Number(bot.realizedPnlUsdt) || 0;
  const unrealized = Number(bot.unrealizedPnlUsdt) || 0;
  const total = realized + unrealized;
  const pnlClass = (value: number) => (value > 0 ? 'positive' : value < 0 ? 'negative' : '');

  async function close() {
    if (
      !window.confirm(
        `Закрыть бот ${bot.symbol} (${bot.source})? Реальный бот будет отменён нативным API Pionex, позиция закроется в USDT.`,
      )
    ) {
      return;
    }
    setClosing(true);
    try {
      await api(`/api/autogrid/bots/${bot.id}/close`, { method: 'POST' });
      await onClosed();
    } catch {
      /* surfaced by the parent reload */
    } finally {
      setClosing(false);
    }
  }

  async function submitAdjust(event: React.FormEvent) {
    event.preventDefault();
    setAdjustError(null);
    const body: Record<string, unknown> = { mode: adjustMode };
    if (adjustMode === 'invest_in') {
      body.quoteInvestment = quoteInvestment;
    } else {
      body.lower = lower;
      body.upper = upper;
      body.row = Number(row) || bot.gridNum;
    }
    try {
      await api(`/api/autogrid/bots/${bot.id}/adjust`, {
        method: 'POST',
        body: JSON.stringify(body),
      });
      setAdjusting(false);
      await onClosed();
    } catch (error) {
      setAdjustError(describeError(error));
    }
  }

  return (
    <>
      <tr>
        <td>
          <strong
            style={{ cursor: 'pointer', color: '#38bdf8' }}
            onClick={onOpenChart}
            title="Открыть интерактивный график"
          >
            {bot.symbol}
          </strong>
        </td>
        <td><span className={`badge ${bot.source === 'REAL' ? 'danger' : 'neutral'}`}>{bot.source}</span></td>
        <td>
          <span className={`badge ${bot.status === 'RUNNING' ? 'success' : 'warning'}`}>{bot.status}</span>
          <small>{bot.reconciliationState}</small>
        </td>
        <td>{bot.direction}</td>
        <td>
          {bot.lowerPrice} – {bot.upperPrice}
          <small>{bot.gridNum} уровней</small>
        </td>
        <td>
          <div style={{ display: 'flex', flexDirection: 'column', gap: '2px' }}>
            <span>
              <strong>{bot.leverage}x</strong>
              {bot.leverageMode === 'ADAPTIVE' && bot.baseLeverage && bot.leverage < bot.baseLeverage ? (
                <span title={bot.leverageReason} style={{ marginLeft: 4, cursor: 'help' }}>🛡️</span>
              ) : (
                <span title={bot.leverageReason || 'Базовое плечо'} style={{ marginLeft: 4 }}>🟢</span>
              )}
            </span>
            {bot.leverageReason && (
              <span style={{ fontSize: '0.70rem', color: '#94a3b8', whiteSpace: 'nowrap' }} title={bot.leverageReason}>
                {bot.leverageReason}
              </span>
            )}
          </div>
        </td>
        <td>{bot.quoteInvestment}</td>
        <td className={pnlClass(realized)}>{bot.realizedPnlUsdt ?? '—'}</td>
        <td className={pnlClass(unrealized)}>{bot.unrealizedPnlUsdt ?? '—'}</td>
        <td className={pnlClass(total)}><strong>{total.toFixed(4)}</strong></td>
        <td>{bot.adjustmentsCount}</td>
        {canOperate && (
          <td>
            <div className="row-actions">
              <button
                className="button small"
                onClick={onOpenChart}
                title="Посмотреть свечной график"
              >
                📊
              </button>
              <button
                className="button small"
                disabled={adjusting || bot.status !== 'RUNNING'}
                onClick={() => setAdjusting((value) => !value)}
              >
                Вести
              </button>
              <button
                className="button small danger"
                disabled={closing || !closableStatuses.includes(bot.status)}
                onClick={() => void close()}
              >
                {closing ? '…' : 'Закрыть'}
              </button>
            </div>
          </td>
        )}
      </tr>
      {adjusting && (
        <tr>
          <td colSpan={12}>
            <form className="inline-form" onSubmit={submitAdjust}>
              <select
                value={adjustMode}
                onChange={(event) => setAdjustMode(event.target.value as 'invest_in' | 'adjust_params')}
                style={{ maxWidth: 200 }}
              >
                <option value="invest_in">Довложить USDT (invest_in)</option>
                <option value="adjust_params">Сдвинуть диапазон (adjust_params)</option>
              </select>
              {adjustMode === 'invest_in' ? (
                <input
                  placeholder="Сумма, USDT"
                  value={quoteInvestment}
                  onChange={(event) => setQuoteInvestment(event.target.value)}
                  inputMode="decimal"
                  required
                />
              ) : (
                <>
                  <input
                    placeholder="Нижняя граница"
                    value={lower}
                    onChange={(event) => setLower(event.target.value)}
                    inputMode="decimal"
                    required
                  />
                  <input
                    placeholder="Верхняя граница"
                    value={upper}
                    onChange={(event) => setUpper(event.target.value)}
                    inputMode="decimal"
                    required
                  />
                  <input
                    placeholder="Уровней"
                    value={row}
                    onChange={(event) => setRow(event.target.value)}
                    inputMode="numeric"
                    style={{ maxWidth: 110 }}
                  />
                </>
              )}
              <button className="button primary" type="submit">Применить</button>
              <button className="button ghost" type="button" onClick={() => setAdjusting(false)}>Отмена</button>
              {adjustError && <span className="negative compact">{adjustError}</span>}
            </form>
          </td>
        </tr>
      )}
    </>
  );
}

function ClosedBotRow({ bot }: { bot: AutoGridClosedBot }) {
  const pnl = Number(bot.realizedPnlUsdt) || 0;
  return (
    <tr>
      <td><strong>{bot.symbol}</strong></td>
      <td><span className={`badge ${bot.source === 'REAL' ? 'danger' : 'neutral'}`}>{bot.source}</span></td>
      <td>
        <span className={`badge ${reasonBadge(bot.closedReason)}`}>{bot.closedReason ?? bot.status}</span>
      </td>
      <td>{bot.direction}</td>
      <td>{bot.quoteInvestment}</td>
      <td className={pnl > 0 ? 'positive' : pnl < 0 ? 'negative' : ''}>
        <strong>{bot.realizedPnlUsdt ?? '—'}</strong>
      </td>
      <td>{bot.closedAt ? new Date(bot.closedAt).toLocaleString() : '—'}</td>
    </tr>
  );
}

function reasonBadge(reason: string | null): string {
  if (!reason) return 'neutral';
  if (reason.startsWith('TAKE_PROFIT')) return 'success';
  if (reason.startsWith('STOP_LOSS') || reason.startsWith('RANGE_BREAK') || reason.startsWith('LIQUID')) {
    return 'danger';
  }
  return 'warning';
}

function ManualDeployPanel({
  onDeployed,
  state,
}: {
  onDeployed: () => Promise<void>;
  state: AutoGridState;
}) {
  const [symbol, setSymbol] = useState('');
  const [mode, setMode] = useState<'AUTO' | 'PAPER' | 'REAL'>('AUTO');
  const [direction, setDirection] = useState('NEUTRAL');
  const [leverage, setLeverage] = useState('');
  const [lower, setLower] = useState('');
  const [upper, setUpper] = useState('');
  const [row, setRow] = useState('');
  const [rangeSource, setRangeSource] = useState('MANUAL');
  const [hint, setHint] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  function prefillFromScanner() {
    const candidate = state.candidates.find(
      (item) => item.decision === 'ACCEPTED' && (!symbol || item.symbol === symbol),
    );
    if (!candidate) {
      setHint('Принятых кандидатов сканера нет — запустите скан или заполните вручную.');
      return;
    }
    setSymbol(candidate.symbol);
    setDirection(
      candidate.recommendedTrend === 'long'
        ? 'LONG'
        : candidate.recommendedTrend === 'short'
          ? 'SHORT'
          : 'NEUTRAL',
    );
    setLeverage(String(candidate.recommendedLeverage));
    setLower(candidate.lowerPrice);
    setUpper(candidate.upperPrice);
    setRow(String(candidate.gridNum));
    setRangeSource('SCANNER');
    setHint(`Взято из последнего скана: ${candidate.symbol} (score ${Number(candidate.score).toFixed(3)}).`);
  }

  async function prefillFromAIKit() {
    if (!symbol.trim().toUpperCase().endsWith('_PERP')) {
      setHint('Введите символ вида BTC_USDT_PERP, затем нажмите «Из AI Kit».');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const data = await api<AIKitResponse>(
        `/api/autogrid/ai-strategy?symbol=${encodeURIComponent(symbol.trim().toUpperCase())}`,
      );
      setLower(data.futuresAdapted.lower);
      setUpper(data.futuresAdapted.upper);
      setRow(String(data.futuresAdapted.gridCount || 20));
      setRangeSource('AI_KIT');
      setHint(
        `AI Kit: год. ${data.strategy.annualized}, волат. ${data.strategy.volatility}. ` +
          `${data.futuresAdapted.note} — можно изменить любые поля.`,
      );
    } catch (fetchError) {
      setError(describeError(fetchError));
    } finally {
      setBusy(false);
    }
  }

  async function deploy(event: React.FormEvent) {
    event.preventDefault();
    if (mode === 'REAL' && !window.confirm(
      `Открыть РЕАЛЬНОГО бота ${symbol.trim().toUpperCase()} на настоящие деньги Pionex?`,
    )) {
      return;
    }
    setBusy(true);
    setError(null);
    setHint(null);
    try {
      const result = await api<{ source: string }>('/api/autogrid/bots', {
        method: 'POST',
        body: JSON.stringify({
          symbol: symbol.trim().toUpperCase(),
          mode: mode === 'AUTO' ? '' : mode,
          direction,
          leverage: leverage ? Number(leverage) : 0,
          lower: lower || '0',
          upper: upper || '0',
          row: row ? Number(row) : 0,
          rangeSource,
        }),
      });
      setHint(
        `Бот открыт (${result.source === 'REAL' ? 'РЕАЛЬНЫЙ' : 'симуляция'}). ` +
          'Пустые поля возьмутся из рекомендаций сканера.',
      );
      await onDeployed();
    } catch (deployError) {
      setError(describeError(deployError));
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="panel" onSubmit={deploy}>
      <div className="panel-heading">
        <div>
          <span className="eyebrow">MANUAL DEPLOY</span>
          <h3>Открыть бота вручную</h3>
        </div>
        <span className="muted">Рекомендации AI Kit / сканера — любое поле можно переопределить</span>
      </div>
      {error && <div className="alert danger" style={{ marginTop: 0 }}><span>{error}</span></div>}
      {hint && <div className="alert success" style={{ marginTop: 0 }}><span>{hint}</span></div>}
      <div className="form-grid">
        <label>
          Режим этого бота
          <select
            value={mode}
            onChange={(event) => setMode(event.target.value as 'AUTO' | 'PAPER' | 'REAL')}
          >
            <option value="AUTO">Как в настройках автопилота</option>
            <option value="PAPER">PAPER — симуляция</option>
            <option value="REAL">REAL — настоящие деньги</option>
          </select>
        </label>
        <label>
          Символ (PERP)
          <input
            value={symbol}
            onChange={(event) => setSymbol(event.target.value)}
            placeholder="BTC_USDT_PERP"
            required
          />
        </label>
        <label>
          Направление
          <select value={direction} onChange={(event) => setDirection(event.target.value)}>
            <option value="NEUTRAL">NEUTRAL (нейтральный)</option>
            <option value="LONG">LONG (long-грид)</option>
            <option value="SHORT">SHORT (short-грид)</option>
          </select>
        </label>
        <label>
          Плечо (пусто = из настроек/сканера)
          <input value={leverage} onChange={(event) => setLeverage(event.target.value)} inputMode="numeric" />
        </label>
        <label>
          Нижняя граница
          <input value={lower} onChange={(event) => setLower(event.target.value)} inputMode="decimal" />
        </label>
        <label>
          Верхняя граница
          <input value={upper} onChange={(event) => setUpper(event.target.value)} inputMode="decimal" />
        </label>
        <label>
          Уровней (пусто = AI Kit / ATR)
          <input value={row} onChange={(event) => setRow(event.target.value)} inputMode="numeric" />
        </label>
      </div>
      <div className="panel-actions">
        <div className="row-actions">
          <button type="button" className="button" disabled={busy} onClick={prefillFromScanner}>
            Подставить из сканера
          </button>
          <button type="button" className="button" disabled={busy} onClick={() => void prefillFromAIKit()}>
            Диапазон из AI Kit (адаптированный)
          </button>
        </div>
        <button className="button primary" type="submit" disabled={busy || symbol.trim() === ''}>
          {busy ? 'Открытие…' : 'Открыть бота'}
        </button>
      </div>
      <p className="muted compact">
        {mode === 'REAL'
          ? 'REAL: перед созданием Pionex проверит параметры нативным checkParams; шлюзы real_grid_execution_enabled + real_native_grid и kill switch должны быть открыты. '
          : ''}
        AI Kit по документации Pionex — Spot-only: ширина диапазона берётся из рекомендации биржи,
        центр — по цене PERP, лимит ±12.5%; перед REAL созданием Pionex сам проверит параметры
        (checkParams, мин. инвестиция, оценка ликвидации).
      </p>
    </form>
  );
}
