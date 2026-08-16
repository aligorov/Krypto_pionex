import { useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { LLMSettings, LLMAuditRecord } from '../types';

type Provider = 'gemini' | 'anthropic' | 'openrouter' | 'custom';

interface ProviderProfile {
  id: Provider;
  icon: string;
  title: string;
  tagline: string;
  keyPrefix: string;
  keyHint: string;
  keyUrl: string;
  keyUrlLabel: string;
  recommended?: boolean;
  note: string;
}

const PROVIDERS: ProviderProfile[] = [
  {
    id: 'gemini',
    icon: '✦',
    title: 'Google Gemini',
    tagline: 'Рекомендуем: дёшево, быстро, самый строгий JSON',
    keyPrefix: 'AIza',
    keyHint: 'Ключ Google AI Studio, начинается с AIza… (~39 символов)',
    keyUrl: 'https://aistudio.google.com/apikey',
    keyUrlLabel: 'aistudio.google.com/apikey',
    recommended: true,
    note: 'Бесплатного тарифа хватает на сотни аудитов. Лучший выбор для новостного вето.',
  },
  {
    id: 'anthropic',
    icon: '🅐',
    title: 'Anthropic Claude',
    tagline: 'Максимальное качество рассуждений',
    keyPrefix: 'sk-ant-',
    keyHint: 'Ключ console.anthropic.com, начинается с sk-ant-…',
    keyUrl: 'https://console.anthropic.com/settings/keys',
    keyUrlLabel: 'console.anthropic.com',
    note: 'Дороже и медленнее Flash-моделей; для пограничных кандидатов рассуждает глубже всех.',
  },
  {
    id: 'openrouter',
    icon: '🛣',
    title: 'OpenRouter',
    tagline: 'Любая модель из каталога одним ключом',
    keyPrefix: 'sk-or-v1-',
    keyHint: 'Ключ openrouter.ai, начинается с sk-or-v1-… (~73 символа)',
    keyUrl: 'https://openrouter.ai/settings/keys',
    keyUrlLabel: 'openrouter.ai/settings/keys',
    note: 'Ошибка «User not found» = ключ от другого сервиса или скопирован не целиком.',
  },
  {
    id: 'custom',
    icon: '⚙',
    title: 'Свой эндпоинт',
    tagline: 'OpenAI-совместимый API с вашим baseUrl',
    keyPrefix: '',
    keyHint: 'Ключ вашего провайдера (любой формат)',
    keyUrl: '',
    keyUrlLabel: '',
    note: 'baseUrl обязан быть https и публичным хостом; внутренние адреса отклоняются.',
  },
];

const MODEL_PRESETS: Record<Provider, string[]> = {
  gemini: ['gemini-3.7-flash', 'gemini-3.6-flash', 'gemini-3.5-flash', 'gemini-3.5-flash-lite', 'gemini-3.1-pro'],
  anthropic: ['claude-3-7-sonnet-20250219', 'claude-3-5-sonnet-20241022', 'claude-3-5-haiku-20241022'],
  openrouter: ['google/gemini-2.0-flash-001', 'anthropic/claude-3.7-sonnet', 'deepseek/deepseek-r1', 'openai/o3-mini'],
  custom: ['default'],
};

function keyLooksValid(provider: Provider, key: string): 'empty' | 'ok' | 'warn' {
  const trimmed = key.trim();
  if (trimmed === '') return 'empty';
  const profile = PROVIDERS.find((item) => item.id === provider)!;
  if (profile.keyPrefix === '') return 'ok';
  return trimmed.startsWith(profile.keyPrefix) ? 'ok' : 'warn';
}

export default function LLMSettings() {
  const [view, setView] = useState<'config' | 'audits'>('config');
  const [settings, setSettings] = useState<LLMSettings | null>(null);
  const [audits, setAudits] = useState<LLMAuditRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string; latency?: number } | null>(null);
  const [notice, setNotice] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

  const [enabled, setEnabled] = useState(false);
  const [provider, setProvider] = useState<Provider>('gemini');
  const [apiKey, setApiKey] = useState('');
  const [model, setModel] = useState('gemini-3.7-flash');
  const [baseUrl, setBaseUrl] = useState('');
  const [temperature, setTemperature] = useState(0.2);
  const [thinkingEnabled, setThinkingEnabled] = useState(true);
  const [requireAuditForReal, setRequireAuditForReal] = useState(false);
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);

  useEffect(() => {
    loadSettings();
  }, []);

  async function loadSettings() {
    setLoading(true);
    try {
      const data = await api<LLMSettings>('/api/llm/settings');
      setSettings(data);
      setEnabled(data.enabled);
      setProvider(data.provider);
      setModel(data.model || 'gemini-3.7-flash');
      setBaseUrl(data.baseUrl || '');
      setTemperature(data.temperature ?? 0.2);
      setThinkingEnabled(data.thinkingEnabled ?? true);
      setRequireAuditForReal(data.requireAuditForReal ?? false);
      setApiKey('');
      setTestResult(null);
      setAvailableModels([]);
    } catch (err) {
      setNotice({ kind: 'danger', text: describeError(err) });
    } finally {
      setLoading(false);
    }
  }

  async function loadAudits() {
    try {
      const data = await api<LLMAuditRecord[]>('/api/llm/audits');
      setAudits(data);
    } catch {
      setAudits([]);
    }
  }

  useEffect(() => {
    if (view === 'audits') {
      void loadAudits();
    }
  }, [view]);

  function handleProviderChange(next: Provider) {
    setProvider(next);
    setTestResult(null);
    setAvailableModels([]);
    const presets = MODEL_PRESETS[next] || [];
    if (presets.length > 0 && !presets.includes(model)) {
      setModel(presets[0]);
    }
    if (next !== 'custom' && baseUrl) {
      setBaseUrl('');
    }
  }

  async function handleFetchModels() {
    setLoadingModels(true);
    setNotice(null);
    try {
      const res = await api<{ ok: boolean; models?: string[]; error?: string }>('/api/llm/models', {
        method: 'POST',
        body: JSON.stringify({ provider, apiKey: apiKey.trim() || undefined, baseUrl }),
      });
      if (res.ok && res.models && res.models.length > 0) {
        setAvailableModels(res.models);
        setNotice({ kind: 'success', text: `Получено ${res.models.length} моделей.` });
      } else {
        setNotice({ kind: 'danger', text: res.error || 'Не удалось получить список моделей' });
      }
    } catch (err) {
      setNotice({ kind: 'danger', text: describeError(err) });
    } finally {
      setLoadingModels(false);
    }
  }

  async function handleTest() {
    setTesting(true);
    setTestResult(null);
    setNotice(null);
    try {
      const res = await api<{ ok: boolean; response?: string; error?: string; latencyMs: number }>('/api/llm/test', {
        method: 'POST',
        body: JSON.stringify({
          provider,
          apiKey: apiKey.trim() || undefined,
          model,
          baseUrl,
          temperature,
          thinkingEnabled,
        }),
      });
      setTestResult({
        ok: res.ok,
        message: res.ok
          ? `Подключение успешно! ${res.latencyMs} мс`
          : `Ошибка API: ${res.error || 'неизвестная'}`,
        latency: res.latencyMs,
      });
    } catch (err) {
      setTestResult({ ok: false, message: `Сбой запроса: ${describeError(err)}` });
    } finally {
      setTesting(false);
    }
  }

  async function handleSave() {
    setSaving(true);
    setNotice(null);
    try {
      const updated = await api<LLMSettings>('/api/llm/settings', {
        method: 'PUT',
        body: JSON.stringify({
          id: 1,
          enabled,
          provider,
          apiKey: apiKey.trim() || undefined,
          model,
          baseUrl: provider === 'custom' ? baseUrl : '',
          temperature,
          thinkingEnabled,
          requireAuditForReal,
          auditIntervalSeconds: 3600,
        }),
      });
      setSettings(updated);
      setApiKey('');
      setNotice({ kind: 'success', text: 'Настройки AI Мозга сохранены в PostgreSQL.' });
    } catch (err) {
      setNotice({ kind: 'danger', text: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  const profile = PROVIDERS.find((item) => item.id === provider)!;
  const keyState = keyLooksValid(provider, apiKey);
  const modelOptions = Array.from(new Set([...MODEL_PRESETS[provider], ...availableModels]));

  if (loading) {
    return (
      <div className="section" style={{ textAlign: 'center', padding: '3rem' }}>
        <div className="loading-spinner" />
        <p style={{ marginTop: '1rem', color: 'var(--text-secondary)' }}>Загрузка настроек AI…</p>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: '980px', margin: '0 auto' }}>
      <div className="section-header" style={{ marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <span style={{ fontSize: '1.75rem' }}>🧠</span>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.4rem', fontWeight: 700 }}>AI Мозг — аудит кандидатов</h2>
            <p style={{ margin: '0.2rem 0 0', color: 'var(--text-secondary)', fontSize: '0.85rem' }}>
              Перед REAL-деплоем каждый кандидат проходит LLM-аудит с новостным вето (UNLOCK / DELIST / EXPLOIT)
            </p>
          </div>
        </div>
        <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center' }}>
          {settings && (
            <span className={`badge ${settings.enabled ? 'success' : 'neutral'}`}>
              {settings.enabled ? '🟢 Включён' : '⚪ Выключен'}
            </span>
          )}
          {settings?.apiKeyMasked && <span className="badge neutral">Ключ: {settings.apiKeyMasked}</span>}
        </div>
      </div>

      <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1.25rem' }}>
        {(['config', 'audits'] as const).map((item) => (
          <button
            key={item}
            className={`btn btn-sm ${view === item ? 'btn-primary' : ''}`}
            onClick={() => setView(item)}
          >
            {item === 'config' ? 'Настройки' : `Аудиты (${audits.length})`}
          </button>
        ))}
      </div>

      {notice && <div className={`banner ${notice.kind === 'success' ? 'success' : 'error'}`} style={{ marginBottom: '1rem' }}>{notice.text}</div>}

      {view === 'config' && (
        <>
          <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
            <h3 style={{ margin: '0 0 0.75rem', fontSize: '1.05rem' }}>1. Провайдер</h3>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(220px, 1fr))', gap: '0.75rem' }}>
              {PROVIDERS.map((item) => (
                <div
                  key={item.id}
                  onClick={() => handleProviderChange(item.id)}
                  style={{
                    border: `1px solid ${provider === item.id ? 'var(--primary-color)' : 'var(--border-color)'}`,
                    borderRadius: '8px',
                    padding: '0.85rem',
                    cursor: 'pointer',
                    background: provider === item.id ? 'rgba(56, 189, 248, 0.07)' : 'transparent',
                  }}
                >
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <strong style={{ fontSize: '0.95rem' }}>{item.icon} {item.title}</strong>
                    {item.recommended && (
                      <span className="badge" style={{ background: 'rgba(16, 185, 129, 0.15)', color: '#34d399', border: '1px solid rgba(16, 185, 129, 0.3)', fontSize: '10px' }}>
                        РЕКОМЕНДУЕМ
                      </span>
                    )}
                  </div>
                  <small style={{ color: 'var(--text-secondary)', display: 'block', marginTop: '0.3rem' }}>{item.tagline}</small>
                </div>
              ))}
            </div>
            <small className="muted" style={{ display: 'block', marginTop: '0.75rem' }}>{profile.note}</small>
          </div>

          <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
            <h3 style={{ margin: '0 0 0.75rem', fontSize: '1.05rem' }}>2. Ключ API</h3>
            <label style={{ display: 'block', fontSize: '0.8rem', fontWeight: 600, marginBottom: '0.3rem' }}>
              {profile.keyHint}
            </label>
            <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center' }}>
              <input
                className="input"
                type="password"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                placeholder={settings?.apiKeyMasked ? `Сохранён: ${settings.apiKeyMasked} — оставь пустым, чтобы не менять` : `Вставь ключ (${profile.keyPrefix}…)`}
                style={{ flex: 1 }}
                autoComplete="off"
              />
              {keyState === 'ok' && <span title="Формат ключа соответствует провайдеру" style={{ color: '#34d399' }}>✓</span>}
              {keyState === 'warn' && (
                <span title={`Ключ должен начинаться с ${profile.keyPrefix}`} style={{ color: '#f87171' }}>⚠ не тот формат</span>
              )}
            </div>
            <div style={{ display: 'flex', gap: '1rem', marginTop: '0.5rem', flexWrap: 'wrap' }}>
              {profile.keyUrl && (
                <small>
                  Получить ключ:{' '}
                  <a href={profile.keyUrl} target="_blank" rel="noreferrer" style={{ color: '#38bdf8' }}>
                    {profile.keyUrlLabel}
                  </a>
                </small>
              )}
              <small className="muted">Пустое поле при сохранении = остаётся прежний ключ (Zero-ENV: хранится в PostgreSQL)</small>
            </div>
          </div>

          <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
            <h3 style={{ margin: '0 0 0.75rem', fontSize: '1.05rem' }}>3. Модель</h3>
            <div style={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap' }}>
              <input
                className="input"
                list="llm-model-options"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                style={{ flex: 1, minWidth: '240px' }}
                placeholder="Имя модели"
              />
              <datalist id="llm-model-options">
                {modelOptions.map((item) => (
                  <option key={item} value={item} />
                ))}
              </datalist>
              <button className="btn" onClick={() => void handleFetchModels()} disabled={loadingModels}>
                {loadingModels ? 'Загрузка…' : '📥 Получить модели'}
              </button>
            </div>
            <small className="muted" style={{ display: 'block', marginTop: '0.5rem' }}>
              Пресеты провайдера в выпадающем списке; «Получить модели» подтягивает живой список от провайдера
              (использует сохранённый ключ, если поле ключа пусто).
            </small>
          </div>

          {provider === 'custom' && (
            <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
              <h3 style={{ margin: '0 0 0.75rem', fontSize: '1.05rem' }}>Base URL</h3>
              <input
                className="input"
                value={baseUrl}
                onChange={(e) => setBaseUrl(e.target.value)}
                placeholder="https://your-llm.example.com/v1/chat/completions"
              />
              <small className="muted" style={{ display: 'block', marginTop: '0.5rem' }}>
                Только https и публичный хост — внутренние адреса отклоняются (SSRF-защита).
              </small>
            </div>
          )}

          <div className="card" style={{ padding: '1.25rem', marginBottom: '1.25rem' }}>
            <h3 style={{ margin: '0 0 0.75rem', fontSize: '1.05rem' }}>4. Поведение аудита</h3>
            <div style={{ display: 'flex', flexDirection: 'column', gap: '0.75rem' }}>
              <label style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '0.75rem' }}>
                <span>
                  <strong style={{ fontSize: '0.9rem' }}>Аудит включён</strong>
                  <small className="muted" style={{ display: 'block' }}>Кандидаты проходят через LLM перед деплоем</small>
                </span>
                <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} style={{ transform: 'scale(1.2)', cursor: 'pointer' }} />
              </label>
              <label style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '0.75rem' }}>
                <span>
                  <strong style={{ fontSize: '0.9rem' }}>Fail-Closed аудит (REAL)</strong>
                  <small className="muted" style={{ display: 'block' }}>Нет ответа LLM = кандидат отклонён. Деплой на реальные деньги только после успешного аудита</small>
                </span>
                <input type="checkbox" checked={requireAuditForReal} onChange={(e) => setRequireAuditForReal(e.target.checked)} style={{ transform: 'scale(1.2)', cursor: 'pointer' }} />
              </label>
              <label style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: '0.75rem' }}>
                <span>
                  <strong style={{ fontSize: '0.9rem' }}>Thinking Mode</strong>
                  <small className="muted" style={{ display: 'block' }}>Глубокие рассуждения перед вердиктом</small>
                </span>
                <input type="checkbox" checked={thinkingEnabled} onChange={(e) => setThinkingEnabled(e.target.checked)} style={{ transform: 'scale(1.2)', cursor: 'pointer' }} />
              </label>
              <div>
                <div style={{ display: 'flex', justifyContent: 'space-between' }}>
                  <strong style={{ fontSize: '0.9rem' }}>Температура</strong>
                  <small className="muted">{temperature.toFixed(2)}</small>
                </div>
                <input
                  type="range"
                  min={0}
                  max={1}
                  step={0.05}
                  value={temperature}
                  onChange={(e) => setTemperature(Number(e.target.value))}
                  style={{ width: '100%', cursor: 'pointer' }}
                />
                <small className="muted">0.1–0.3 — детерминированные вердикты для риск-гейта</small>
              </div>
            </div>
          </div>

          <div style={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap', alignItems: 'center' }}>
            <button className="btn btn-primary" onClick={() => void handleSave()} disabled={saving}>
              {saving ? 'Сохранение…' : '💾 Сохранить'}
            </button>
            <button className="btn" onClick={() => void handleTest()} disabled={testing}>
              {testing ? 'Проверка…' : '🔌 Проверить подключение'}
            </button>
            <small className="muted">Тест с пустым полем ключа проверяет сохранённый ключ</small>
          </div>

          {testResult && (
            <div className={`banner ${testResult.ok ? 'success' : 'error'}`} style={{ marginTop: '1rem' }}>
              {testResult.message}
              {testResult.latency !== undefined && (
                <span style={{ marginLeft: '0.5rem', opacity: 0.8 }}>Скорость отклика: {testResult.latency} мс</span>
              )}
            </div>
          )}
        </>
      )}

      {view === 'audits' && (
        <div className="panel">
          <div className="panel-heading">
            <div>
              <span className="eyebrow">ИСТОРИЯ</span>
              <h3>Последние аудиты ({audits.length})</h3>
            </div>
          </div>
          <div style={{ overflowX: 'auto' }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Время</th>
                  <th>Символ</th>
                  <th>Вердикт</th>
                  <th>Уверенность</th>
                  <th>Режим</th>
                  <th>Обоснование</th>
                </tr>
              </thead>
              <tbody>
                {audits.length === 0 && (
                  <tr>
                    <td colSpan={6} style={{ textAlign: 'center', padding: '1.5rem', color: 'var(--text-secondary)' }}>
                      Аудитов пока нет — включи AI Мозг и дождись скана
                    </td>
                  </tr>
                )}
                {audits.map((audit) => (
                  <tr key={audit.id}>
                    <td><span className="badge neutral">{new Date(audit.createdAt).toLocaleString()}</span></td>
                    <td><strong>{audit.symbol}</strong></td>
                    <td>
                      <span
                        className="badge"
                        style={{
                          background: audit.decision === 'APPROVED' ? 'rgba(16, 185, 129, 0.15)' : 'rgba(239, 68, 68, 0.15)',
                          color: audit.decision === 'APPROVED' ? '#34d399' : '#f87171',
                        }}
                      >
                        {audit.decision}
                      </span>
                    </td>
                    <td>{Math.round(Number(audit.confidence) * 100)}%</td>
                    <td><small>{audit.regime}</small></td>
                    <td style={{ maxWidth: '380px' }}><small className="muted">{audit.reasoning}</small></td>
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
