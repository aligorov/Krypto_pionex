import { FormEvent, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { MacroSources } from '../types';

interface Props {
  canManage: boolean;
}

interface TestResult {
  ok: boolean;
  error?: string;
  latencyMs?: number;
}

export default function MacroSettings({ canManage }: Props) {
  const [status, setStatus] = useState<MacroSources | null>(null);
  const [key, setKey] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<TestResult | null>(null);

  function load() {
    api<MacroSources>('/api/macro/sources')
      .then((data) => setStatus(data))
      .catch((loadError) => setError(describeError(loadError)));
  }

  useEffect(load, []);

  async function submit(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await api<MacroSources>('/api/macro/sources', {
        method: 'PUT',
        body: JSON.stringify({ fredApiKey: key.trim() }),
      });
      setStatus(updated);
      setKey('');
      setSaved(true);
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  }

  async function test() {
    setTesting(true);
    setTestResult(null);
    setError(null);
    try {
      const result = await api<TestResult>('/api/macro/test', {
        method: 'POST',
        body: JSON.stringify(key.trim() ? { fredApiKey: key.trim() } : {}),
      });
      setTestResult(result);
    } catch (testError) {
      setError(describeError(testError));
    } finally {
      setTesting(false);
    }
  }

  return (
    <div className="panel">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">MACRO DATA</span>
          <h3>FRED API (ФРБ Сент-Луиса)</h3>
        </div>
      </div>
      <p>
        Бесплатные режимные ряды (доходности, доллар-индекс, VIX, стресс-индекс) для контекста
        LLM-аудита. Ключ хранится в PostgreSQL и никогда не покидает сервер. Без ключа работают
        бесключевые источники (Yahoo VIX/DXY, новости RSS, FOMC-календарь).
      </p>
      {error && <div className="alert danger"><span>{error}</span></div>}
      {saved && (
        <div className="alert success">
          <span>
            Ключ сохранён{status && !status.hasKey ? ' (FRED-нога отключена)' : ''}. Первый
            сбор — в течение часа.
          </span>
        </div>
      )}
      {status && (
        <p className="muted">
          Статус: {status.hasKey
            ? `ключ настроен (${status.keyLength} симв.)`
            : 'ключ не задан — FRED-ряды не собираются'}
          {status.updatedAt ? `, обновлён ${new Date(status.updatedAt).toLocaleString()}` : ''}
        </p>
      )}
      <form className="form-grid" onSubmit={submit}>
        <label>
          FRED API-ключ (32 символа)
          <input
            value={key}
            disabled={!canManage || busy}
            onChange={(event) => setKey(event.target.value)}
            placeholder={status?.hasKey ? '•••• — введи новый, чтобы заменить' : 'fredaccount.stlouisfed.org/apikeys'}
            inputMode="text"
            autoComplete="off"
            spellCheck={false}
          />
        </label>
        <div className="button-row">
          {canManage && (
            <button className="button primary" type="submit" disabled={busy || (!key.trim() && !status?.hasKey)}>
              {busy ? 'Сохранение…' : key.trim() ? 'Сохранить ключ' : 'Убрать ключ'}
            </button>
          )}
          {canManage && (
            <button className="button" type="button" disabled={testing || (!key.trim() && !status?.hasKey)} onClick={() => void test()}>
              {testing ? 'Проверка…' : 'Проверить ключ'}
            </button>
          )}
        </div>
      </form>
      {testResult && (
        <div className={`alert ${testResult.ok ? 'success' : 'danger'}`}>
          <span>
            {testResult.ok
              ? `Ключ валиден, FRED ответил за ${testResult.latencyMs ?? '?'} мс`
              : `Ошибка: ${testResult.error ?? 'неизвестная'}`}
          </span>
        </div>
      )}
      {status?.series && status.series.length > 0 && (
        <div>
          <span className="eyebrow">СОБРАННЫЕ РЯДЫ</span>
          <ul className="metric-list">
            {status.series.map((point) => (
              <li key={point.metric}>
                <strong>{point.metric}</strong> = {point.value}
                <span className="muted"> · {new Date(point.capturedAt).toLocaleTimeString()}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}
