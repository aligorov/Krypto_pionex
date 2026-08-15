import { FormEvent, useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { describeError } from './AutoGridAutopilot';
import type { APIToken } from '../types';

interface Props {
  canManage: boolean;
}

const ALL_SCOPES = ['mcp:read', 'mcp:operate', 'mcp:trade', 'mcp:admin'];

export default function MCPServer({ canManage }: Props) {
  const [tokens, setTokens] = useState<APIToken[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState('');
  const [scopes, setScopes] = useState<string[]>(['mcp:read']);
  const [secret, setSecret] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setTokens(await api<APIToken[]>('/api/mcp/tokens'));
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  async function create(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    setSecret(null);
    try {
      const result = await api<{ token: APIToken; secret: string }>('/api/mcp/tokens', {
        method: 'POST',
        body: JSON.stringify({ name, scopes }),
      });
      setSecret(result.secret);
      setName('');
      await load();
    } catch (createError) {
      setError(describeError(createError));
    } finally {
      setBusy(false);
    }
  }

  async function revoke(id: string) {
    if (!window.confirm('Отозвать токен? Клиенты с ним немедленно потеряют доступ.')) return;
    setBusy(true);
    try {
      await api(`/api/mcp/tokens/${id}`, { method: 'DELETE' });
      await load();
    } catch (revokeError) {
      setError(describeError(revokeError));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="section-stack">
      {error && <div className="alert danger"><span>{error}</span></div>}

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">MCP ENDPOINT</span>
            <h3>Streamable HTTP MCP</h3>
          </div>
        </div>
        <p className="muted">
          Подключите MCP-клиент к <code>/mcp</code> с заголовком{' '}
          <code>Authorization: Bearer &lt;token&gt;</code>. Токены хранятся в PostgreSQL как SHA-256
          хэши; полный доступ к командам — только через confirm-коды control plane.
        </p>
      </div>

      {secret && (
        <div className="alert success">
          <div>
            <strong>Токен создан — показывается один раз:</strong>
            <pre>{secret}</pre>
          </div>
          <button onClick={() => setSecret(null)}>×</button>
        </div>
      )}

      <div className="panel">
        <div className="panel-heading">
          <div>
            <span className="eyebrow">TOKENS</span>
            <h3>Токены ({tokens.length})</h3>
          </div>
        </div>

        {canManage && (
          <form className="inline-form" onSubmit={create} style={{ marginBottom: 18 }}>
            <input
              placeholder="Имя токена (напр. claude-local)"
              value={name}
              onChange={(event) => setName(event.target.value)}
              required
            />
            <select
              multiple
              value={scopes}
              onChange={(event) =>
                setScopes(Array.from(event.target.selectedOptions).map((option) => option.value))}
              style={{ minHeight: 39 }}
            >
              {ALL_SCOPES.map((scope) => (
                <option key={scope} value={scope}>{scope}</option>
              ))}
            </select>
            <button className="button primary" type="submit" disabled={busy || name.trim() === ''}>
              Создать токен
            </button>
          </form>
        )}

        {tokens.length === 0 ? (
          <div className="empty-state">Токенов нет.</div>
        ) : (
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>Имя</th>
                  <th>Префикс</th>
                  <th>Скоупы</th>
                  <th>Последнее использование</th>
                  <th>Истекает</th>
                  <th>Создан</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {tokens.map((token) => (
                  <tr key={token.id}>
                    <td><strong>{token.name}</strong></td>
                    <td><code>{token.prefix}…</code></td>
                    <td>
                      {token.scopes.map((scope) => (
                        <span key={scope} className="badge neutral" style={{ marginRight: 4 }}>{scope}</span>
                      ))}
                    </td>
                    <td>{token.lastUsedAt ? new Date(token.lastUsedAt).toLocaleString() : '—'}</td>
                    <td>{token.expiresAt ? new Date(token.expiresAt).toLocaleString() : 'никогда'}</td>
                    <td>{new Date(token.createdAt).toLocaleString()}</td>
                    <td>
                      <button className="button small danger" disabled={busy} onClick={() => void revoke(token.id)}>
                        Отозвать
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}
