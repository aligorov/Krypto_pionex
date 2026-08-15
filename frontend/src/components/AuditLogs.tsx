import { useCallback, useEffect, useState } from 'react';
import { api } from '../api';
import { describeError } from './AutoGridAutopilot';
import type { AuditEvent } from '../types';

export default function AuditLogs() {
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [action, setAction] = useState('');
  const [outcome, setOutcome] = useState('');

  const load = useCallback(async () => {
    const params = new URLSearchParams();
    if (action) params.set('action', action);
    if (outcome) params.set('outcome', outcome);
    params.set('limit', '200');
    try {
      setEvents(await api<AuditEvent[]>(`/api/audit?${params.toString()}`));
      setError(null);
    } catch (loadError) {
      setError(describeError(loadError));
    }
  }, [action, outcome]);

  useEffect(() => {
    void load();
  }, [load]);

  return (
    <div className="panel">
      <div className="panel-heading">
        <div>
          <span className="eyebrow">AUDIT TRAIL</span>
          <h3>Журнал действий</h3>
        </div>
        <div className="filter-bar" style={{ marginBottom: 0 }}>
          <input
            placeholder="Фильтр: действие (напр. autogrid)"
            value={action}
            onChange={(event) => setAction(event.target.value)}
          />
          <select value={outcome} onChange={(event) => setOutcome(event.target.value)}>
            <option value="">Все исходы</option>
            <option value="SUCCESS">SUCCESS</option>
            <option value="DENIED">DENIED</option>
            <option value="ERROR">ERROR</option>
          </select>
        </div>
      </div>
      {error && <div className="alert danger"><span>{error}</span></div>}
      {events.length === 0 ? (
        <div className="empty-state">Событий нет.</div>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Время</th>
                <th>Действие</th>
                <th>Актор</th>
                <th>Ресурс</th>
                <th>Исход</th>
                <th>IP</th>
                <th>Детали</th>
              </tr>
            </thead>
            <tbody>
              {events.map((event, index) => (
                <tr key={`${event.createdAt}-${index}`}>
                  <td>{new Date(event.createdAt).toLocaleString()}</td>
                  <td><strong>{event.action}</strong></td>
                  <td>
                    {event.actor}
                    <small>{event.actorType}</small>
                  </td>
                  <td>
                    {event.resourceType}
                    <small>{event.resourceId}</small>
                  </td>
                  <td>
                    <span
                      className={`badge ${
                        event.outcome === 'SUCCESS' ? 'success' : event.outcome === 'DENIED' ? 'warning' : 'danger'
                      }`}
                    >
                      {event.outcome}
                    </span>
                  </td>
                  <td><small>{event.ipAddress}</small></td>
                  <td>
                    <details>
                      <summary>детали</summary>
                      <pre>{JSON.stringify(event.details, null, 2)}</pre>
                    </details>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
