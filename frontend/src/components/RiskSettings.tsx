import { FormEvent, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { RiskSettings as RiskSettingsModel } from '../types';

interface Props {
  canManage: boolean;
}

export default function RiskSettings({ canManage }: Props) {
  const [settings, setSettings] = useState<RiskSettingsModel | null>(null);
  const [form, setForm] = useState<RiskSettingsModel | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api<RiskSettingsModel>('/api/risk/settings')
      .then((data) => {
        setSettings(data);
        setForm({ ...data });
      })
      .catch((loadError) => setError(describeError(loadError)));
  }, []);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!form) return;
    setBusy(true);
    setError(null);
    setSaved(false);
    try {
      const updated = await api<RiskSettingsModel>('/api/risk/settings', {
        method: 'PUT',
        body: JSON.stringify(form),
      });
      setSettings(updated);
      setForm({ ...updated });
      setSaved(true);
    } catch (submitError) {
      setError(describeError(submitError));
    } finally {
      setBusy(false);
    }
  }

  async function toggleKillSwitch() {
    if (!settings || !form) return;
    setBusy(true);
    setError(null);
    try {
      // Send the form's values: toggling the switch mid-edit must not
      // silently revert the admin's unsaved limit changes.
      const updated = await api<RiskSettingsModel>('/api/risk/settings', {
        method: 'PUT',
        body: JSON.stringify({ ...form, killSwitchEnabled: !settings.killSwitchEnabled }),
      });
      setSettings(updated);
      setForm({ ...updated });
    } catch (toggleError) {
      setError(describeError(toggleError));
    } finally {
      setBusy(false);
    }
  }

  if (!settings || !form) {
    return <div className="empty-state">{error ?? 'Загрузка риск-настроек…'}</div>;
  }

  return (
    <div className="section-stack">
      {error && <div className="alert danger"><span>{error}</span></div>}
      {saved && <div className="alert success"><span>Риск-настройки сохранены.</span></div>}

      <div className={`kill-panel ${settings.killSwitchEnabled ? 'active' : ''}`}>
        <div>
          <span className="eyebrow">DURABLE KILL SWITCH</span>
          <h2>{settings.killSwitchEnabled ? 'Торговля заблокирована' : 'Торговля разрешена'}</h2>
          <p>
            Kill switch живёт в risk_settings (PostgreSQL) и проверяется перед каждым созданием бота и
            ордера. Аварийная остановка включает его автоматически.
          </p>
        </div>
        {canManage && (
          <button className={`button ${settings.killSwitchEnabled ? 'primary' : 'danger'}`} disabled={busy} onClick={() => void toggleKillSwitch()}>
            {settings.killSwitchEnabled ? 'Разрешить торговлю' : 'Включить kill switch'}
          </button>
        )}
      </div>

      <form className="panel" onSubmit={submit}>
        <div className="panel-heading">
          <div>
            <span className="eyebrow">RISK LIMITS</span>
            <h3>Долговечные лимиты риска</h3>
          </div>
          {canManage && (
            <button className="button primary" type="submit" disabled={busy}>
              {busy ? 'Сохранение…' : 'Сохранить'}
            </button>
          )}
        </div>
        <div className="form-grid">
          <label>
            Макс. экспозиция аккаунта, USDT
            <input
              value={form.maxAccountExposureUsd}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxAccountExposureUsd: event.target.value })}
              inputMode="decimal"
            />
          </label>
          <label>
            Макс. экспозиция на символ, USDT
            <input
              value={form.maxSymbolExposureUsd}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxSymbolExposureUsd: event.target.value })}
              inputMode="decimal"
            />
          </label>
          <label>
            Макс. дневной убыток, USDT
            <input
              value={form.maxDailyLossUsd}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxDailyLossUsd: event.target.value })}
              inputMode="decimal"
            />
          </label>
          <label>
            Макс. плечо
            <input
              value={form.maxLeverage}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxLeverage: Number(event.target.value) })}
              inputMode="numeric"
            />
          </label>
          <label>
            Макс. активных грид-ботов
            <input
              value={form.maxActiveGridBots}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxActiveGridBots: Number(event.target.value) })}
              inputMode="numeric"
            />
          </label>
          <label>
            Макс. открытых позиций
            <input
              value={form.maxOpenPositions}
              disabled={!canManage}
              onChange={(event) => setForm({ ...form, maxOpenPositions: Number(event.target.value) })}
              inputMode="numeric"
            />
          </label>
        </div>
      </form>
    </div>
  );
}
