import { useEffect, useState } from 'react';
import { api } from '../api';
import type { AutoGridState, Dashboard } from '../types';

interface Props {
  onRefresh: () => void;
}

export default function Overview({ onRefresh }: Props) {
  const [dashboard, setDashboard] = useState<Dashboard | null>(null);
  const [state, setState] = useState<AutoGridState | null>(null);

  useEffect(() => {
    api<Dashboard>('/api/dashboard').then(setDashboard).catch(() => setDashboard(null));
    api<AutoGridState>('/api/autogrid').then(setState).catch(() => setState(null));
    const timer = window.setInterval(onRefresh, 15000);
    return () => window.clearInterval(timer);
  }, [onRefresh]);

  const pnl = state?.pnl;
  const exchange = state?.exchange;

  return (
    <div className="section-stack">
      <div className="metric-grid">
        <Metric label="Автопилот" value={state?.settings.status ?? '—'} />
        <Metric label="Активных ботов" value={String(state?.activeBots.length ?? 0)} />
        <Metric
          label="PnL СИМУЛЯЦИЯ"
          value={`${pnl?.paper.totalUsdt ?? '0'} USDT`}
          tone={(Number(pnl?.paper.totalUsdt) || 0) >= 0 ? 'positive' : 'negative'}
        />
        <Metric
          label="PnL REAL"
          value={`${pnl?.real.totalUsdt ?? '0'} USDT`}
          tone={(Number(pnl?.real.totalUsdt) || 0) >= 0 ? 'positive' : 'negative'}
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
          {state?.settings.pnlTargetUsdt !== '0' && (
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

function Metric({ label, value, tone, hint }: { label: string; value: string; tone?: string; hint?: string }) {
  return (
    <div className="metric-card">
      <span>{label}</span>
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
