import { useCallback, useEffect, useState } from 'react';
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

export default function Candidates({ canOperate: _canOperate }: Props) {
  const [state, setState] = useState<AutoGridState | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [aiKit, setAiKit] = useState<Record<string, AIKitState>>({});

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

  if (!state) {
    return <div className="empty-state">{error ?? 'Загрузка кандидатов…'}</div>;
  }

  const scan = state.lastScan;
  const accepted = state.candidates.filter((candidate) => candidate.decision === 'ACCEPTED');
  const rejected = state.candidates.filter((candidate) => candidate.decision !== 'ACCEPTED');

  return (
    <div className="section-stack">
      {error && (
        <div className="alert danger">
          <span>{error}</span>
          <button onClick={() => setError(null)}>×</button>
        </div>
      )}

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">LAST SCAN</span>
            <h3>Кандидаты последнего скана</h3>
          </div>
          <span className="muted">
            {scan
              ? `${new Date(scan.startedAt).toLocaleString()} · ${scan.candidatesFound} пар · принято ${accepted.length}`
              : 'Скан ещё не выполнялся'}
          </span>
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
                  <th>Символ</th>
                  <th>Score</th>
                  <th>Режим рынка</th>
                  <th>Позиция в диапазоне</th>
                  <th>Волатильность</th>
                  <th>ADX</th>
                  <th>Грид (диапазон)</th>
                  <th>Направление</th>
                  <th>Плечо</th>
                  <th>EV / Sharpe</th>
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
                  <th>Символ</th>
                  <th>Причина</th>
                  <th>Волатильность</th>
                  <th>Объём 24ч</th>
                </tr>
              </thead>
              <tbody>
                {rejected.map((candidate) => (
                  <tr key={candidate.id}>
                    <td>{candidate.symbol}</td>
                    <td><small>{candidate.rejectionReason ?? '—'}</small></td>
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
        <strong>{candidate.symbol}</strong>
        <small>{candidate.currentPrice}</small>
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
