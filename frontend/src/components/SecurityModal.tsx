import { FormEvent, useState } from 'react';
import { api, ApiError } from '../api';
import type { TOTPSetupResponse, User } from '../types';
import { QRCodeSVG } from './QRCodeSVG';

interface Props {
  user: User;
  onClose: () => void;
  onUserUpdated: (updated: User) => void;
  onPasswordChanged: () => void;
  defaultTab?: 'password' | '2fa';
}

export default function SecurityModal({
  user,
  onClose,
  onUserUpdated,
  onPasswordChanged,
  defaultTab = 'password',
}: Props) {
  const [tab, setTab] = useState<'password' | '2fa'>(
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

  // Password validation checks
  const lenValid = newPassword.length >= 12 && newPassword.length <= 128;
  const letterValid = /[a-zA-Z]/.test(newPassword);
  const digitValid = /[0-9]/.test(newPassword);
  const symbolValid = /[^a-zA-Z0-9]/.test(newPassword);
  const matchValid = newPassword.length > 0 && newPassword === confirmPassword;
  const allPasswordValid = lenValid && letterValid && digitValid && symbolValid && matchValid;

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
              🛡️ 2FA (Google Authenticator)
              {twoFactorEnabled ? (
                <span className="badge success" style={{ marginLeft: 8 }}>ВКЛ</span>
              ) : (
                <span className="badge" style={{ marginLeft: 8, opacity: 0.6 }}>ВЫКЛ</span>
              )}
            </button>
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
        </div>
      </div>
    </div>
  );
}
