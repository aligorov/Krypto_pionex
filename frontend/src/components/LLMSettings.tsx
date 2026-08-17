import { Fragment, useCallback, useEffect, useMemo, useState } from 'react';
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
    keyHint: 'Ключ openrouter.ai, начинается с sk-or-v1-… (~73 символов)',
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

function catalystOf(audit: LLMAuditRecord) {
  const catalyst = audit.recommendedParams?.news_catalyst;
  if (!catalyst || (!catalyst.detected && (catalyst.type ?? 'NONE').toUpperCase() === 'NONE')) return null;
  return catalyst;
}

function severityClass(severity: string): string {
  switch ((severity ?? '').toUpperCase()) {
    case 'CRITICAL':
    case 'HIGH':
      return 'danger';
    case 'MEDIUM':
      return 'warning';
    default:
      return 'neutral';
  }
}

export default function LLMSettings({ canManage }: { canManage: boolean }) {
  const [view, setView] = useState<'config' | 'audits'>('config');
  const [settings, setSettings] = useState<LLMSettings | null>(null);
  const [audits, setAudits] = useState<LLMAuditRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string; latency?: number; response?: string } | null>(null);
  const [notice, setNotice] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

  const [enabled, setEnabled] = useState(false);
  const [provider, setProvider] = useState<Provider>('gemini');
  const [apiKey, setApiKey] = useState('');
  const [model, setModel] = useState('gemini-3.7-flash');
  const [baseUrl, setBaseUrl] = useState('');
  const [temperature, setTemperature] = useState(0.2);
  const [thinkingEnabled, setThinkingEnabled] = useState(true);
  const [requireAuditForReal, setRequireAuditForReal] = useState(false);
  const [groundingEnabled, setGroundingEnabled] = useState(true);
  const [availableModels, setAvailableModels] = useState<string[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);

  // Hidden-but-preserved settings: the backend PUT replaces the whole row, so
  // fields the form does not edit must be echoed back verbatim or every save
  // silently resets them.
  const [requireApprovalToDeploy, setRequireApprovalToDeploy] = useState(false);
  const [auditIntervalSeconds, setAuditIntervalSeconds] = useState(3600);

  const [auditSymbolFilter, setAuditSymbolFilter] = useState('');
  const [auditVerdictFilter, setAuditVerdictFilter] = useState<'ALL' | 'APPROVED' | 'REJECTED'>('ALL');
  const [catalystOnly, setCatalystOnly] = useState(false);
  const [expandedAudit, setExpandedAudit] = useState<string | null>(null);

  const loadSettings = useCallback(async () => {
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
      setGroundingEnabled(data.groundingEnabled ?? true);
      setRequireApprovalToDeploy(data.requireApprovalToDeploy ?? false);
      setAuditIntervalSeconds(data.auditIntervalSeconds || 3600);
      setApiKey('');
      setTestResult(null);
      setAvailableModels([]);
    } catch (err) {
      setNotice({ kind: 'danger', text: describeError(err) });
    } finally {
      setLoading(false);
    }
  }, []);

  const loadAudits = useCallback(async () => {
    try {
      const data = await api<LLMAuditRecord[]>('/api/llm/audits');
      setAudits(data);
    } catch {
      /* keep previous list; the refresh button retries */
    }
  }, []);

  useEffect(() => {
    void loadSettings();
  }, [loadSettings]);

  // Audits stream in during every scan — keep the history live while the tab
  // is open instead of freezing at first visit.
  useEffect(() => {
    if (view !== 'audits') return;
    void loadAudits();
    const timer = window.setInterval(() => void loadAudits(), 10000);
    return () => window.clearInterval(timer);
  }, [view, loadAudits]);

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
        response: res.response,
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
          groundingEnabled: provider === 'gemini' ? groundingEnabled : false,
          requireAuditForReal,
          requireApprovalToDeploy,
          auditIntervalSeconds,
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

  const filteredAudits = useMemo(
    () =>
      audits.filter((audit) => {
        if (auditVerdictFilter !== 'ALL' && audit.decision !== auditVerdictFilter) return false;
        if (catalystOnly && !catalystOf(audit)) return false;
        const needle = auditSymbolFilter.trim().toUpperCase();
        return needle === '' || audit.symbol.toUpperCase().includes(needle);
      }),
    [audits, auditVerdictFilter, catalystOnly, auditSymbolFilter],
  );
  const auditStats = useMemo(
    () => ({
      approved: audits.filter((item) => item.decision === 'APPROVED').length,
      rejected: audits.filter((item) => item.decision === 'REJECTED').length,
      catalysts: audits.filter((item) => catalystOf(item)).length,
    }),
    [audits],
  );

  const profile = PROVIDERS.find((item) => item.id === provider)!;
  const keyState = keyLooksValid(provider, apiKey);
  const modelOptions = Array.from(new Set([...MODEL_PRESETS[provider], ...availableModels]));
  const groundingAvailable = provider === 'gemini';

  if (loading) {
    return (
      <div className="section" style={{ textAlign: 'center', padding: '3rem' }}>
        <div className="loading-spinner" />
        <p style={{ marginTop: '1rem', color: 'var(--muted)' }}>Загрузка настроек AI…</p>
      </div>
    );
  }

  return (
    <div style={{ maxWidth: '1080px', margin: '0 auto' }}>
      <div className="section-header" style={{ marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <span style={{ fontSize: '1.75rem' }}>🧠</span>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.4rem', fontWeight: 700 }}>AI Мозг — аудит кандидатов</h2>
            <p style={{ margin: '0.2rem 0 0', color: 'var(--muted)', fontSize: '0.85rem' }}>
              Перед REAL-деплоем каждый кандидат проходит LLM-аудит с новостным вето (UNLOCK / DELIST / EXPLOIT)
            </p>
          </div>
        </div>
        <div style={{ display: 'flex', gap: '0.5rem', alignItems: 'center', flexWrap: 'wrap' }}>
          {settings && (
            <span className={`badge ${settings.enabled ? 'success' : 'neutral'}`}>
              {settings.enabled ? '🟢 Включён' : '⚪ Выключен'}
            </span>
          )}
          {settings?.apiKeyMasked && <span className="badge neutral">Ключ: {settings.apiKeyMasked}</span>}
          {settings && (
            <span className="badge neutral" title={new Date(settings.updatedAt).toLocaleString()}>
              {settings.provider} · {settings.model}
            </span>
          )}
        </div>
      </div>

      <div style={{ display: 'flex', gap: '0.5rem', marginBottom: '1.25rem' }}>
        {(['config', 'audits'] as const).map((item) => (
          <button
            key={item}
            className={`button small ${view === item ? 'primary' : 'secondary'}`}
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
                    border: `1px solid ${provider === item.id ? 'var(--accent)' : 'var(--border)'}`,
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
                  <small style={{ color: 'var(--muted)', display: 'block', marginTop: '0.3rem' }}>{item.tagline}</small>
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
              <button className="button secondary" onClick={() => void handleFetchModels()} disabled={loadingModels}>
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
              <label
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  gap: '0.75rem',
                  opacity: groundingAvailable ? 1 : 0.5,
                }}
                title={groundingAvailable ? undefined : 'Живой поиск доступен только для Gemini'}
              >
                <span>
                  <strong style={{ fontSize: '0.9rem' }}>Живой поиск новостей (google_search)</strong>
                  <small className="muted" style={{ display: 'block' }}>
                    {groundingAvailable
                      ? 'Модель гуглит токен перед вердиктом: анлоки, делисты, эксплойты — по факту, а не по памяти. При сбое инструмента автоматический откат на обычный режим'
                      : 'Недоступно для этого провайдера — переключись на Gemini'}
                  </small>
                </span>
                <input
                  type="checkbox"
                  checked={groundingEnabled && groundingAvailable}
                  disabled={!groundingAvailable}
                  onChange={(e) => setGroundingEnabled(e.target.checked)}
                  style={{ transform: 'scale(1.2)', cursor: groundingAvailable ? 'pointer' : 'not-allowed' }}
                />
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
            <button className="button primary" onClick={() => void handleSave()} disabled={saving || !canManage}>
              {saving ? 'Сохранение…' : '💾 Сохранить'}
            </button>
            <button className="button secondary" onClick={() => void handleTest()} disabled={testing || !canManage}>
              {testing ? 'Проверка…' : '🔌 Проверить подключение'}
            </button>
            <button className="button secondary" onClick={() => void loadSettings()} disabled={loading}>
              ↺ Перезагрузить
            </button>
            <small className="muted">
              Тест с пустым полем ключа проверяет сохранённый ключ
              {canManage ? '' : ' · изменение настроек доступно только администратору'}
            </small>
          </div>

          {testResult && (
            <div className={`banner ${testResult.ok ? 'success' : 'error'}`} style={{ marginTop: '1rem' }}>
              <div>
                {testResult.message}
                {testResult.latency !== undefined && (
                  <span style={{ marginLeft: '0.5rem', opacity: 0.8 }}>Скорость отклика: {testResult.latency} мс</span>
                )}
              </div>
              {testResult.response && (
                <div style={{ marginTop: '0.5rem', fontSize: '0.8rem', opacity: 0.85, whiteSpace: 'pre-wrap' }}>
                  Ответ модели: {testResult.response}
                </div>
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
              <h3>Последние аудиты ({filteredAudits.length}{filteredAudits.length !== audits.length ? ` из ${audits.length}` : ''})</h3>
            </div>
            <div style={{ display: 'flex', gap: '0.75rem', alignItems: 'center' }}>
              <small className="muted">
                ✅ {auditStats.approved} · ⛔ {auditStats.rejected} · 📰 вето {auditStats.catalysts}
              </small>
              <button className="button secondary" style={{ padding: '4px 10px' }} onClick={() => void loadAudits()}>
                🔄 Обновить
              </button>
            </div>
          </div>

          <form className="inline-form" style={{ marginBottom: 12, flexWrap: 'wrap' }} onSubmit={(event) => event.preventDefault()}>
            <input
              placeholder="Символ"
              value={auditSymbolFilter}
              onChange={(event) => setAuditSymbolFilter(event.target.value)}
              style={{ maxWidth: 180 }}
            />
            <select
              value={auditVerdictFilter}
              onChange={(event) => setAuditVerdictFilter(event.target.value as 'ALL' | 'APPROVED' | 'REJECTED')}
              style={{ maxWidth: 150 }}
            >
              <option value="ALL">Все вердикты</option>
              <option value="APPROVED">APPROVED</option>
              <option value="REJECTED">REJECTED</option>
            </select>
            <label style={{ display: 'flex', alignItems: 'center', gap: 6, fontSize: '0.85rem' }}>
              <input type="checkbox" checked={catalystOnly} onChange={(event) => setCatalystOnly(event.target.checked)} />
              только с катализатором
            </label>
            {(auditSymbolFilter !== '' || auditVerdictFilter !== 'ALL' || catalystOnly) && (
              <button
                type="button"
                className="button ghost"
                onClick={() => {
                  setAuditSymbolFilter('');
                  setAuditVerdictFilter('ALL');
                  setCatalystOnly(false);
                }}
              >
                Сбросить
              </button>
            )}
            <span className="muted compact">Обновляется каждые 10 с · клик по строке — детали</span>
          </form>

          {audits.length === 0 ? (
            <div className="empty-state">Аудитов пока нет — включи AI Мозг и дождись скана</div>
          ) : filteredAudits.length === 0 ? (
            <div className="empty-state">По фильтрам ничего не найдено.</div>
          ) : (
            <div className="table-wrap">
              <table>
                <thead>
                  <tr>
                    <th>Время</th>
                    <th>Символ</th>
                    <th>Вердикт</th>
                    <th>Катализатор</th>
                    <th>Модель</th>
                    <th>Обоснование</th>
                  </tr>
                </thead>
                <tbody>
                  {filteredAudits.map((audit) => {
                    const catalyst = catalystOf(audit);
                    const confidence = Number(audit.confidence);
                    const expanded = expandedAudit === audit.id;
                    return (
                      <Fragment key={audit.id}>
                        <tr
                          style={{ cursor: 'pointer' }}
                          onClick={() => setExpandedAudit(expanded ? null : audit.id)}
                          title="Показать детали аудита"
                        >
                          <td><small>{new Date(audit.createdAt).toLocaleString()}</small></td>
                          <td><strong>{audit.symbol}</strong></td>
                          <td>
                            <span
                              className={`badge ${audit.decision === 'APPROVED' ? 'success' : 'danger'}`}
                            >
                              {audit.decision}
                            </span>
                            <small className="muted" style={{ display: 'block' }}>
                              {Number.isFinite(confidence) ? `${Math.round(confidence * 100)}%` : '—'}
                            </small>
                          </td>
                          <td>
                            {catalyst ? (
                              <span
                                className={`badge ${severityClass(catalyst.severity)}`}
                                title={`${catalyst.summary || 'без описания'}${catalyst.eta_hours ? ` · ETA ~${catalyst.eta_hours}ч` : ''}`}
                              >
                                📰 {catalyst.type} · {catalyst.severity}
                              </span>
                            ) : (
                              <small className="muted">—</small>
                            )}
                          </td>
                          <td>
                            <small>{audit.model}</small>
                            <small className="muted" style={{ display: 'block' }}>{audit.latencyMs} мс</small>
                          </td>
                          <td style={{ maxWidth: '420px' }}>
                            <small className="muted">
                              {audit.recommendedParams?.rejection_reason
                                ? audit.recommendedParams.rejection_reason
                                : audit.reasoning}
                            </small>
                          </td>
                        </tr>
                        {expanded && (
                          <tr>
                            <td colSpan={6}>
                              <div style={{ background: 'rgba(0,0,0,0.2)', borderRadius: 6, padding: '0.75rem', fontSize: '0.8rem' }}>
                                <div style={{ marginBottom: '0.4rem' }}>
                                  <strong>Обоснование модели:</strong> {audit.reasoning || '—'}
                                </div>
                                {audit.recommendedParams?.rejection_reason && (
                                  <div style={{ marginBottom: '0.4rem' }}>
                                    <strong>Причина отклонения:</strong> {audit.recommendedParams.rejection_reason}
                                  </div>
                                )}
                                {catalyst && (
                                  <div style={{ marginBottom: '0.4rem' }}>
                                    <strong>Катализатор:</strong> {catalyst.type} / {catalyst.severity}
                                    {catalyst.eta_hours ? ` · ETA ~${catalyst.eta_hours} ч` : ''} — {catalyst.summary || 'без описания'}
                                  </div>
                                )}
                                {audit.recommendedParams && Object.keys(audit.recommendedParams).filter((key) => !['news_catalyst', 'rejection_reason'].includes(key)).length > 0 && (
                                  <div style={{ marginBottom: '0.4rem' }}>
                                    <strong>Параметры от модели:</strong>{' '}
                                    <code>{JSON.stringify(
                                      Object.fromEntries(
                                        Object.entries(audit.recommendedParams).filter(([key]) => !['news_catalyst', 'rejection_reason'].includes(key)),
                                      ),
                                    )}</code>
                                  </div>
                                )}
                                <div className="muted">
                                  Провайдер: {audit.provider} · латентность {audit.latencyMs} мс · режим {audit.regime}
                                  {audit.candidateId ? ` · кандидат ${audit.candidateId.slice(0, 8)}…` : ''}
                                </div>
                              </div>
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
          <div className="panel-actions">
            <span className="muted compact">
              Бэкенд хранит последние записи аудита: вердикт, уверенность, катализатор, параметры и сырой ответ модели.
            </span>
          </div>
        </div>
      )}
    </div>
  );
}
