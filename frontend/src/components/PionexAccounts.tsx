import { FormEvent, useCallback, useEffect, useState } from 'react';
import { api, describeError } from '../api';
import type { Account } from '../types';

interface Props {
  canManage: boolean;
}

interface AccountForm {
  name: string;
  apiKey: string;
  apiSecret: string;
  hasFuturesPermission: boolean;
  hasBotPermission: boolean;
  verifyNow: boolean;
}

const emptyForm: AccountForm = {
  name: '',
  apiKey: '',
  apiSecret: '',
  hasFuturesPermission: true,
  hasBotPermission: true,
  verifyNow: true,
};

export default function PionexAccounts({ canManage }: Props) {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [form, setForm] = useState<AccountForm>(emptyForm);
  const [showForm, setShowForm] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  const [message, setMessage] = useState<{ kind: 'success' | 'danger'; text: string } | null>(null);

  const load = useCallback(async () => {
    const result = await api<Account[]>('/api/accounts');
    setAccounts(result);
  }, []);

  useEffect(() => {
    let active = true;
    api<Account[]>('/api/accounts')
      .then((result) => {
        if (active) setAccounts(result);
      })
      .catch((error: unknown) => {
        if (active) setMessage({ kind: 'danger', text: describeError(error) });
      });
    return () => {
      active = false;
    };
  }, []);

  async function createAccount(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy('create');
    setMessage(null);
    try {
      const created = await api<Account>('/api/accounts', {
        method: 'POST',
        body: JSON.stringify({
          name: form.name,
          apiKey: form.apiKey,
          apiSecret: form.apiSecret,
          isPaper: false,
          hasFuturesPermission: form.hasFuturesPermission,
          hasBotPermission: form.hasBotPermission,
        }),
      });
      let text = 'Аккаунт сохранён в PostgreSQL. Ключи зашифрованы и не возвращаются в браузер.';
      if (form.verifyNow) {
        try {
          await api<Account>(`/api/accounts/${created.id}/verify`, { method: 'POST' });
          text = 'Аккаунт сохранён, Futures read-доступ проверен через Pionex и аккаунт включён.';
        } catch (error: unknown) {
          text = `Аккаунт сохранён выключенным, но проверка Pionex не прошла: ${describeError(error)}`;
        }
      }
      setForm(emptyForm);
      setShowForm(false);
      setMessage({ kind: form.verifyNow && text.includes('не прошла') ? 'danger' : 'success', text });
      await load();
    } catch (error: unknown) {
      setMessage({ kind: 'danger', text: describeError(error) });
    } finally {
      setBusy(null);
    }
  }

  async function verify(account: Account) {
    setBusy(`verify:${account.id}`);
    setMessage(null);
    try {
      await api<Account>(`/api/accounts/${account.id}/verify`, { method: 'POST' });
      setMessage({
        kind: 'success',
        text: 'Futures read-доступ подтверждён. Bot/Trade permission остаётся декларацией оператора: Pionex не даёт безопасной read-only проверки записи.',
      });
      await load();
    } catch (error: unknown) {
      setMessage({ kind: 'danger', text: describeError(error) });
      await load();
    } finally {
      setBusy(null);
    }
  }

  async function toggle(account: Account) {
    setBusy(`toggle:${account.id}`);
    setMessage(null);
    try {
      await api<Account>(`/api/accounts/${account.id}`, {
        method: 'PATCH',
        body: JSON.stringify({ isEnabled: !account.isEnabled }),
      });
      await load();
    } catch (error: unknown) {
      setMessage({ kind: 'danger', text: describeError(error) });
    } finally {
      setBusy(null);
    }
  }

  async function remove(account: Account) {
    if (!window.confirm(`Удалить аккаунт «${account.name}» и его зашифрованные credentials?`)) {
      return;
    }
    setBusy(`delete:${account.id}`);
    setMessage(null);
    try {
      await api<{ ok: boolean }>(`/api/accounts/${account.id}`, { method: 'DELETE' });
      setMessage({ kind: 'success', text: 'Аккаунт и credentials удалены.' });
      await load();
    } catch (error: unknown) {
      setMessage({ kind: 'danger', text: describeError(error) });
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="section-stack">
      {message && (
        <div className={`alert ${message.kind}`}>
          <span>{message.text}</span>
          <button type="button" onClick={() => setMessage(null)} aria-label="Закрыть">×</button>
        </div>
      )}

      <section className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">DATABASE-BACKED · PIONEX ONLY</span>
            <h2>Pionex API-аккаунты</h2>
            <p className="muted">
              Секреты шифруются на backend. В браузер, API-ответы, audit и логи возвращается только fingerprint.
            </p>
          </div>
          {canManage && (
            <button className="button primary" type="button" onClick={() => setShowForm((value) => !value)}>
              {showForm ? 'Закрыть' : '+ Добавить API Pionex'}
            </button>
          )}
        </div>

        {showForm && (
          <form className="account-form card-inset" onSubmit={createAccount}>
            <label>
              Название аккаунта
              <input
                required
                minLength={3}
                maxLength={64}
                value={form.name}
                onChange={(event) => setForm({ ...form, name: event.target.value })}
                placeholder="Pionex Futures Main"
              />
            </label>
            <label>
              PIONEX-KEY
              <input
                required
                autoComplete="off"
                value={form.apiKey}
                onChange={(event) => setForm({ ...form, apiKey: event.target.value })}
                placeholder="Вставьте API key"
              />
            </label>
            <label>
              PIONEX-SECRET
              <input
                required
                type="password"
                autoComplete="new-password"
                value={form.apiSecret}
                onChange={(event) => setForm({ ...form, apiSecret: event.target.value })}
                placeholder="Вставьте API secret"
              />
            </label>
            <div className="permission-grid">
              <label className="check-field">
                <input
                  type="checkbox"
                  checked={form.hasFuturesPermission}
                  onChange={(event) => setForm({ ...form, hasFuturesPermission: event.target.checked })}
                />
                Я включил Futures trading permission в Pionex
              </label>
              <label className="check-field">
                <input
                  type="checkbox"
                  checked={form.hasBotPermission}
                  onChange={(event) => setForm({ ...form, hasBotPermission: event.target.checked })}
                />
                Я включил Bot API permission в Pionex
              </label>
              <label className="check-field">
                <input
                  type="checkbox"
                  checked={form.verifyNow}
                  onChange={(event) => setForm({ ...form, verifyNow: event.target.checked })}
                />
                Сразу проверить read-доступ
              </label>
            </div>
            <div className="panel-actions">
              <span className="compact">
                Проверка выполняет только GET /uapi/v1/account/balances и не размещает ордера.
              </span>
              <button className="button primary" disabled={busy === 'create'} type="submit">
                {busy === 'create' ? 'Сохраняю…' : 'Зашифровать и сохранить'}
              </button>
            </div>
          </form>
        )}

        {accounts.length === 0 ? (
          <div className="empty-state">
            Pionex API-аккаунты ещё не добавлены. Без аккаунта REAL-режим Автопилота заблокирован.
          </div>
        ) : (
          <div className="account-grid">
            {accounts.map((account) => (
              <article className="account-card" key={account.id}>
                <div className="account-card-title">
                  <div>
                    <h3>{account.name}</h3>
                    <code>{account.keyFingerprint || 'fingerprint unavailable'}</code>
                  </div>
                  <span className={`badge ${account.isEnabled ? 'success' : 'danger'}`}>
                    {account.isEnabled ? 'ENABLED' : 'DISABLED'}
                  </span>
                </div>
                <div className="capability-row">
                  <Capability ok={account.hasReadPermission} label="READ VERIFIED" />
                  <Capability ok={account.hasFuturesPermission} label="FUTURES DECLARED" />
                  <Capability ok={account.hasBotPermission} label="BOT DECLARED" />
                </div>
                <div className="definition">
                  <span>Capability status</span>
                  <strong>{account.capabilityStatus}</strong>
                </div>
                <div className="definition">
                  <span>Последняя проверка</span>
                  <strong>{formatDate(account.lastVerifiedAt)}</strong>
                </div>
                {account.lastError && <p className="account-error">{account.lastError}</p>}
                {canManage && (
                  <div className="row-actions">
                    <button
                      className="button small primary"
                      type="button"
                      disabled={busy !== null}
                      onClick={() => void verify(account)}
                    >
                      Проверить
                    </button>
                    <button
                      className="button small secondary"
                      type="button"
                      disabled={busy !== null || (!account.hasReadPermission && !account.isEnabled)}
                      onClick={() => void toggle(account)}
                    >
                      {account.isEnabled ? 'Выключить' : 'Включить'}
                    </button>
                    <button
                      className="button small danger"
                      type="button"
                      disabled={busy !== null}
                      onClick={() => void remove(account)}
                    >
                      Удалить
                    </button>
                  </div>
                )}
              </article>
            ))}
          </div>
        )}
      </section>
    </div>
  );
}

function Capability({ ok, label }: { ok: boolean; label: string }) {
  return <span className={`badge ${ok ? 'success' : 'warning'}`}>{ok ? '✓' : '—'} {label}</span>;
}

function formatDate(value: string | null): string {
  return value ? new Date(value).toLocaleString('ru-RU') : 'не проверялся';
}
