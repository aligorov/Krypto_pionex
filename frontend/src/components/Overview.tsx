import { useCallback, useEffect, useState } from 'react';
import { api, describeError, getCachedAutoGrid, setCachedAutoGrid } from '../api';
import type { AutoGridState, Dashboard } from '../types';
import { SmartDataBadge } from './SmartDataBadge';

interface Props {
  onRefresh: () => void;
  canOperate: boolean;
}

export default function Overview({ onRefresh, canOperate }: Props) {
  const [dashboard, setDashboard] = useState<Dashboard | null>(null);
  const [state, setState] = useState<AutoGridState | null>(() => getCachedAutoGrid<AutoGridState>());
  const [clearError, setClearError] = useState<string | null>(null);
  const [clearingPaper, setClearingPaper] = useState(false);

  // The gates panel and PnL live HERE: this tab must poll into its own
  // state, not kick App's topbar refresh.
  const refresh = useCallback(() => {
    api<Dashboard>('/api/dashboard').then(setDashboard).catch(() => {});
    api<AutoGridState>('/api/autogrid')
      .then((res) => {
        setState(res);
        setCachedAutoGrid(res);
      })
      .catch(() => {});
  }, []);

  useEffect(() => {
    refresh();
    const timer = window.setInterval(refresh, 15000);
    return () => window.clearInterval(timer);
  }, [refresh]);

  const pnl = state?.pnl;
  const epoch = state?.epoch ?? null;
  const exchange = state?.exchange;
  // fmtSigned formats an epoch leg for the breakdown subline.
  const fmtSigned = (value: string | undefined) => {
    const num = Number(value) || 0;
    return `${num >= 0 ? '+' : ''}${num.toFixed(2)}`;
  };
  const epochPnl = Number(epoch?.epochPnlUsdt) || 0;

  async function handleClearPaper() {
    if (!window.confirm('Сбросить историю и накопленный PnL симуляции (PAPER)? Завершенные боты будут удалены.')) {
      return;
    }
    setClearingPaper(true);
    setClearError(null);
    try {
      const res = await api<{ success: boolean; deletedCount: number }>('/api/autogrid/paper/clear', {
        method: 'POST',
        body: JSON.stringify({ includeRunning: false }),
      });
      setClearError(null);
      void refresh();
      void onRefresh();
      window.setTimeout(() => window.alert(`История симуляции очищена (${res.deletedCount} ботов удалено).`), 0);
    } catch (error) {
      setClearError(describeError(error));
    } finally {
      setClearingPaper(false);
    }
  }

  return (
    <div className="section-stack">
      {clearError && <div className="alert danger"><span>Очистка не удалась: {clearError}</span></div>}
      <SmartDataBadge />
      <div className="metric-grid">
        <Metric label="Автопилот" value={state?.settings?.status ?? '—'} />
        <Metric label="Активных ботов" value={String(state?.activeBots.length ?? 0)} />
        <Metric
          label="PnL СИМУЛЯЦИЯ"
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
        {/* v2.0.88 «одна правда на экране»: раньше здесь светился
            pnl.real.totalUsdt — realized-only, где NULL-финалы стоп-лоссов
            считались нулём (+23.43 при реальном минусе). Теперь это тот же
            эпохальный агрегат, что TOTAL PnL на вкладке автопилота
            (state.epoch = AccountEquityEpoch), с breakdown-подстрокой. */}
        <Metric
          label="PnL REAL (эпоха)"
          value={epoch ? `${fmtSigned(epoch.epochPnlUsdt)} USDT` : '—'}
          tone={epoch ? (epochPnl >= 0 ? 'positive' : 'negative') : undefined}
          hint={epoch
            ? `закрыты ${fmtSigned(epoch.closedKnownUsdt)} (оценки ${fmtSigned(epoch.closedEstimatedUsdt)}) · плавающий ${fmtSigned(epoch.runningFloatingUsdt)} · стоп-минусы неизвестны: ${epoch.unknownCount}`
            : 'агрегат эпохи недоступен: нет аккаунта или ошибка запроса (журнал: EQUITY_CAPTURE_FAILED)'}
        />
        <Metric
          label="Баланс биржи · спот (USDT)"
          value={exchange?.connected ? String(Number(exchange.spotUsdtFree || 0).toFixed(2)) : '—'}
          hint={exchange?.connected
            ? `в ботах/ордерах (спот): ${Number(exchange.spotUsdtFrozen || 0).toFixed(2)} · фьючерсы: ${Number(exchange.usdtFree || 0).toFixed(2)}`
            : undefined}
        />
      </div>

      {exchange && (
        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">EXCHANGE BALANCES</span>
              <h3>Все балансы биржи</h3>
            </div>
            <span className="muted">
              {exchange.connected
                ? `аккаунт «${exchange.accountName}» · обновлено ${new Date(exchange.updatedAt).toLocaleTimeString()}`
                : `недоступно: ${exchange.error ?? 'нет аккаунта'}`}
            </span>
          </div>
          <div className="two-column">
            <div>
              <h4 style={{ margin: '0 0 10px' }}>Спот (инвестиции грид-ботов берутся отсюда)</h4>
              <div className="table-wrap" style={{ margin: 0 }}>
                <table>
                  <thead><tr><th>Монета</th><th>Свободно</th><th>В ботах/ордерах</th></tr></thead>
                  <tbody>
                    {sortBalances((exchange.spotCoins ?? []).map((coin) => ({
                      coin: coin.coin, free: coin.free, frozen: coin.frozen,
                    }))).map((coin) => (
                      <tr key={coin.coin} className={coin.coin === 'USDT' ? 'positive' : ''}>
                        <td><strong>{coin.coin}</strong></td>
                        <td>{trimNum(coin.free)}</td>
                        <td>{trimNum(coin.frozen)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            </div>
            <div>
              <h4 style={{ margin: '0 0 10px' }}>Фьючерсы (маржа/позиции)</h4>
              <div className="table-wrap" style={{ margin: 0 }}>
                <table>
                  <thead><tr><th>Монета</th><th>Свободно</th><th>Заморожено</th><th>Долг</th></tr></thead>
                  <tbody>
                    {sortBalances((exchange.coins ?? []).map((coin) => ({
                      coin: coin.coin, free: coin.free, frozen: coin.frozen,
                    }))).map((coin) => {
                      const full = (exchange.coins ?? []).find((item) => item.coin === coin.coin);
                      return (
                        <tr key={coin.coin} className={coin.coin === 'USDT' ? 'positive' : ''}>
                          <td><strong>{coin.coin}</strong></td>
                          <td>{trimNum(coin.free)}</td>
                          <td>{trimNum(coin.frozen)}</td>
                          <td>{trimNum(full?.debts)}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
          <p className="muted compact" style={{ marginBottom: 0 }}>
            Свободный спот-USDT — источник инвестиций новых гридов (investmentFrom: USER);
            замороженный — уже работает в ваших ботах/ордерах на Pionex.
          </p>
        </div>
      )}

      <div className="two-column">
        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">EXECUTION GATES</span>
              <h3>Шлюзы реального исполнения</h3>
            </div>
          </div>
          <Gate label="Kill switch" on={dashboard?.killSwitchEnabled ?? true} />
          <Gate label="real_grid_execution_enabled" on={dashboard?.realGridEnabled ?? false} invert />
          <Gate label="База данных" on={dashboard?.databaseHealthy ?? false} />
          <Gate label="Активных аккаунтов Pionex" on={(dashboard?.activeAccounts ?? 0) > 0} />
          <p className="muted compact">
            REAL-гриды создаются только при выключенном kill switch и включённом
            real_grid_execution_enabled (конфиг и feature flag в PostgreSQL).
          </p>
        </div>

        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">STRATEGY</span>
              <h3>Как работает автопилот</h3>
            </div>
          </div>
          <div className="definition"><span>1. Скан пар</span><strong>волатильность · ADX/EMA · объём</strong></div>
          <div className="definition"><span>2. AI Kit биржи</span><strong>нативный aiStrategy (advisory)</strong></div>
          <div className="definition"><span>3. Валидация</span><strong>нативный checkParams</strong></div>
          <div className="definition"><span>4. Открытие</span><strong>futuresGrid/create + profit_amount TP</strong></div>
          <div className="definition"><span>5. Ведение</span><strong>reconcile · PnL · adjustParams</strong></div>
          <div className="definition"><span>6. Закрытие</span><strong>цель PnL / стоп / пробой → cancel</strong></div>
          {state?.settings?.pnlTargetUsdt && state.settings.pnlTargetUsdt !== '0' && (
            <p className="muted compact">
              Цель каждого бота: +{state?.settings.pnlTargetUsdt} USDT — исполняется нативно на Pionex
              и дублируется циклом ведения.
            </p>
          )}
        </div>
      </div>

      {dashboard && !dashboard.databaseHealthy && (
        <div className="alert danger">
          <span>База данных недоступна — проверьте состояние PostgreSQL.</span>
        </div>
      )}
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

function Gate({ label, on, invert }: { label: string; on: boolean; invert?: boolean }) {
  const healthy = invert ? !on : on;
  return (
    <div className="definition">
      <span>{label}</span>
      <span className={`badge ${healthy ? 'success' : 'danger'}`}>{on ? 'ON' : 'OFF'}</span>
    </div>
  );
}

function sortBalances(coins: Array<{ coin: string; free: string; frozen: string }>) {
  return [...coins].sort((a, b) => {
    if (a.coin === 'USDT') return -1;
    if (b.coin === 'USDT') return 1;
    const aActive = Number(a.free) > 0 || Number(a.frozen) > 0;
    const bActive = Number(b.free) > 0 || Number(b.frozen) > 0;
    if (aActive !== bActive) return aActive ? -1 : 1;
    return a.coin.localeCompare(b.coin);
  });
}

function trimNum(value: string | undefined): string {
  const parsed = Number(value ?? 0);
  if (!Number.isFinite(parsed)) return value ?? '—';
  if (parsed === 0) return '0';
  if (Math.abs(parsed) >= 1000) return parsed.toFixed(2);
  if (Math.abs(parsed) >= 1) return parsed.toFixed(4);
  return parsed.toPrecision(6);
}
