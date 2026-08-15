import { FormEvent, useEffect, useState } from 'react';
import { api, ApiError } from '../api';
import type { IPBan, MyIPResponse, TOTPSetupResponse, User, WhitelistEntry } from '../types';
import { QRCodeSVG } from './QRCodeSVG';

interface Props {
  user: User;
  onClose: () => void;
  onUserUpdated: (updated: User) => void;
  onPasswordChanged: () => void;
  defaultTab?: 'password' | '2fa' | 'ip';
}

export default function SecurityModal({
  user,
  onClose,
  onUserUpdated,
  onPasswordChanged,
  defaultTab = 'password',
}: Props) {
  const [tab, setTab] = useState<'password' | '2fa' | 'ip'>(
    user.mustChangePassword ? 'password' : defaultTab
  );

  // Password state
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [showPassword, setShowPassword] = useState(false);
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState<string | null>(null);

  // 2FA state
  const [twoFactorEnabled, setTwoFactorEnabled] = useState(user.twoFactorEnabled);
  const [setupData, setSetupData] = useState<TOTPSetupResponse | null>(null);
  const [totpCode, setTotpCode] = useState('');
  const [disablePassword, setDisablePassword] = useState('');
  const [twoFactorBusy, setTwoFactorBusy] = useState(false);
  const [twoFactorError, setTwoFactorError] = useState<string | null>(null);
  const [twoFactorSuccess, setTwoFactorSuccess] = useState<string | null>(null);
  const [copiedKey, setCopiedKey] = useState(false);
  const [copiedCodes, setCopiedCodes] = useState(false);

  // IP Security & Fail2ban state
  const [myIP, setMyIP] = useState<MyIPResponse | null>(null);
  const [bans, setBans] = useState<IPBan[]>([]);
  const [whitelist, setWhitelist] = useState<WhitelistEntry[]>([]);
  const [newWhitelistIP, setNewWhitelistIP] = useState('');
  const [newWhitelistDesc, setNewWhitelistDesc] = useState('');
  const [ipBusy, setIpBusy] = useState(false);
  const [ipError, setIpError] = useState<string | null>(null);
  const [ipSuccess, setIpSuccess] = useState<string | null>(null);

  // Password validation checks
  const lenValid = newPassword.length >= 12 && newPassword.length <= 128;
  const letterValid = /[a-zA-Z]/.test(newPassword);
  const digitValid = /[0-9]/.test(newPassword);
  const symbolValid = /[^a-zA-Z0-9]/.test(newPassword);
  const matchValid = newPassword.length > 0 && newPassword === confirmPassword;
  const allPasswordValid = lenValid && letterValid && digitValid && symbolValid && matchValid;

  useEffect(() => {
    if (tab === 'ip' && user.role === 'ADMIN') {
      loadIPSecurity();
    }
  }, [tab, user.role]);

  async function loadIPSecurity() {
    setIpBusy(true);
    setIpError(null);
    try {
      const [ipRes, bansRes, wlRes] = await Promise.all([
        api<MyIPResponse>('/api/security/my-ip'),
        api<IPBan[]>('/api/security/bans'),
        api<WhitelistEntry[]>('/api/security/whitelist'),
      ]);
      setMyIP(ipRes);
      setBans(bansRes || []);
      setWhitelist(wlRes || []);
    } catch (err) {
      setIpError(err instanceof ApiError ? err.message : 'Не удалось загрузить данные IP-безопасности');
    } finally {
      setIpBusy(false);
    }
  }

  async function handlePasswordSubmit(e: FormEvent) {
    e.preventDefault();
    if (!allPasswordValid) return;
    setPasswordBusy(true);
    setPasswordError(null);
    try {
      await api('/api/me/password', {
        method: 'PUT',
        body: JSON.stringify({ password: newPassword }),
      });
      onPasswordChanged();
    } catch (err) {
      setPasswordError(err instanceof ApiError ? err.message : 'Не удалось изменить пароль');
      setPasswordBusy(false);
    }
  }

  async function start2FASetup() {
    setTwoFactorBusy(true);
    setTwoFactorError(null);
    setTwoFactorSuccess(null);
    try {
      const data = await api<TOTPSetupResponse>('/api/auth/2fa/setup');
      setSetupData(data);
      setTotpCode('');
    } catch (err) {
      setTwoFactorError(err instanceof ApiError ? err.message : 'Не удалось инициализировать 2FA');
    } finally {
      setTwoFactorBusy(false);
    }
  }

  async function confirm2FAEnable(e: FormEvent) {
    e.preventDefault();
    if (!setupData || totpCode.trim().length !== 6) return;
    setTwoFactorBusy(true);
    setTwoFactorError(null);
    try {
      await api('/api/auth/2fa/enable', {
        method: 'POST',
        body: JSON.stringify({
          secret: setupData.secret,
          code: totpCode.trim(),
          recoveryCodes: setupData.recoveryCodes,
        }),
      });
      setTwoFactorEnabled(true);
      setSetupData(null);
      setTwoFactorSuccess('Двухфакторная аутентификация (2FA) успешно подключена!');
      onUserUpdated({ ...user, twoFactorEnabled: true });
    } catch (err) {
      setTwoFactorError(err instanceof ApiError ? err.message : 'Неверный код подтверждения');
    } finally {
      setTwoFactorBusy(false);
    }
  }

  async function handle2FADisable(e: FormEvent) {
    e.preventDefault();
    if (!disablePassword) return;
    setTwoFactorBusy(true);
    setTwoFactorError(null);
    setTwoFactorSuccess(null);
    try {
      await api('/api/auth/2fa/disable', {
        method: 'POST',
        body: JSON.stringify({ password: disablePassword }),
      });
      setTwoFactorEnabled(false);
      setDisablePassword('');
      setTwoFactorSuccess('2FA отключена. Теперь вход возможен только по паролю.');
      onUserUpdated({ ...user, twoFactorEnabled: false });
    } catch (err) {
      setTwoFactorError(err instanceof ApiError ? err.message : 'Неверный текущий пароль');
    } finally {
      setTwoFactorBusy(false);
    }
  }

  async function handleUnban(ip: string) {
    setIpBusy(true);
    setIpError(null);
    try {
      await api(`/api/security/bans/${encodeURIComponent(ip)}`, { method: 'DELETE' });
      setIpSuccess(`IP ${ip} успешно разблокирован`);
      await loadIPSecurity();
    } catch (err) {
      setIpError(err instanceof ApiError ? err.message : 'Не удалось разблокировать IP');
    } finally {
      setIpBusy(false);
    }
  }

  async function handleAddWhitelist(e: FormEvent) {
    e.preventDefault();
    if (!newWhitelistIP.trim()) return;
    setIpBusy(true);
    setIpError(null);
    setIpSuccess(null);
    try {
      await api('/api/security/whitelist', {
        method: 'POST',
        body: JSON.stringify({
          ipOrCidr: newWhitelistIP.trim(),
          description: newWhitelistDesc.trim(),
        }),
      });
      setIpSuccess(`Адрес ${newWhitelistIP} добавлен в белый список`);
      setNewWhitelistIP('');
      setNewWhitelistDesc('');
      await loadIPSecurity();
    } catch (err) {
      setIpError(err instanceof ApiError ? err.message : 'Не удалось добавить IP в белый список');
    } finally {
      setIpBusy(false);
    }
  }

  async function handleRemoveWhitelist(id: number, ipOrCidr: string) {
    if (!confirm(`Удалить ${ipOrCidr} из белого списка?`)) return;
    setIpBusy(true);
    setIpError(null);
    try {
      await api(`/api/security/whitelist/${id}`, { method: 'DELETE' });
      setIpSuccess(`Запись ${ipOrCidr} удалена из белого списка`);
      await loadIPSecurity();
    } catch (err) {
      setIpError(err instanceof ApiError ? err.message : 'Не удалось удалить запись');
    } finally {
      setIpBusy(false);
    }
  }

  async function handleWhitelistMyIP() {
    if (!myIP?.ip) return;
    setNewWhitelistIP(myIP.ip);
    setNewWhitelistDesc('Текущий IP администратора');
  }

  function copyText(text: string, isCodes = false) {
    navigator.clipboard.writeText(text);
    if (isCodes) {
      setCopiedCodes(true);
      setTimeout(() => setCopiedCodes(false), 2500);
    } else {
      setCopiedKey(true);
      setTimeout(() => setCopiedKey(false), 2500);
    }
  }

  function downloadRecoveryCodes(codes: string[]) {
    const text = `PIONEX CONTROL — РЕЗЕРВНЫЕ КОДЫ 2FA\nАккаунт: ${user.username}\nДата: ${new Date().toLocaleString()}\n\nСохраните эти одноразовые коды в надежном месте:\n\n` +
      codes.map((c, i) => `${i + 1}. ${c}`).join('\n') +
      `\n\nКаждый код можно использовать ровно один раз для входа при утере смартфона.`;
    const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = `pionex_2fa_recovery_${user.username}.txt`;
    a.click();
    URL.revokeObjectURL(url);
  }

  return (
    <div className="modal-backdrop" onClick={user.mustChangePassword ? undefined : onClose}>
      <div className="modal-dialog security-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <div>
            <span className="eyebrow">SECURITY SETTINGS</span>
            <h2>Безопасность аккаунта</h2>
          </div>
          {!user.mustChangePassword && (
            <button className="modal-close-btn" onClick={onClose} title="Закрыть">
              ×
            </button>
          )}
        </div>

        {user.mustChangePassword && (
          <div className="alert warning" style={{ margin: '16px 24px 0' }}>
            ⚠️ Для продолжения работы требуется установить новый безопасный пароль.
          </div>
        )}

        {!user.mustChangePassword && (
          <div className="modal-tabs">
            <button
              className={`modal-tab ${tab === 'password' ? 'active' : ''}`}
              onClick={() => setTab('password')}
            >
              🔑 Смена пароля
            </button>
            <button
              className={`modal-tab ${tab === '2fa' ? 'active' : ''}`}
              onClick={() => setTab('2fa')}
            >
              🛡️ 2FA (Authenticator)
              {twoFactorEnabled ? (
                <span className="badge success" style={{ marginLeft: 8 }}>ВКЛ</span>
              ) : (
                <span className="badge" style={{ marginLeft: 8, opacity: 0.6 }}>ВЫКЛ</span>
              )}
            </button>
            {user.role === 'ADMIN' && (
              <button
                className={`modal-tab ${tab === 'ip' ? 'active' : ''}`}
                onClick={() => setTab('ip')}
              >
                🚫 IP-защита & Whitelist
                {bans.length > 0 && (
                  <span className="badge badge-danger" style={{ marginLeft: 8 }}>
                    {bans.length} бан
                  </span>
                )}
              </button>
            )}
          </div>
        )}

        <div className="modal-body">
          {tab === 'password' && (
            <form onSubmit={handlePasswordSubmit} className="form-stack">
              {passwordError && <div className="alert danger">{passwordError}</div>}

              <label>
                Новый пароль
                <div className="password-input-wrapper">
                  <input
                    type={showPassword ? 'text' : 'password'}
                    value={newPassword}
                    onChange={(e) => setNewPassword(e.target.value)}
                    placeholder="Введите минимум 12 символов"
                    autoComplete="new-password"
                    required
                  />
                  <button
                    type="button"
                    className="password-toggle-btn"
                    onClick={() => setShowPassword(!showPassword)}
                    tabIndex={-1}
                  >
                    {showPassword ? 'Скрыть' : 'Показать'}
                  </button>
                </div>
              </label>

              <label>
                Подтверждение пароля
                <input
                  type={showPassword ? 'text' : 'password'}
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  placeholder="Повторите новый пароль"
                  autoComplete="new-password"
                  required
                />
              </label>

              <div className="password-requirements">
                <span className="compact" style={{ fontWeight: 700, marginBottom: 4, display: 'block' }}>
                  Требования к паролю:
                </span>
                <div className="requirement-grid">
                  <div className={`req-item ${lenValid ? 'valid' : ''}`}>
                    <span className="req-icon">{lenValid ? '✓' : '•'}</span> От 12 до 128 символов
                  </div>
                  <div className={`req-item ${letterValid ? 'valid' : ''}`}>
                    <span className="req-icon">{letterValid ? '✓' : '•'}</span> Латинские буквы (a-z, A-Z)
                  </div>
                  <div className={`req-item ${digitValid ? 'valid' : ''}`}>
                    <span className="req-icon">{digitValid ? '✓' : '•'}</span> Цифры (0-9)
                  </div>
                  <div className={`req-item ${symbolValid ? 'valid' : ''}`}>
                    <span className="req-icon">{symbolValid ? '✓' : '•'}</span> Спецсимволы (!@#$%^&*...)
                  </div>
                  <div className={`req-item ${matchValid ? 'valid' : ''}`}>
                    <span className="req-icon">{matchValid ? '✓' : '•'}</span> Пароли совпадают
                  </div>
                </div>
              </div>

              <div className="modal-actions">
                {!user.mustChangePassword && (
                  <button type="button" className="button secondary" onClick={onClose}>
                    Отмена
                  </button>
                )}
                <button
                  type="submit"
                  className="button primary"
                  disabled={!allPasswordValid || passwordBusy}
                >
                  {passwordBusy ? 'Сохранение…' : 'Обновить пароль'}
                </button>
              </div>
            </form>
          )}

          {tab === '2fa' && (
            <div className="two-factor-content">
              {twoFactorError && <div className="alert danger">{twoFactorError}</div>}
              {twoFactorSuccess && <div className="alert success">{twoFactorSuccess}</div>}

              {twoFactorEnabled && !setupData && (
                <div className="two-factor-active-card">
                  <div className="status-banner active">
                    <span className="status-icon">🛡️</span>
                    <div>
                      <strong>Двухфакторная защита активна</strong>
                      <p className="muted" style={{ margin: '4px 0 0', fontSize: '0.85rem' }}>
                        При каждом входе в систему требуется ввод 6-значного кода из Google Authenticator / 1Password.
                      </p>
                    </div>
                  </div>

                  <div className="disable-2fa-section">
                    <h4>Отключение 2FA</h4>
                    <p className="muted" style={{ fontSize: '0.85rem' }}>
                      Для отключения двухфакторной защиты введите текущий пароль от аккаунта:
                    </p>
                    <form onSubmit={handle2FADisable} className="inline-form" style={{ marginTop: 12 }}>
                      <input
                        type="password"
                        placeholder="Текущий пароль"
                        value={disablePassword}
                        onChange={(e) => setDisablePassword(e.target.value)}
                        required
                        style={{ flex: 1 }}
                      />
                      <button
                        type="submit"
                        className="button danger"
                        disabled={!disablePassword || twoFactorBusy}
                      >
                        {twoFactorBusy ? 'Отключение…' : 'Отключить 2FA'}
                      </button>
                    </form>
                  </div>
                </div>
              )}

              {!twoFactorEnabled && !setupData && (
                <div className="two-factor-inactive-card">
                  <div className="status-banner inactive">
                    <span className="status-icon">⚠️</span>
                    <div>
                      <strong>2FA не подключена</strong>
                      <p className="muted" style={{ margin: '4px 0 0', fontSize: '0.85rem' }}>
                        Защитите ваш торговый аккаунт вторым фактором через Google Authenticator, Яндекс Ключ или 1Password.
                      </p>
                    </div>
                  </div>

                  <button
                    type="button"
                    className="button primary"
                    onClick={start2FASetup}
                    disabled={twoFactorBusy}
                    style={{ marginTop: 16 }}
                  >
                    {twoFactorBusy ? 'Генерация ключа…' : '⚡ Подключить 2FA'}
                  </button>
                </div>
              )}

              {setupData && (
                <form onSubmit={confirm2FAEnable} className="two-factor-wizard">
                  <div className="wizard-step">
                    <span className="step-num">1</span>
                    <div className="step-body">
                      <strong>Отсканируйте QR-код приложением аутентификации</strong>
                      <p className="muted" style={{ fontSize: '0.85rem', margin: '4px 0 12px' }}>
                        Откройте Google Authenticator, Apple Passwords или 1Password и отсканируйте QR-код:
                      </p>

                      <div className="qr-container">
                        <QRCodeSVG value={setupData.otpauthUrl} size={160} />
                        <div className="qr-text-info">
                          <span className="muted" style={{ fontSize: '0.75rem' }}>Или введите ключ вручную:</span>
                          <div className="secret-key-box">
                            <code>{setupData.secret}</code>
                            <button
                              type="button"
                              className="button small secondary"
                              onClick={() => copyText(setupData.secret)}
                            >
                              {copiedKey ? '✓ Скопировано' : 'Копировать'}
                            </button>
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>

                  <div className="wizard-step">
                    <span className="step-num">2</span>
                    <div className="step-body">
                      <strong>Сохраните резервные коды восстановления</strong>
                      <p className="muted" style={{ fontSize: '0.85rem', margin: '4px 0 10px' }}>
                        Если вы потеряете доступ к телефону, каждый код позволит войти 1 раз:
                      </p>

                      <div className="recovery-codes-grid">
                        {setupData.recoveryCodes.map((code, idx) => (
                          <span key={idx} className="recovery-code-chip">
                            {code}
                          </span>
                        ))}
                      </div>

                      <div style={{ display: 'flex', gap: '8px', marginTop: 10 }}>
                        <button
                          type="button"
                          className="button small secondary"
                          onClick={() => copyText(setupData.recoveryCodes.join('\n'), true)}
                        >
                          {copiedCodes ? '✓ Коды скопированы' : '📋 Скопировать коды'}
                        </button>
                        <button
                          type="button"
                          className="button small secondary"
                          onClick={() => downloadRecoveryCodes(setupData.recoveryCodes)}
                        >
                          💾 Скачать .txt
                        </button>
                      </div>
                    </div>
                  </div>

                  <div className="wizard-step">
                    <span className="step-num">3</span>
                    <div className="step-body">
                      <strong>Подтвердите активацию кодом из приложения</strong>
                      <p className="muted" style={{ fontSize: '0.85rem', margin: '4px 0 10px' }}>
                        Введите текущий 6-значный код из вашего приложения аутентификации:
                      </p>

                      <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
                        <input
                          type="text"
                          maxLength={6}
                          placeholder="000000"
                          value={totpCode}
                          onChange={(e) => setTotpCode(e.target.value.replace(/\D/g, ''))}
                          style={{
                            fontSize: '1.4rem',
                            letterSpacing: '0.3em',
                            textAlign: 'center',
                            width: '180px',
                            fontWeight: 700,
                          }}
                          autoFocus
                          required
                        />
                        <button
                          type="submit"
                          className="button primary"
                          disabled={totpCode.trim().length !== 6 || twoFactorBusy}
                        >
                          {twoFactorBusy ? 'Проверка…' : 'Подтвердить и включить'}
                        </button>
                        <button
                          type="button"
                          className="button secondary"
                          onClick={() => setSetupData(null)}
                        >
                          Отмена
                        </button>
                      </div>
                    </div>
                  </div>
                </form>
              )}
            </div>
          )}

          {tab === 'ip' && user.role === 'ADMIN' && (
            <div className="ip-security-content">
              {ipError && <div className="alert danger">{ipError}</div>}
              {ipSuccess && <div className="alert success">{ipSuccess}</div>}

              {/* Current Admin IP Banner */}
              <div className="status-banner" style={{ background: 'var(--surface-soft)', display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                <div style={{ display: 'flex', gap: 12, alignItems: 'center' }}>
                  <span style={{ fontSize: '24px' }}>🌐</span>
                  <div>
                    <div style={{ fontSize: '0.85rem', color: 'var(--muted)' }}>Ваш текущий IP-адрес:</div>
                    <strong style={{ fontSize: '1.1rem', letterSpacing: '0.05em' }}>{myIP?.ip || 'Определение…'}</strong>
                    {myIP?.whitelisted ? (
                      <span className="badge success" style={{ marginLeft: 10 }}>✓ В белом списке</span>
                    ) : (
                      <span className="badge" style={{ marginLeft: 10, opacity: 0.8 }}>Защищен лимитами</span>
                    )}
                  </div>
                </div>
                {myIP && !myIP.whitelisted && (
                  <button
                    type="button"
                    className="button small primary"
                    onClick={handleWhitelistMyIP}
                  >
                    + Добавить мой IP в Whitelist
                  </button>
                )}
              </div>

              {/* Active Bans Section */}
              <div className="security-sub-section">
                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 8 }}>
                  <h3 style={{ margin: 0, fontSize: '15px' }}>🚫 Активные блокировки Fail2ban ({bans.length})</h3>
                  <button type="button" className="button small secondary" onClick={loadIPSecurity} disabled={ipBusy}>
                    {ipBusy ? 'Обновление…' : '↻ Обновить'}
                  </button>
                </div>
                <p className="muted" style={{ fontSize: '0.8rem', margin: '0 0 10px' }}>
                  IP-адреса, превысившие 5 неудачных попыток входа за 10 минут (бан на 15 минут).
                </p>

                {bans.length === 0 ? (
                  <div className="empty-state-box">
                    <span>🛡️ Нет заблокированных IP-адресов. Все входящие запросы в норме.</span>
                  </div>
                ) : (
                  <div className="table-wrapper">
                    <table className="table">
                      <thead>
                        <tr>
                          <th>IP адрес</th>
                          <th>Попыток</th>
                          <th>Последняя попытка</th>
                          <th>Заблокирован до</th>
                          <th>Действие</th>
                        </tr>
                      </thead>
                      <tbody>
                        {bans.map((b) => (
                          <tr key={b.ip}>
                            <td><code>{b.ip}</code></td>
                            <td><span className="badge badge-danger">{b.failedAttempts}</span></td>
                            <td className="muted" style={{ fontSize: '0.8rem' }}>
                              {new Date(b.lastFailedAt).toLocaleTimeString()}
                            </td>
                            <td>
                              <strong style={{ color: 'var(--danger)' }}>
                                {b.bannedUntil ? new Date(b.bannedUntil).toLocaleTimeString() : '—'}
                              </strong>
                            </td>
                            <td>
                              <button
                                type="button"
                                className="button small"
                                onClick={() => handleUnban(b.ip)}
                                disabled={ipBusy}
                              >
                                Разблокировать
                              </button>
                            </td>
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </div>

              {/* Whitelist Section */}
              <div className="security-sub-section" style={{ marginTop: 24, paddingTop: 18, borderTop: '1px solid var(--border)' }}>
                <h3 style={{ margin: '0 0 4px', fontSize: '15px' }}>🛡️ Белый список IP / Подсетей (Whitelist)</h3>
                <p className="muted" style={{ fontSize: '0.8rem', margin: '0 0 12px' }}>
                  Адреса и диапазоны CIDR из белого списка никогда не блокируются и не получают задержек при входе.
                </p>

                <form onSubmit={handleAddWhitelist} className="whitelist-add-form">
                  <input
                    type="text"
                    placeholder="IP или CIDR (напр. 198.51.100.42 или 192.168.1.0/24)"
                    value={newWhitelistIP}
                    onChange={(e) => setNewWhitelistIP(e.target.value)}
                    required
                    style={{ flex: 2 }}
                  />
                  <input
                    type="text"
                    placeholder="Описание (напр. Домашний VPN)"
                    value={newWhitelistDesc}
                    onChange={(e) => setNewWhitelistDesc(e.target.value)}
                    style={{ flex: 2 }}
                  />
                  <button type="submit" className="button primary" disabled={ipBusy || !newWhitelistIP.trim()}>
                    + Добавить
                  </button>
                </form>

                <div className="table-wrapper" style={{ marginTop: 12 }}>
                  <table className="table">
                    <thead>
                      <tr>
                        <th>IP / CIDR</th>
                        <th>Описание</th>
                        <th>Кем добавлен</th>
                        <th>Дата</th>
                        <th></th>
                      </tr>
                    </thead>
                    <tbody>
                      {whitelist.map((w) => {
                        const isLoopback = w.ipOrCidr === '127.0.0.1/32' || w.ipOrCidr === '::1/128';
                        return (
                          <tr key={w.id}>
                            <td><code>{w.ipOrCidr}</code></td>
                            <td>{w.description || <span className="muted">—</span>}</td>
                            <td className="muted" style={{ fontSize: '0.8rem' }}>{w.createdBy}</td>
                            <td className="muted" style={{ fontSize: '0.8rem' }}>
                              {new Date(w.createdAt).toLocaleDateString()}
                            </td>
                            <td style={{ textAlign: 'right' }}>
                              {!isLoopback ? (
                                <button
                                  type="button"
                                  className="button small danger"
                                  onClick={() => handleRemoveWhitelist(w.id, w.ipOrCidr)}
                                  disabled={ipBusy}
                                  title="Удалить из белого списка"
                                >
                                  ✕
                                </button>
                              ) : (
                                <span className="muted" style={{ fontSize: '0.75rem' }}>системный</span>
                              )}
                            </td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
