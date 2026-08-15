import { useState, useEffect } from 'react';
import { api, describeError } from '../api';
import { LLMSettings, LLMAuditRecord } from '../types';

interface LLMSettingsModalProps {
  onClose: () => void;
  onSaved?: () => void;
}

const MODEL_PRESETS: Record<string, string[]> = {
  gemini: [
    'gemini-2.0-flash',
    'gemini-2.5-flash',
    'gemini-2.0-flash-thinking-exp-01-21',
    'gemini-2.0-pro-exp-02-05',
    'gemini-1.5-pro',
  ],
  anthropic: [
    'claude-3-7-sonnet-20250219',
    'claude-3-5-sonnet-20241022',
    'claude-3-5-haiku-20241022',
  ],
  openrouter: [
    'anthropic/claude-3.7-sonnet',
    'google/gemini-2.0-flash-001',
    'deepseek/deepseek-r1',
    'openai/o3-mini',
  ],
  custom: [
    'default',
  ],
};

export function LLMSettingsModal({ onClose, onSaved }: LLMSettingsModalProps) {
  const [activeTab, setActiveTab] = useState<'config' | 'audits'>('config');
  const [settings, setSettings] = useState<LLMSettings | null>(null);
  const [audits, setAudits] = useState<LLMAuditRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string; latency?: number } | null>(null);
  const [message, setMessage] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

  // Form states
  const [enabled, setEnabled] = useState(false);
  const [provider, setProvider] = useState<'gemini' | 'anthropic' | 'openrouter' | 'custom'>('gemini');
  const [apiKey, setApiKey] = useState('');
  const [model, setModel] = useState('gemini-2.0-flash');
  const [baseUrl, setBaseUrl] = useState('');
  const [temperature, setTemperature] = useState(0.2);
  const [thinkingEnabled, setThinkingEnabled] = useState(true);

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
      setModel(data.model || 'gemini-2.0-flash');
      setBaseUrl(data.baseUrl || '');
      setTemperature(data.temperature ?? 0.2);
      setThinkingEnabled(data.thinkingEnabled ?? true);
      setApiKey('');
    } catch (err) {
      setMessage({ kind: 'danger', text: describeError(err) });
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
    if (activeTab === 'audits') {
      loadAudits();
    }
  }, [activeTab]);

  function handleProviderChange(newProvider: 'gemini' | 'anthropic' | 'openrouter' | 'custom') {
    setProvider(newProvider);
    setTestResult(null);
    const presets = MODEL_PRESETS[newProvider] || [];
    if (presets.length > 0 && !presets.includes(model)) {
      setModel(presets[0]);
    }
    if (newProvider === 'openrouter' && !baseUrl) {
      setBaseUrl('https://openrouter.ai/api/v1/chat/completions');
    }
  }

  async function handleTest() {
    setTesting(true);
    setTestResult(null);
    setMessage(null);
    try {
      const res = await api<{ ok: boolean; response?: string; error?: string; latencyMs: number }>('/api/llm/test', {
        method: 'POST',
        body: JSON.stringify({
          provider,
          apiKey: apiKey.trim() || settings?.apiKeyMasked,
          model,
          baseUrl,
          temperature,
          thinkingEnabled,
        }),
      });
      if (res.ok) {
        setTestResult({
          ok: true,
          message: `Подключение успешно! Ответ: ${res.response || 'OK'}`,
          latency: res.latencyMs,
        });
      } else {
        setTestResult({
          ok: false,
          message: `Ошибка API: ${res.error}`,
          latency: res.latencyMs,
        });
      }
    } catch (err) {
      setTestResult({
        ok: false,
        message: `Сбой запроса: ${describeError(err)}`,
      });
    } finally {
      setTesting(false);
    }
  }

  async function handleSave() {
    setSaving(true);
    setMessage(null);
    try {
      const updated = await api<LLMSettings>('/api/llm/settings', {
        method: 'PUT',
        body: JSON.stringify({
          id: 1,
          enabled,
          provider,
          apiKey: apiKey.trim() || undefined,
          model,
          baseUrl,
          temperature,
          thinkingEnabled,
          auditIntervalSeconds: 3600,
        }),
      });
      setSettings(updated);
      setApiKey('');
      setMessage({ kind: 'success', text: 'Настройки AI Мозга успешно сохранены в PostgreSQL.' });
      onSaved?.();
    } catch (err) {
      setMessage({ kind: 'danger', text: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="modal-overlay" onClick={onClose}>
      <div className="modal-content" style={{ maxWidth: '680px' }} onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <div>
            <h3>🧠 AI Мозг & LLM Аналитика</h3>
            <p className="muted" style={{ margin: 0, fontSize: '13px' }}>
              Нейросетевой аудит кандидатов (Gemini / Claude 3.7 / OpenRouter)
            </p>
          </div>
          <button className="btn btn-secondary btn-small" onClick={onClose}>✕</button>
        </div>

        {message && (
          <div className={`toast-banner ${message.kind === 'danger' ? 'danger' : 'success'}`} style={{ margin: '14px 0' }}>
            {message.text}
          </div>
        )}

        <div style={{ display: 'flex', gap: '8px', borderBottom: '1px solid var(--border)', paddingBottom: '8px', margin: '14px 0' }}>
          <button
            className={`btn btn-small ${activeTab === 'config' ? 'btn-primary' : 'btn-secondary'}`}
            onClick={() => setActiveTab('config')}
          >
            ⚙️ Конфигурация модели
          </button>
          <button
            className={`btn btn-small ${activeTab === 'audits' ? 'btn-primary' : 'btn-secondary'}`}
            onClick={() => setActiveTab('audits')}
          >
            📜 Журнал AI-аудитов
          </button>
        </div>

        {loading ? (
          <div style={{ padding: '30px', textAlign: 'center' }} className="muted">Загрузка настроек…</div>
        ) : activeTab === 'config' ? (
          <div className="section-stack" style={{ gap: '16px' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', background: 'var(--surface-subtle)', padding: '12px 16px', borderRadius: '10px', border: '1px solid var(--border)' }}>
              <div>
                <strong style={{ display: 'block', fontSize: '14px' }}>Включить AI-фильтрацию кандидатов</strong>
                <small className="muted">Сканер передает каждого кандидата в LLM для проверки микроструктуры</small>
              </div>
              <label className="toggle-switch">
                <input
                  type="checkbox"
                  checked={enabled}
                  onChange={(e) => setEnabled(e.target.checked)}
                />
                <span className="slider round"></span>
              </label>
            </div>

            <div>
              <label style={{ display: 'block', marginBottom: '6px', fontSize: '13px', fontWeight: 600 }}>
                Провайдер AI:
              </label>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: '8px' }}>
                <button
                  type="button"
                  className={`btn btn-small ${provider === 'gemini' ? 'btn-primary' : 'btn-secondary'}`}
                  onClick={() => handleProviderChange('gemini')}
                >
                  ✨ Google Gemini
                </button>
                <button
                  type="button"
                  className={`btn btn-small ${provider === 'anthropic' ? 'btn-primary' : 'btn-secondary'}`}
                  onClick={() => handleProviderChange('anthropic')}
                >
                  ⚡ Claude 3.7
                </button>
                <button
                  type="button"
                  className={`btn btn-small ${provider === 'openrouter' ? 'btn-primary' : 'btn-secondary'}`}
                  onClick={() => handleProviderChange('openrouter')}
                >
                  🌐 OpenRouter
                </button>
              </div>
            </div>

            <div>
              <label style={{ display: 'block', marginBottom: '6px', fontSize: '13px', fontWeight: 600 }}>
                API-Ключ ({provider.toUpperCase()}):
              </label>
              <input
                type="password"
                className="input"
                placeholder={settings?.apiKeyMasked ? `Текущий: ${settings.apiKeyMasked}` : 'Вставьте ваш API ключ...'}
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
                style={{ width: '100%' }}
              />
              <small className="muted" style={{ display: 'block', marginTop: '4px' }}>
                Ключ сохраняется в PostgreSQL в зашифрованном виде (Zero-ENV).
              </small>
            </div>

            <div>
              <label style={{ display: 'block', marginBottom: '6px', fontSize: '13px', fontWeight: 600 }}>
                Модель нейросети:
              </label>
              <div style={{ display: 'flex', flexWrap: 'wrap', gap: '6px', marginBottom: '8px' }}>
                {(MODEL_PRESETS[provider] || []).map((p) => (
                  <button
                    key={p}
                    type="button"
                    className={`btn btn-small ${model === p ? 'btn-primary' : 'btn-secondary'}`}
                    style={{ fontSize: '11px', padding: '3px 8px' }}
                    onClick={() => setModel(p)}
                  >
                    {p}
                  </button>
                ))}
              </div>
              <input
                type="text"
                className="input"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                placeholder="Имя модели..."
                style={{ width: '100%' }}
              />
            </div>

            <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: '12px' }}>
              <div>
                <label style={{ display: 'block', marginBottom: '6px', fontSize: '13px', fontWeight: 600 }}>
                  Температура:
                </label>
                <input
                  type="number"
                  step="0.05"
                  min="0.0"
                  max="1.0"
                  className="input"
                  value={temperature}
                  onChange={(e) => setTemperature(parseFloat(e.target.value) || 0.2)}
                  style={{ width: '100%' }}
                />
              </div>

              <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'space-between', background: 'var(--surface-subtle)', padding: '8px 12px', borderRadius: '8px', border: '1px solid var(--border)' }}>
                <div>
                  <span style={{ fontSize: '12px', fontWeight: 600, display: 'block' }}>Thinking Mode</span>
                  <small className="muted">Рассуждения (Reasoning)</small>
                </div>
                <input
                  type="checkbox"
                  checked={thinkingEnabled}
                  onChange={(e) => setThinkingEnabled(e.target.checked)}
                />
              </div>
            </div>

            {testResult && (
              <div
                className={`toast-banner ${testResult.ok ? 'success' : 'danger'}`}
                style={{ fontSize: '13px', padding: '10px 14px' }}
              >
                <div>{testResult.message}</div>
                {testResult.latency !== undefined && (
                  <small style={{ opacity: 0.85 }}>Задержка: {testResult.latency} ms</small>
                )}
              </div>
            )}

            <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginTop: '10px', paddingTop: '14px', borderTop: '1px solid var(--border)' }}>
              <button
                type="button"
                className="btn btn-secondary"
                disabled={testing}
                onClick={handleTest}
              >
                {testing ? 'Тестирование…' : '🧪 Проверить подключение'}
              </button>

              <div style={{ display: 'flex', gap: '8px' }}>
                <button type="button" className="btn btn-secondary" onClick={onClose}>
                  Отмена
                </button>
                <button
                  type="button"
                  className="btn btn-primary"
                  disabled={saving}
                  onClick={handleSave}
                >
                  {saving ? 'Сохранение…' : 'Сохранить настройки'}
                </button>
              </div>
            </div>
          </div>
        ) : (
          <div>
            {audits.length === 0 ? (
              <div style={{ padding: '30px', textAlign: 'center' }} className="muted">
                История аудитов пуста. Запустите автопилот с включенным AI Мозгом для появления записей.
              </div>
            ) : (
              <div className="table-responsive" style={{ maxHeight: '380px', overflowY: 'auto' }}>
                <table className="table">
                  <thead>
                    <tr>
                      <th>Время</th>
                      <th>Символ</th>
                      <th>Модель</th>
                      <th>Решение</th>
                      <th>Уверенность</th>
                      <th>Обоснование</th>
                    </tr>
                  </thead>
                  <tbody>
                    {audits.map((a) => (
                      <tr key={a.id}>
                        <td style={{ whiteSpace: 'nowrap', fontSize: '12px' }}>
                          {new Date(a.createdAt).toLocaleTimeString()}
                        </td>
                        <td><strong>{a.symbol}</strong></td>
                        <td style={{ fontSize: '11px' }} className="muted">{a.model}</td>
                        <td>
                          <span className={`badge ${a.decision === 'APPROVED' ? 'success' : 'danger'}`}>
                            {a.decision === 'APPROVED' ? '✓ APPROVED' : '✕ REJECTED'}
                          </span>
                        </td>
                        <td>{Math.round(Number(a.confidence) * 100)}%</td>
                        <td style={{ fontSize: '12px', maxWidth: '240px' }} title={a.reasoning}>
                          {a.reasoning}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
