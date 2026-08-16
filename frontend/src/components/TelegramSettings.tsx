import React, { useState, useEffect } from 'react';
import { api, describeError } from '../api';
import { TelegramSettings as TelegramSettingsType } from '../types';

export const TelegramSettings: React.FC = () => {
  const [settings, setSettings] = useState<TelegramSettingsType | null>(null);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<{ success: boolean; message: string } | null>(null);
  const [saveSuccess, setSaveSuccess] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [showToken, setShowToken] = useState(false);

  useEffect(() => {
    fetchSettings();
  }, []);

  const fetchSettings = async () => {
    try {
      setLoading(true);
      setError(null);
      const res = await api<{ data: TelegramSettingsType }>('/api/telegram/settings');
      setSettings(res.data);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setLoading(false);
    }
  };

  const handleSave = async (e?: React.FormEvent) => {
    if (e) e.preventDefault();
    if (!settings) return;
    try {
      setSaving(true);
      setError(null);
      setSaveSuccess(null);
      const res = await api<{ data: TelegramSettingsType; message: string }>('/api/telegram/settings', {
        method: 'PUT',
        body: JSON.stringify(settings),
      });
      setSettings(res.data);
      setSaveSuccess(res.message || 'Настройки сохранены');
      setTimeout(() => setSaveSuccess(null), 4000);
    } catch (err) {
      setError(describeError(err));
    } finally {
      setSaving(false);
    }
  };

  const handleTest = async () => {
    if (!settings) return;
    try {
      setTesting(true);
      setTestResult(null);
      setError(null);
      const res = await api<{ success: boolean; message: string }>('/api/telegram/test', {
        method: 'POST',
        body: JSON.stringify({
          botToken: settings.botToken,
          chatID: settings.chatID,
          topicID: settings.topicID,
        }),
      });
      setTestResult({ success: true, message: res.message || 'Тестовое сообщение отправлено!' });
    } catch (err) {
      setTestResult({ success: false, message: describeError(err) });
    } finally {
      setTesting(false);
    }
  };

  if (loading) {
    return (
      <div className="section" style={{ textAlign: 'center', padding: '3rem' }}>
        <div className="loading-spinner" />
        <p style={{ marginTop: '1rem', color: 'var(--text-secondary)' }}>Загрузка настроек Telegram...</p>
      </div>
    );
  }

  if (!settings) {
    return (
      <div className="section">
        <div className="banner error">Не удалось загрузить настройки Telegram</div>
      </div>
    );
  }

  return (
    <div className="telegram-container" style={{ maxWidth: '960px', margin: '0 auto' }}>
      {/* Header */}
      <div className="section-header" style={{ marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: '0.75rem' }}>
          <span style={{ fontSize: '1.75rem' }}>✈️</span>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.4rem', fontWeight: 700 }}>Telegram Уведомления</h2>
            <p style={{ margin: '0.2rem 0 0', color: 'var(--text-secondary)', fontSize: '0.85rem' }}>
              Мгновенные оповещения об операциях ботов, настраиваемые шаблоны и команды управления
            </p>
          </div>
        </div>
      </div>

      {error && <div className="banner error" style={{ marginBottom: '1rem' }}>{error}</div>}
      {saveSuccess && <div className="banner success" style={{ marginBottom: '1rem' }}>{saveSuccess}</div>}
      {testResult && (
        <div className={`banner ${testResult.success ? 'success' : 'error'}`} style={{ marginBottom: '1rem' }}>
          {testResult.message}
        </div>
      )}

      {/* Main Settings Card */}
      <div className="card" style={{ padding: '1.5rem', marginBottom: '1.5rem' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', borderBottom: '1px solid var(--border-color)', paddingBottom: '1rem', marginBottom: '1.25rem' }}>
          <div>
            <h3 style={{ margin: 0, fontSize: '1.1rem' }}>Статус подключения</h3>
            <span style={{ fontSize: '0.8rem', color: 'var(--text-secondary)' }}>
              Zero-ENV: данные хранятся защищенно в PostgreSQL
            </span>
          </div>
          <label style={{ display: 'flex', alignItems: 'center', gap: '0.5rem', cursor: 'pointer', fontWeight: 600 }}>
            <input
              type="checkbox"
              checked={settings.enabled}
              onChange={(e) => setSettings({ ...settings, enabled: e.target.checked })}
              style={{ width: '18px', height: '18px', accentColor: 'var(--primary-color)' }}
            />
            {settings.enabled ? (
              <span style={{ color: 'var(--success-color, #10b981)' }}>🟢 Включено</span>
            ) : (
              <span style={{ color: 'var(--text-secondary)' }}>⚪ Отключено</span>
            )}
          </label>
        </div>

        <form onSubmit={handleSave}>
          <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(280px, 1fr))', gap: '1.25rem', marginBottom: '1.25rem' }}>
            <div>
              <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
                Токен бота (Bot Token) <span style={{ color: 'red' }}>*</span>
              </label>
              <div style={{ position: 'relative' }}>
                <input
                  type={showToken ? 'text' : 'password'}
                  className="input"
                  placeholder="123456789:ABCdefGHIjklMNOpqrSTUvwxYZ"
                  value={settings.botToken}
                  onChange={(e) => setSettings({ ...settings, botToken: e.target.value })}
                  style={{ width: '100%', paddingRight: '45px' }}
                />
                <button
                  type="button"
                  onClick={() => setShowToken(!showToken)}
                  style={{
                    position: 'absolute',
                    right: '8px',
                    top: '50%',
                    transform: 'translateY(-50%)',
                    background: 'none',
                    border: 'none',
                    cursor: 'pointer',
                    fontSize: '0.9rem',
                    color: 'var(--text-secondary)',
                  }}
                  title={showToken ? 'Скрыть токен' : 'Показать токен'}
                >
                  {showToken ? '🙈' : '👁️'}
                </button>
              </div>
              <small style={{ color: 'var(--text-secondary)', fontSize: '0.75rem' }}>
                Получите у <code>@BotFather</code> в Telegram
              </small>
            </div>

            <div>
              <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
                Chat ID <span style={{ color: 'red' }}>*</span>
              </label>
              <input
                type="text"
                className="input"
                placeholder="123456789 или -100123456789"
                value={settings.chatID}
                onChange={(e) => setSettings({ ...settings, chatID: e.target.value })}
                style={{ width: '100%' }}
              />
              <small style={{ color: 'var(--text-secondary)', fontSize: '0.75rem' }}>
                Ваш личный ID или ID группы (узнать в <code>@userinfobot</code>)
              </small>
            </div>

            <div>
              <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
                Topic / Thread ID (Опционально)
              </label>
              <input
                type="text"
                className="input"
                placeholder="Например: 12"
                value={settings.topicID}
                onChange={(e) => setSettings({ ...settings, topicID: e.target.value })}
                style={{ width: '100%' }}
              />
              <small style={{ color: 'var(--text-secondary)', fontSize: '0.75rem' }}>
                Для супергрупп с включенными темами (форумами)
              </small>
            </div>

            <div>
              <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
                Интервал сводки (PnL Digest)
              </label>
              <select
                className="input"
                value={settings.digestIntervalMinutes}
                onChange={(e) => setSettings({ ...settings, digestIntervalMinutes: Number(e.target.value) })}
                style={{ width: '100%' }}
              >
                <option value={15}>Каждые 15 минут</option>
                <option value={30}>Каждые 30 минут</option>
                <option value={60}>Каждый час (Рекомендуется)</option>
                <option value={120}>Каждые 2 часа</option>
                <option value={240}>Каждые 4 часа</option>
                <option value={720}>Каждые 12 часов</option>
              </select>
              <small style={{ color: 'var(--text-secondary)', fontSize: '0.75rem' }}>
                Периодичность отправки текущего PnL и статуса ботов
              </small>
            </div>
          </div>

          <div style={{ display: 'flex', gap: '0.75rem', flexWrap: 'wrap', marginTop: '1.25rem' }}>
            <button
              type="button"
              className="button secondary"
              onClick={handleTest}
              disabled={testing || !settings.botToken || !settings.chatID}
              style={{ display: 'flex', alignItems: 'center', gap: '0.4rem' }}
            >
              {testing ? '⏳ Отправка...' : '🧪 Отправить тестовое сообщение'}
            </button>
            <button
              type="submit"
              className="button primary"
              disabled={saving}
              style={{ display: 'flex', alignItems: 'center', gap: '0.4rem', marginLeft: 'auto' }}
            >
              {saving ? '⏳ Сохранение...' : '💾 Сохранить настройки'}
            </button>
          </div>
        </form>
      </div>

      {/* Event Filters */}
      <div className="card" style={{ padding: '1.5rem', marginBottom: '1.5rem' }}>
        <h3 style={{ margin: '0 0 1rem', fontSize: '1.1rem', borderBottom: '1px solid var(--border-color)', paddingBottom: '0.75rem' }}>
          🔔 События для отправки уведомлений
        </h3>
        <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(260px, 1fr))', gap: '1rem' }}>
          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyBotCreated}
              onChange={(e) => setSettings({ ...settings, notifyBotCreated: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>🚀 Запуск бота</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Оповещение при открытии новой сетки автопилотом
              </div>
            </div>
          </label>

          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyTakeProfit}
              onChange={(e) => setSettings({ ...settings, notifyTakeProfit: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>🎯 Тейк-профит (PnL Target)</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Закрытие бота с прибылью при достижении цели
              </div>
            </div>
          </label>

          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyStopLoss}
              onChange={(e) => setSettings({ ...settings, notifyStopLoss: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>🛡️ Стоп-лосс / Защита</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Фиксация убытка или срабатывание защитного стопа
              </div>
            </div>
          </label>

          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyRangeAdjust}
              onChange={(e) => setSettings({ ...settings, notifyRangeAdjust: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>🔄 Сдвиг диапазона (Adjust)</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Автоматическая подтяжка сетки за ушедшим рынком
              </div>
            </div>
          </label>

          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyDigest}
              onChange={(e) => setSettings({ ...settings, notifyDigest: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>📊 Периодическая сводка</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Регулярный отчет по активным ботам и суммарному PnL
              </div>
            </div>
          </label>

          <label style={{ display: 'flex', alignItems: 'flex-start', gap: '0.6rem', cursor: 'pointer' }}>
            <input
              type="checkbox"
              checked={settings.notifyEmergency}
              onChange={(e) => setSettings({ ...settings, notifyEmergency: e.target.checked })}
              style={{ marginTop: '3px' }}
            />
            <div>
              <div style={{ fontWeight: 600, fontSize: '0.9rem' }}>🚨 Аварийный стоп / Kill Switch</div>
              <div style={{ fontSize: '0.75rem', color: 'var(--text-secondary)' }}>
                Критические алерты безопасности и ручные экстренные остановки
              </div>
            </div>
          </label>
        </div>
      </div>

      {/* Templates Editor */}
      <div className="card" style={{ padding: '1.5rem', marginBottom: '1.5rem' }}>
        <h3 style={{ margin: '0 0 0.5rem', fontSize: '1.1rem' }}>📝 Шаблоны сообщений (HTML)</h3>
        <p style={{ margin: '0 0 1.25rem', fontSize: '0.8rem', color: 'var(--text-secondary)' }}>
          Используйте переменные <code>{'{{bot_number}}'}</code>, <code>{'{{symbol}}'}</code>, <code>{'{{pnl_usdt}}'}</code>, <code>{'{{direction}}'}</code>, <code>{'{{leverage}}'}</code>, <code>{'{{reason}}'}</code>. Поддерживается HTML-разметка Telegram (<code>&lt;b&gt;</code>, <code>&lt;code&gt;</code>, <code>&lt;i&gt;</code>).
        </p>

        <div style={{ display: 'flex', flexDirection: 'column', gap: '1.25rem' }}>
          <div>
            <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
              🚀 Шаблон: Запуск бота
            </label>
            <textarea
              className="input"
              rows={4}
              value={settings.templateBotCreated}
              onChange={(e) => setSettings({ ...settings, templateBotCreated: e.target.value })}
              style={{ width: '100%', fontFamily: 'monospace', fontSize: '0.82rem' }}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
              🎯 Шаблон: Тейк-профит (Take-Profit)
            </label>
            <textarea
              className="input"
              rows={3}
              value={settings.templateTakeProfit}
              onChange={(e) => setSettings({ ...settings, templateTakeProfit: e.target.value })}
              style={{ width: '100%', fontFamily: 'monospace', fontSize: '0.82rem' }}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
              🛡️ Шаблон: Стоп-лосс (Stop-Loss)
            </label>
            <textarea
              className="input"
              rows={3}
              value={settings.templateStopLoss}
              onChange={(e) => setSettings({ ...settings, templateStopLoss: e.target.value })}
              style={{ width: '100%', fontFamily: 'monospace', fontSize: '0.82rem' }}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
              🔄 Шаблон: Сдвиг диапазона (Adjust)
            </label>
            <textarea
              className="input"
              rows={3}
              value={settings.templateRangeAdjust}
              onChange={(e) => setSettings({ ...settings, templateRangeAdjust: e.target.value })}
              style={{ width: '100%', fontFamily: 'monospace', fontSize: '0.82rem' }}
            />
          </div>

          <div>
            <label style={{ display: 'block', fontSize: '0.85rem', fontWeight: 600, marginBottom: '0.35rem' }}>
              📊 Шаблон: Периодическая сводка PnL (Digest)
            </label>
            <textarea
              className="input"
              rows={4}
              value={settings.templateDigest}
              onChange={(e) => setSettings({ ...settings, templateDigest: e.target.value })}
              style={{ width: '100%', fontFamily: 'monospace', fontSize: '0.82rem' }}
            />
          </div>
        </div>

        <div style={{ marginTop: '1.25rem', textAlign: 'right' }}>
          <button
            type="button"
            className="button primary"
            onClick={() => handleSave()}
            disabled={saving}
          >
            {saving ? '⏳ Сохранение...' : '💾 Сохранить шаблоны'}
          </button>
        </div>
      </div>

      {/* Bot Commands Help */}
      <div className="card" style={{ padding: '1.25rem', background: 'var(--surface-color-subtle, rgba(255,255,255,0.02))' }}>
        <h4 style={{ margin: '0 0 0.5rem', fontSize: '0.95rem' }}>🎮 Интерактивные команды в Telegram боте</h4>
        <p style={{ margin: '0 0 0.75rem', fontSize: '0.8rem', color: 'var(--text-secondary)' }}>
          Вы можете отправлять боту команды прямо в чат Telegram:
        </p>
        <ul style={{ margin: 0, paddingLeft: '1.25rem', fontSize: '0.82rem', color: 'var(--text-secondary)' }}>
          <li><code>/status</code> — Мгновенный отчет по количеству активных ботов и зафиксированному PnL</li>
          <li><code>/kill</code> — Экстренная активация Kill Switch (блокирует запуск новых позиций)</li>
        </ul>
      </div>
    </div>
  );
};
