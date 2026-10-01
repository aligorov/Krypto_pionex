import { useCallback, useEffect, useMemo, useState } from 'react';
import { api, describeError } from '../api';
import type { EntryDecision, ReplayRun, ReplayRunItem } from '../types';

// «Replay гейтов» (план §7, экран 2): прогон записанной ленты решений с
// изменённым порогом одного гейта — или нулевой override («Валидация
// модели»), который обязан воспроизводить сохранённые вердикты. Прогоны
// неизменяемы; ничего торгового не происходит — только эксперимент в PG.

interface Props {
  canOperate: boolean;
}

const RUN_STATUS_BADGES: Record<string, string> = {
  QUEUED: 'badge neutral',
  RUNNING: 'badge warning',
  DONE: 'badge success',
  FAILED: 'badge danger',
  CANCELLED: 'badge neutral',
};

const ITEM_VERDICT_STYLES: Record<string, { label: string; cls: string }> = {
  REPRODUCED: { label: 'воспроизведён', cls: 'badge success' },
  CHANGED: { label: 'изменился', cls: 'badge warning' },
  NOT_REPLAYABLE: { label: 'не переигрывается', cls: 'badge neutral' },
};

function fmtUsd(value: unknown): string {
  const num = Number(value);
  if (!Number.isFinite(num)) return '—';
  return `${num > 0 ? '+' : ''}${num.toFixed(2)} $`;
}

function fmtNum(value: unknown): string {
  const num = Number(value);
  if (!Number.isFinite(num)) return '—';
  return String(num);
}

function toLocalInputValue(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0');
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

interface RunDetail {
  run: ReplayRun;
  items: ReplayRunItem[];
}

export default function ReplayLab({ canOperate }: Props) {
  const [gate, setGate] = useState('');
  const [gateOptions, setGateOptions] = useState<string[]>([]);
  const [threshold, setThreshold] = useState('');
  const [from, setFrom] = useState(() => toLocalInputValue(new Date(Date.now() - 24 * 3600 * 1000)));
  const [to, setTo] = useState(() => toLocalInputValue(new Date()));
  const [validation, setValidation] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [runs, setRuns] = useState<ReplayRun[]>([]);
  const [selected, setSelected] = useState<RunDetail | null>(null);
  const [detailError, setDetailError] = useState<string | null>(null);

  const loadRuns = useCallback(async () => {
    try {
      const res = await api<{ data: ReplayRun[] }>('/api/autogrid/replay/runs?limit=20');
      setRuns(res.data ?? []);
      setError(null);
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, []);

  // Список гейтов — реально решавшие в журнале (без угадывания имён).
  useEffect(() => {
    api<{ data: EntryDecision[] }>('/api/autogrid/decisions?limit=200')
      .then((res) => {
        const codes = new Set<string>((res.data ?? []).map((d) => d.code).filter(Boolean));
        setGateOptions([...codes].sort());
      })
      .catch(() => setGateOptions([]));
  }, []);

  useEffect(() => {
    void loadRuns();
    const timer = window.setInterval(() => void loadRuns(), 5000);
    return () => window.clearInterval(timer);
  }, [loadRuns]);

  const openRun = useCallback(async (id: string) => {
    setDetailError(null);
    try {
      const res = await api<RunDetail>(`/api/autogrid/replay/runs/${id}`);
      setSelected(res);
    } catch (openError) {
      setDetailError(describeError(openError));
    }
  }, []);

  // Пока открытый прогон в работе — обновляем и его (статусы/статистика).
  useEffect(() => {
    if (!selected || (selected.run.status !== 'QUEUED' && selected.run.status !== 'RUNNING')) return;
    const timer = window.setInterval(() => void openRun(selected.run.id), 5000);
    return () => window.clearInterval(timer);
  }, [selected, openRun]);

  async function submit() {
    setSubmitting(true);
    setNotice(null);
    setError(null);
    try {
      const body: Record<string, unknown> = {
        from: new Date(from).toISOString(),
        to: new Date(to).toISOString(),
      };
      if (validation) {
        body['validation'] = true;
      } else {
        body['gate'] = gate;
        const params: Record<string, unknown> = {};
        const parsed = Number(threshold);
        if (threshold.trim() !== '' && Number.isFinite(parsed)) {
          params['threshold'] = parsed;
        }
        body['params'] = params;
      }
      const res = await api<{ runId: string }>('/api/autogrid/replay/run', {
        method: 'POST',
        body: JSON.stringify(body),
      });
      setNotice(`Прогон поставлен в очередь (runId ${res.runId.slice(0, 8)}…). Worker обработает его пакетами.`);
      void loadRuns();
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setSubmitting(false);
    }
  }

  const submitDisabled =
    submitting ||
    !canOperate ||
    from === '' ||
    to === '' ||
    (!validation && gate === '') ||
    (!validation && threshold.trim() === '');

  const statsSummary = useMemo(() => (run: ReplayRun) => ({
    checked: fmtNum(run.stats['checked']),
    changed: fmtNum(run.stats['verdicts_changed']),
    chain: fmtNum(run.stats['full_chain_available']),
    outcomes: fmtNum(run.stats['outcomes_available']),
  }), []);

  return (
    <div className="section">
      <div className="section-header" style={{ marginBottom: '1rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <span style={{ fontSize: '1.75rem' }}>⏪</span>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.4rem', fontWeight: 700 }}>Replay гейтов</h2>
            <p style={{ margin: '0.2rem 0 0', color: 'var(--muted)', fontSize: '0.85rem' }}>
              Проверка изменения порога на записанной ленте решений до правки боевых настроек.
              Прогон — только эксперимент: боты не создаются, заявки не отправляются.
            </p>
          </div>
        </div>
      </div>

      {error && <div className="alert danger" style={{ marginBottom: '1rem' }}>{error}</div>}
      {notice && <div className="alert success" style={{ marginBottom: '1rem' }}>{notice}</div>}

      <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
        <div style={{ display: 'flex', gap: '1rem', flexWrap: 'wrap', alignItems: 'flex-end' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>Гейт</label>
            <select
              value={gate}
              onChange={(e) => setGate(e.target.value)}
              disabled={validation}
              style={{ width: '200px' }}
            >
              <option value="">{gateOptions.length === 0 ? '— журнал пуст —' : 'выбери гейт…'}</option>
              {gateOptions.map((code) => (
                <option key={code} value={code}>{code}</option>
              ))}
            </select>
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>Новый порог</label>
            <input
              type="number"
              step="0.1"
              value={threshold}
              onChange={(e) => setThreshold(e.target.value)}
              disabled={validation}
              placeholder="1.8"
              style={{ width: '110px' }}
            />
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>Период с</label>
            <input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              style={{ width: '210px' }}
            />
          </div>
          <div>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>по</label>
            <input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              style={{ width: '210px' }}
            />
          </div>
          <label
            className="badge"
            style={{
              display: 'flex', alignItems: 'center', gap: '6px', cursor: 'pointer', padding: '8px 10px',
              background: validation ? 'rgba(101, 245, 190, 0.09)' : undefined,
              border: `1px solid ${validation ? 'rgba(101, 245, 190, 0.3)' : 'var(--border)'}`,
            }}
            title="Нулевой override по выбранному периоду: сохранённые вердикты обязаны воспроизводиться, иначе модель не откалибрована"
          >
            <input
              type="checkbox"
              checked={validation}
              onChange={(e) => setValidation(e.target.checked)}
              style={{ width: 16, height: 16, accentColor: 'var(--accent)' }}
            />
            Валидация модели
          </label>
          <button
            className="button primary"
            onClick={() => void submit()}
            disabled={submitDisabled}
            title={canOperate ? '' : 'Требуется роль OPERATOR'}
          >
            {submitting ? 'Ставлю в очередь…' : validation ? '▶ Запустить валидацию' : '▶ Сравнить'}
          </button>
        </div>
        <small className="muted" style={{ display: 'block', marginTop: '0.75rem' }}>
          N/K/M/L: проверено / вердикт изменился / доступна полная цепочка / доступен исход.
          Без зелёной валидации результаты прогонов помечаются «модель не откалибрована».
        </small>
      </div>

      {selected && (
        <div className="panel" style={{ marginBottom: '1.25rem' }}>
          <div className="panel-heading">
            <div>
              <span className="eyebrow">RUN {selected.run.id.slice(0, 8)}</span>
              <h3>
                Прогон ·{' '}
                {selected.run.overrides && Object.keys(selected.run.overrides).length > 0
                  ? String(selected.run.overrides['gate'] ?? 'override')
                  : 'валидация (нулевой override)'}
              </h3>
              <small className="muted" style={{ display: 'block', marginTop: '2px' }}>
                {new Date(selected.run.periodFrom).toLocaleString()} — {new Date(selected.run.periodTo).toLocaleString()}
                {selected.run.statusReason ? ` · ${selected.run.statusReason}` : ''}
              </small>
            </div>
            <div style={{ display: 'flex', gap: '8px', alignItems: 'center', flexWrap: 'wrap' }}>
              {selected.run.modelCalibrated === true && <span className="badge success">модель откалибрована</span>}
              {selected.run.modelCalibrated === false && <span className="badge danger">модель не откалибрована</span>}
              {selected.run.modelCalibrated === null && <span className="badge neutral">калибровка не проверялась</span>}
              <span className={RUN_STATUS_BADGES[selected.run.status] ?? 'badge neutral'}>{selected.run.status}</span>
              <button className="button small" onClick={() => setSelected(null)}>Закрыть</button>
            </div>
          </div>

          <div className="metric-grid" style={{ gridTemplateColumns: 'repeat(auto-fit, minmax(140px, 1fr))' }}>
            <div className="metric-card"><span>Проверено (N)</span><strong>{statsSummary(selected.run).checked}</strong></div>
            <div className="metric-card"><span>Вердикт изменился (K)</span><strong>{statsSummary(selected.run).changed}</strong></div>
            <div className="metric-card"><span>Полная цепочка (M)</span><strong>{statsSummary(selected.run).chain}</strong></div>
            <div className="metric-card"><span>Исход доступен (L)</span><strong>{statsSummary(selected.run).outcomes}</strong></div>
            <div className="metric-card">
              <span>ΔPnL модели</span>
              <strong
                style={{
                  color: Number(selected.run.effect['delta_pnl_proxy']) > 0
                    ? '#34d399'
                    : Number(selected.run.effect['delta_pnl_proxy']) < 0 ? '#f87171' : undefined,
                }}
              >
                {fmtUsd(selected.run.effect['delta_pnl_proxy'])}
              </strong>
            </div>
          </div>

          <div style={{ display: 'flex', gap: '12px', flexWrap: 'wrap', marginBottom: '14px' }}>
            <span className="badge success" title="Модельный убыток, который override не пропустил бы в сделку">
              предотвращённые потери: {fmtUsd(selected.run.effect['prevented_losses'])}
            </span>
            <span className="badge danger" title="Модельная прибыль, которую override blocking-гейта не даст взять">
              упущенная прибыль: {fmtUsd(selected.run.effect['missed_profits'])}
            </span>
          </div>

          {detailError && <div className="alert danger" style={{ marginBottom: '0.75rem' }}>{detailError}</div>}

          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Символ</th>
                  <th>Вердикт replay</th>
                  <th>Был → стал</th>
                  <th>Цепочка после гейта</th>
                  <th>Исход эпизода</th>
                  <th>Причина не-replay</th>
                </tr>
              </thead>
              <tbody>
                {selected.items.length === 0 && (
                  <tr>
                    <td colSpan={6} style={{ textAlign: 'center', padding: '1.25rem', color: 'var(--muted)' }}>
                      {selected.run.status === 'DONE' ? 'Позиций нет — в периоде не нашлось проверяемых решений' : 'Прогон ещё в работе…'}
                    </td>
                  </tr>
                )}
                {selected.items.map((item) => {
                  const style = ITEM_VERDICT_STYLES[item.verdict] ?? { label: item.verdict, cls: 'badge neutral' };
                  const pnl = item.episodeOutcomePnl === null ? null : Number(item.episodeOutcomePnl);
                  return (
                    <tr key={item.id}>
                      <td><strong>{item.symbol}</strong></td>
                      <td><span className={style.cls}>{style.label}</span></td>
                      <td>
                        <small>
                          {item.baseOutcome || '—'} → <strong>{item.newOutcome || '—'}</strong>
                        </small>
                      </td>
                      <td>{item.chainFollowed ? '✓ полностью' : '— нет'}</td>
                      <td style={{ color: pnl === null ? undefined : pnl > 0 ? '#34d399' : pnl < 0 ? '#f87171' : undefined }}>
                        {pnl === null ? '—' : `${pnl > 0 ? '+' : ''}${pnl.toFixed(2)} $`}
                      </td>
                      <td>
                        <small className="muted" title={item.notReplayableCause}>{item.notReplayableCause || '—'}</small>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">ИСТОРИЯ</span>
            <h3>Прогоны ({runs.length})</h3>
          </div>
        </div>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Создан</th>
                <th>Период</th>
                <th>Эксперимент</th>
                <th>Статус</th>
                <th>N / K / M / L</th>
                <th>ΔPnL</th>
                <th>Модель</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {runs.length === 0 && (
                <tr>
                  <td colSpan={8} style={{ textAlign: 'center', padding: '1.5rem', color: 'var(--muted)' }}>
                    Прогонов пока нет — задай параметры выше
                  </td>
                </tr>
              )}
              {runs.map((run) => {
                const s = statsSummary(run);
                const overrideGate = run.overrides && Object.keys(run.overrides).length > 0
                  ? String(run.overrides['gate'] ?? 'override')
                  : 'валидация';
                return (
                  <tr key={run.id}>
                    <td><span className="badge neutral">{new Date(run.createdAt).toLocaleString()}</span></td>
                    <td>
                      <small>
                        {new Date(run.periodFrom).toLocaleDateString()} — {new Date(run.periodTo).toLocaleDateString()}
                      </small>
                    </td>
                    <td><strong>{overrideGate}</strong></td>
                    <td><span className={RUN_STATUS_BADGES[run.status] ?? 'badge neutral'}>{run.status}</span></td>
                    <td><small>{s.checked} / {s.changed} / {s.chain} / {s.outcomes}</small></td>
                    <td style={{ color: Number(run.effect['delta_pnl_proxy']) > 0 ? '#34d399' : Number(run.effect['delta_pnl_proxy']) < 0 ? '#f87171' : undefined }}>
                      {run.status === 'DONE' ? fmtUsd(run.effect['delta_pnl_proxy']) : '—'}
                    </td>
                    <td>
                      {run.modelCalibrated === true ? (
                        <span className="badge success">ок</span>
                      ) : run.modelCalibrated === false ? (
                        <span className="badge danger">не откалибрована</span>
                      ) : (
                        <span className="badge neutral">—</span>
                      )}
                    </td>
                    <td>
                      <button
                        className="button small"
                        onClick={() => void openRun(run.id)}
                        disabled={selected?.run.id === run.id}
                      >
                        Открыть
                      </button>
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
