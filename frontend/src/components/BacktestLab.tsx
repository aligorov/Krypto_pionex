import { useCallback, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { BacktestJob } from '../types';

interface Props {
  canOperate: boolean;
}

const INTERVALS = ['15M', '30M', '60M', '1H', '4H', '1D'];

const STATUS_BADGES: Record<string, string> = {
  QUEUED: 'badge neutral',
  RUNNING: 'badge',
  DONE: 'badge success',
  FAILED: 'badge danger',
};

export default function BacktestLab({ canOperate }: Props) {
  const [jobs, setJobs] = useState<BacktestJob[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [symbol, setSymbol] = useState('BTC_USDT_PERP');
  const [tf, setTf] = useState('60M');
  const [stopLossPct, setStopLossPct] = useState(8);
  const [submitting, setSubmitting] = useState(false);
  const [notice, setNotice] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await api<{ data: BacktestJob[] }>('/api/backtest/jobs');
      setJobs(res.data ?? []);
      setError(null);
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 5000);
    return () => window.clearInterval(timer);
  }, [load]);

  async function submit() {
    setSubmitting(true);
    setNotice(null);
    try {
      await api('/api/backtest/jobs', {
        method: 'POST',
        body: JSON.stringify({
          symbol: symbol.trim().toUpperCase(),
          interval: tf,
          params: { stop_loss_pct: stopLossPct },
        }),
      });
      setNotice(`Задача для ${symbol.trim().toUpperCase()} поставлена в очередь — результат через ~10-30 секунд.`);
      void load();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setSubmitting(false);
    }
  }

  return (
    <div className="section">
      <div className="section-header" style={{ marginBottom: '1rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <span style={{ fontSize: '1.75rem' }}>🔬</span>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.4rem', fontWeight: 700 }}>Бэктест-лаборатория</h2>
            <p style={{ margin: '0.2rem 0 0', color: 'var(--text-secondary)', fontSize: '0.85rem' }}>
              Purged walk-forward симуляция сетки на истории Pionex: параметры подбираются на train-окне,
              результат считается строго out-of-sample
            </p>
          </div>
        </div>
      </div>

      {error && <div className="banner error" style={{ marginBottom: '1rem' }}>{error}</div>}
      {notice && <div className="banner success" style={{ marginBottom: '1rem' }}>{notice}</div>}

      <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
        <div style={{ display: 'flex', gap: '1rem', flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>Символ</label>
            <input
              className="input"
              value={symbol}
              onChange={(e) => setSymbol(e.target.value)}
              placeholder="BTC_USDT_PERP"
              style={{ width: '220px' }}
            />
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>Таймфрейм</label>
            <select className="input" value={tf} onChange={(e) => setTf(e.target.value)} style={{ width: '110px' }}>
              {INTERVALS.map((item) => (
                <option key={item} value={item}>{item}</option>
              ))}
            </select>
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>
              Стоп-лосс, % (за нижней границей)
            </label>
            <input
              className="input"
              type="number"
              min={1}
              max={30}
              step={0.5}
              value={stopLossPct}
              onChange={(e) => setStopLossPct(Number(e.target.value))}
              style={{ width: '110px' }}
            />
          </div>
          <button
            className="btn btn-primary"
            onClick={() => void submit()}
            disabled={submitting || !canOperate || symbol.trim() === ''}
            title={canOperate ? '' : 'Требуется роль OPERATOR'}
          >
            {submitting ? 'Ставлю в очередь…' : '▶ Запустить бэктест'}
          </button>
        </div>
        <small className="muted" style={{ display: 'block', marginTop: '0.75rem' }}>
          500 свечей выбранного таймфрейма, окно обучения 240 / теста 60 / зазор 6, комиссия maker 0.02%.
          Результат — средний OOS-процент по фолдам, максимальная просадка и количество стоп-срабатываний.
        </small>
      </div>

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">ИСТОРИЯ</span>
            <h3>Задачи ({jobs.length})</h3>
          </div>
        </div>
        <div style={{ overflowX: 'auto' }}>
          <table className="table">
            <thead>
              <tr>
                <th>Создана</th>
                <th>Символ</th>
                <th>TF</th>
                <th>Статус</th>
                <th>Фолдов</th>
                <th>OOS доход</th>
                <th>Max DD</th>
                <th>Сделок</th>
                <th>Стопов</th>
                <th>Детали</th>
              </tr>
            </thead>
            <tbody>
              {jobs.length === 0 && (
                <tr>
                  <td colSpan={10} style={{ textAlign: 'center', padding: '1.5rem', color: 'var(--text-secondary)' }}>
                    Задач пока нет — запусти первый бэктест выше
                  </td>
                </tr>
              )}
              {jobs.map((job) => {
                const result = job.result ?? {};
                const oos = Number(result['oos_return_pct'] ?? 0);
                const dd = Number(result['oos_max_drawdown'] ?? 0);
                return (
                  <tr key={job.id}>
                    <td><span className="badge neutral">{new Date(job.createdAt).toLocaleTimeString()}</span></td>
                    <td><strong>{job.symbol}</strong></td>
                    <td>{job.interval}</td>
                    <td><span className={STATUS_BADGES[job.status] ?? 'badge neutral'}>{job.status}</span></td>
                    <td>{String(result['folds'] ?? '—')}</td>
                    <td style={{ color: oos > 0 ? '#34d399' : oos < 0 ? '#f87171' : undefined }}>
                      {job.status === 'DONE' ? `${oos > 0 ? '+' : ''}${oos.toFixed(2)}%` : '—'}
                    </td>
                    <td>{job.status === 'DONE' ? `${(dd * 100).toFixed(2)}%` : '—'}</td>
                    <td>{String(result['round_trips'] ?? '—')}</td>
                    <td>{String(result['stop_hits'] ?? '—')}</td>
                    <td
                      style={{ cursor: 'help', maxWidth: '320px', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                      title={job.error ?? JSON.stringify(result, null, 2) ?? ''}
                    >
                      <small className="muted">
                        {job.error ? `⚠ ${job.error.slice(0, 60)}` : job.status === 'DONE' ? 'наведи для деталей' : '—'}
                      </small>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
