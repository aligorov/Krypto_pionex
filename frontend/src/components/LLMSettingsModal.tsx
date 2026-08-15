import { useState, useEffect } from 'react';
import { api, describeError } from '../api';
import type { LLMSettings, LLMAuditRecord } from '../types';

interface Props {
  onClose: () => void;
  onSaved?: () => void;
}

const MODEL_PRESETS: Record<string, string[]> = {
  gemini: [
    'gemini-2.0-flash',
    'gemini-2.0-flash-lite',
    'gemini-2.0-pro-exp-02-05',
    'gemini-1.5-flash',
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

export function LLMSettingsModal({ onClose, onSaved }: Props) {
  const [tab, setTab] = useState<'config' | 'audits'>('config');
  const [settings, setSettings] = useState<LLMSettings | null>(null);
  const [audits, setAudits] = useState<LLMAuditRecord[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ ok: boolean; message: string; latency?: number } | null>(null);
  const [statusMessage, setStatusMessage] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

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
      setStatusMessage({ kind: 'danger', text: describeError(err) });
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
    if (tab === 'audits') {
      loadAudits();
    }
  }, [tab]);

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
    setStatusMessage(null);
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
    setStatusMessage(null);
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
      setStatusMessage({ kind: 'success', text: 'Настройки AI Мозга успешно сохранены в PostgreSQL.' });
      onSaved?.();
    } catch (err) {
      setStatusMessage({ kind: 'danger', text: describeError(err) });
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="modal-backdrop" onClick={onClose}>
      <div className="modal-dialog security-modal" style={{ maxWidth: '640px' }} onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <div>
            <span className="eyebrow">AI INTELLIGENCE</span>
            <h2>🧠 AI Мозг & LLM Аналитика</h2>
          </div>
          <button className="modal-close-btn" onClick={onClose} title="Закрыть">
            ×
          </button>
        </div>

        <div className="modal-tabs">
          <button
            className={`modal-tab ${tab === 'config' ? 'active' : ''}`}
            onClick={() => setTab('config')}
          >
            ⚙️ Конфигурация модели
          </button>
          <button
            className={`modal-tab ${tab === 'audits' ? 'active' : ''}`}
            onClick={() => setTab('audits')}
          >
            📜 Журнал AI-аудитов
            {audits.length > 0 && (
              <span className="badge" style={{ marginLeft: 8 }}>{audits.length}</span>
            )}
          </button>
        </div>

        <div className="modal-body">
          {statusMessage && (
            <div className={`alert ${statusMessage.kind === 'danger' ? 'danger' : 'success'}`} style={{ marginBottom: 16 }}>
              {statusMessage.text}
            </div>
          )}

          {loading ? (
            <div className="empty-state">Загрузка настроек…</div>
          ) : tab === 'config' ? (
            <div className="form-stack">
              <div style={{
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                padding: '12px 16px',
                background: 'rgba(255, 255, 255, 0.03)',
                borderRadius: '8px',
                border: '1px solid rgba(255, 255, 255, 0.08)'
              }}>
                <div>
                  <strong style={{ display: 'block', fontSize: '0.95rem' }}>Активировать AI-фильтрацию кандидатов</strong>
                  <small className="muted">Передавать срез свечей и индикаторов в LLM перед открытием сеток</small>
                </div>
                <label style={{ cursor: 'pointer', margin: 0 }}>
                  <input
                    type="checkbox"
                    checked={enabled}
                    onChange={(e) => setEnabled(e.target.checked)}
                    style={{ transform: 'scale(1.3)', cursor: 'pointer' }}
                  />
                </label>
              </div>

              <div>
                <label style={{ marginBottom: 6, display: 'block', fontWeight: 600 }}>Провайдер нейросети</label>
                <div style={{ display: 'grid', gridTemplateColumns: 'repeat(3, 1fr)', gap: 8 }}>
                  <button
                    type="button"
                    className={`button ${provider === 'gemini' ? 'primary' : 'secondary'}`}
                    onClick={() => handleProviderChange('gemini')}
                  >
                    ✨ Google Gemini
                  </button>
                  <button
                    type="button"
                    className={`button ${provider === 'anthropic' ? 'primary' : 'secondary'}`}
                    onClick={() => handleProviderChange('anthropic')}
                  >
                    ⚡ Claude 3.7
                  </button>
                  <button
                    type="button"
                    className={`button ${provider === 'openrouter' ? 'primary' : 'secondary'}`}
                    onClick={() => handleProviderChange('openrouter')}
                  >
                    🌐 OpenRouter
                  </button>
                </div>
              </div>

              <label>
                API-Ключ ({provider.toUpperCase()})
                <input
                  type="password"
                  placeholder={settings?.apiKeyMasked ? `Сохранён: ${settings.apiKeyMasked}` : 'Вставьте ваш API-ключ...'}
                  value={apiKey}
                  onChange={(e) => setApiKey(e.target.value)}
                  autoComplete="off"
                />
                <small className="muted">Ключ надёжно сохраняется в PostgreSQL (Zero-ENV) и маскируется в браузере.</small>
              </label>

              <div>
                <label style={{ marginBottom: 6, display: 'block', fontWeight: 600 }}>Модель</label>
                <div style={{ display: 'flex', flexWrap: 'wrap', gap: 6, marginBottom: 8 }}>
                  {(MODEL_PRESETS[provider] || []).map((p) => (
                    <button
                      key={p}
                      type="button"
                      className={`button small ${model === p ? 'primary' : 'secondary'}`}
                      onClick={() => setModel(p)}
                      style={{ fontSize: '0.8rem', padding: '3px 8px' }}
                    >
                      {p}
                    </button>
                  ))}
                </div>
                <input
                  type="text"
                  value={model}
                  onChange={(e) => setModel(e.target.value)}
                  placeholder="Имя модели (например, gemini-2.0-flash, claude-3-7-sonnet-20250219)..."
                />
              </div>

              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 12 }}>
                <label>
                  Температура генерации
                  <input
                    type="number"
                    step="0.05"
                    min="0.0"
                    max="1.0"
                    value={temperature}
                    onChange={(e) => setTemperature(parseFloat(e.target.value) || 0.2)}
                  />
                </label>

                <div style={{
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  padding: '8px 12px',
                  background: 'rgba(255, 255, 255, 0.03)',
                  borderRadius: '8px',
                  border: '1px solid rgba(255, 255, 255, 0.08)',
                  marginTop: '1.2rem'
                }}>
                  <div>
                    <span style={{ fontSize: '0.85rem', fontWeight: 600, display: 'block' }}>Thinking Mode</span>
                    <small className="muted">Глубокие рассуждения</small>
                  </div>
                  <input
                    type="checkbox"
                    checked={thinkingEnabled}
                    onChange={(e) => setThinkingEnabled(e.target.checked)}
                    style={{ transform: 'scale(1.2)', cursor: 'pointer' }}
                  />
                </div>
              </div>

              {testResult && (
                <div className={`alert ${testResult.ok ? 'success' : 'danger'}`} style={{ fontSize: '0.85rem' }}>
                  <div>{testResult.message}</div>
                  {testResult.latency !== undefined && (
                    <small style={{ opacity: 0.85, display: 'block', marginTop: 4 }}>
                      Скорость отклика: {testResult.latency} ms
                    </small>
                  )}
                </div>
              )}

              <div className="modal-actions" style={{ display: 'flex', justifyContent: 'space-between', marginTop: 12 }}>
                <button
                  type="button"
                  className="button secondary"
                  disabled={testing}
                  onClick={handleTest}
                >
                  {testing ? 'Проверка связи…' : '🧪 Проверить подключение'}
                </button>

                <div style={{ display: 'flex', gap: 8 }}>
                  <button type="button" className="button secondary" onClick={onClose}>
                    Закрыть
                  </button>
                  <button
                    type="button"
                    className="button primary"
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
                <div className="empty-state">
                  История аудитов пуста. Включите AI Мозг и запустите сканирование кандидатов.
                </div>
              ) : (
                <div className="table-wrap" style={{ maxHeight: '380px', overflowY: 'auto' }}>
                  <table>
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
                          <td style={{ whiteSpace: 'nowrap', fontSize: '0.8rem' }}>
                            {new Date(a.createdAt).toLocaleTimeString()}
                          </td>
                          <td><strong>{a.symbol}</strong></td>
                          <td style={{ fontSize: '0.75rem' }} className="muted">{a.model}</td>
                          <td>
                            <span className={`badge ${a.decision === 'APPROVED' ? 'success' : 'danger'}`}>
                              {a.decision === 'APPROVED' ? '✓ APPROVED' : '✕ REJECTED'}
                            </span>
                          </td>
                          <td>{Math.round(Number(a.confidence) * 100)}%</td>
                          <td style={{ fontSize: '0.8rem', maxWidth: '240px' }} title={a.reasoning}>
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
    </div>
  );
}
