import { useCallback, useEffect, useMemo, useState } from 'react';
import { api } from '../api';
import { describeError } from './AutoGridAutopilot';
import type { AIKitResponse, AutoGridCandidate, AutoGridState } from '../types';

interface Props {
  canOperate: boolean;
}

interface AIKitState {
  loading: boolean;
  data: AIKitResponse | null;
  error: string | null;
}

type SortKey = 'createdAt' | 'score' | 'volatilityPct' | 'expectedValuePct' | 'symbol';
type SortOrder = 'asc' | 'desc';

export default function Candidates({ canOperate: _canOperate }: Props) {
  const [state, setState] = useState<AutoGridState | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [aiKit, setAiKit] = useState<Record<string, AIKitState>>({});
  const [sortKey, setSortKey] = useState<SortKey>('createdAt');
  const [sortOrder, setSortOrder] = useState<SortOrder>('desc');

  const load = useCallback(async () => {
    try {
      setState(await api<AutoGridState>('/api/autogrid'));
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, []);

  useEffect(() => {
    void load();
    const timer = window.setInterval(() => void load(), 30000);
    return () => window.clearInterval(timer);
  }, [load]);

  async function fetchAIKit(symbol: string) {
    setAiKit((current) => ({ ...current, [symbol]: { loading: true, data: null, error: null } }));
    try {
      const data = await api<AIKitResponse>(
        `/api/autogrid/ai-strategy?symbol=${encodeURIComponent(symbol)}`,
      );
      setAiKit((current) => ({ ...current, [symbol]: { loading: false, data, error: null } }));
    } catch (fetchError) {
      setAiKit((current) => ({
        ...current,
        [symbol]: { loading: false, data: null, error: describeError(fetchError) },
      }));
    }
  }

  function handleSort(key: SortKey) {
    if (sortKey === key) {
      setSortOrder((prev) => (prev === 'desc' ? 'asc' : 'desc'));
    } else {
      setSortKey(key);
      setSortOrder('desc');
    }
  }

  const sortCandidates = useCallback((candidates: AutoGridCandidate[]) => {
    return [...candidates].sort((a, b) => {
      let comparison = 0;
      switch (sortKey) {
        case 'createdAt':
          comparison = new Date(a.createdAt).getTime() - new Date(b.createdAt).getTime();
          break;
        case 'score':
          comparison = Number(a.score) - Number(b.score);
          break;
        case 'volatilityPct':
          comparison = Number(a.volatilityPct) - Number(b.volatilityPct);
          break;
        case 'expectedValuePct':
          comparison = Number(a.expectedValuePct) - Number(b.expectedValuePct);
          break;
        case 'symbol':
          comparison = a.symbol.localeCompare(b.symbol);
          break;
      }
      return sortOrder === 'desc' ? -comparison : comparison;
    });
  }, [sortKey, sortOrder]);

  const accepted = useMemo(() => {
    if (!state) return [];
    return sortCandidates(state.candidates.filter((c) => c.decision === 'ACCEPTED'));
  }, [state, sortCandidates]);

  const rejected = useMemo(() => {
    if (!state) return [];
    return sortCandidates(state.candidates.filter((c) => c.decision !== 'ACCEPTED'));
  }, [state, sortCandidates]);

  if (!state) {
    return <div className="empty-state">{error ?? 'Загрузка кандидатов…'}</div>;
  }

  const scan = state.lastScan;

  return (
    <div className="section-stack">
      {error && (
        <div className="alert danger">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      <div className="panel">
        <div className="panel-heading" style={{ flexWrap: 'wrap', gap: '1rem' }}>
          <div>
            <span className="eyebrow">LAST SCAN</span>
            <h3>Кандидаты последнего скана ({accepted.length})</h3>
          </div>
          
          <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem', flexWrap: 'wrap' }}>
            <div style={{ display: 'flex', alignItems: 'center', gap: '0.35rem', fontSize: '0.85rem' }}>
              <span className="muted">Сортировка:</span>
              <button
                className={`button small ${sortKey === 'createdAt' ? 'primary' : ''}`}
                onClick={() => handleSort('createdAt')}
                title="Сортировать по времени сканирования"
              >
                Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
              <button
                className={`button small ${sortKey === 'score' ? 'primary' : ''}`}
                onClick={() => handleSort('score')}
                title="Сортировать по Score"
              >
                Score {sortKey === 'score' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
              <button
                className={`button small ${sortKey === 'volatilityPct' ? 'primary' : ''}`}
                onClick={() => handleSort('volatilityPct')}
                title="Сортировать по волатильности"
              >
                Волатильность {sortKey === 'volatilityPct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
              </button>
            </div>
            
            <span className="muted">
              {scan
                ? `${new Date(scan.startedAt).toLocaleString()} · ${scan.candidatesFound} пар`
                : 'Скан ещё не выполнялся'}
            </span>
          </div>
        </div>

        {accepted.length === 0 ? (
          <div className="empty-state">
            Принятых кандидатов нет. Запустите автопилот или сделайте скан на экране «Автопилот».
          </div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th onClick={() => handleSort('createdAt')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th onClick={() => handleSort('symbol')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Символ {sortKey === 'symbol' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th onClick={() => handleSort('score')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Score {sortKey === 'score' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>Режим рынка</th>
                  <th>Позиция в диапазоне</th>
                  <th onClick={() => handleSort('volatilityPct')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Волатильность {sortKey === 'volatilityPct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>ADX</th>
                  <th>Грид (диапазон)</th>
                  <th>Направление</th>
                  <th>Плечо</th>
                  <th onClick={() => handleSort('expectedValuePct')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    EV / Sharpe {sortKey === 'expectedValuePct' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>AI Kit</th>
                </tr>
              </thead>
              <tbody>
                {accepted.map((candidate) => (
                  <CandidateRow
                    key={candidate.id}
                    candidate={candidate}
                    aiKit={aiKit[candidate.symbol]}
                    onFetchAIKit={() => void fetchAIKit(candidate.symbol)}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      {rejected.length > 0 && (
        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">REJECTED</span>
              <h3>Отклонённые ({rejected.length})</h3>
            </div>
          </div>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th onClick={() => handleSort('createdAt')} style={{ cursor: 'pointer', whiteSpace: 'nowrap' }}>
                    Время {sortKey === 'createdAt' ? (sortOrder === 'desc' ? '↓' : '↑') : ''}
                  </th>
                  <th>Символ</th>
                  <th>Причина</th>
                  <th>Волатильность</th>
                  <th>Объём 24ч</th>
                </tr>
              </thead>
              <tbody>
                {rejected.map((candidate) => (
                  <tr key={candidate.id}>
                    <td>
                      <span className="badge neutral" title={new Date(candidate.createdAt).toLocaleString()}>
                        {formatTime(candidate.createdAt)}
                      </span>
                    </td>
                    <td>{candidate.symbol}</td>
                    <td>
                      {candidate.rejectionReason?.startsWith('AI:') ? (
                        <span style={{ color: '#f87171' }}>🧠 {candidate.rejectionReason}</span>
                      ) : (
                        <small>{candidate.rejectionReason ?? '—'}</small>
                      )}
                    </td>
                    <td>{candidate.volatilityPct}%</td>
                    <td>{candidate.volume24h}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}

function CandidateRow({
  candidate,
  aiKit,
  onFetchAIKit,
}: {
  candidate: AutoGridCandidate;
  aiKit?: AIKitState;
  onFetchAIKit: () => void;
}) {
  const assumptions = candidate.modelAssumptions as Record<string, unknown>;
  const regime = String(assumptions['regime'] ?? '—');
  const adx = assumptions['adx'];
  const rangePosition = assumptions['rangePositionPct'];
  const storedAI = assumptions['aiKit'] as Record<string, unknown> | undefined;

  return (
    <tr>
      <td>
        <span className="badge neutral" title={new Date(candidate.createdAt).toLocaleString()}>
          {formatTime(candidate.createdAt)}
        </span>
      </td>
      <td>
        <strong>{candidate.symbol}</strong>
        <small>{candidate.currentPrice}</small>
        {assumptions['llmConfidence'] !== undefined && (
          <div style={{ marginTop: '3px' }}>
            <span
              className="badge"
              style={{ background: 'rgba(59, 130, 246, 0.15)', color: '#60a5fa', border: '1px solid rgba(59, 130, 246, 0.3)', fontSize: '10px', padding: '1px 5px', cursor: 'help' }}
              title={`🧠 AI Мозг (${assumptions['llmRegime'] || 'Аудит'}): ${assumptions['llmReasoning'] || ''}`}
            >
              🧠 AI {Math.round(Number(assumptions['llmConfidence']) * 100)}%
            </span>
          </div>
        )}
      </td>
      <td><strong>{Number(candidate.score).toFixed(3)}</strong></td>
      <td>
        <span className={`badge ${regimeBadge(regime)}`}>{regime}</span>
      </td>
      <td>{formatNumber(rangePosition)}%</td>
      <td>{Number(candidate.volatilityPct).toFixed(2)}%</td>
      <td>{formatNumber(adx)}</td>
      <td>
        {candidate.lowerPrice} – {candidate.upperPrice}
        <small>{candidate.gridNum} уровней</small>
      </td>
      <td>
        <span className={`badge ${candidate.recommendedTrend === 'long' ? 'success' : candidate.recommendedTrend === 'short' ? 'danger' : 'neutral'}`}>
          {candidate.recommendedTrend}
        </span>
      </td>
      <td>{candidate.recommendedLeverage}x</td>
      <td>
        {Number(candidate.expectedValuePct).toFixed(3)}% / {Number(candidate.sharpe).toFixed(2)}
      </td>
      <td>
        {storedAI ? (
          <div>
            <span className="badge success">AI Kit</span>
            <small>
              год. {String(storedAI['annualized'])} · волат. {String(storedAI['volatility'])}
              {storedAI['gridCount'] ? ` · сеток: ${String(storedAI['gridCount'])} (AI)` : ''}
            </small>
          </div>
        ) : aiKit?.loading ? (
          <small>Запрос AI Kit…</small>
        ) : aiKit?.data ? (
          <div>
            <span className="badge success">AI Kit</span>
            <small>
              год. {aiKit.data.strategy.annualized} · волат. {aiKit.data.strategy.volatility} ·{' '}
              {aiKit.data.strategy.gridCount} ур.
            </small>
          </div>
        ) : (
          <div>
            <button className="button small" onClick={onFetchAIKit}>AI Kit</button>
            {aiKit?.error && <small>{aiKit.error}</small>}
          </div>
        )}
      </td>
    </tr>
  );
}

function regimeBadge(regime: string): string {
  if (regime === 'TREND_UP') return 'success';
  if (regime === 'TREND_DOWN') return 'danger';
  if (regime === 'RANGE') return 'neutral';
  return 'warning';
}

function formatNumber(value: unknown): string {
  const parsed = Number(value);
  if (!Number.isFinite(parsed)) return '—';
  return parsed.toFixed(1);
}

function formatTime(dateStr: string): string {
  if (!dateStr) return '—';
  const d = new Date(dateStr);
  if (isNaN(d.getTime())) return '—';
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
}
