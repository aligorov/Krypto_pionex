import { Component, type ErrorInfo, type ReactNode } from 'react';

interface State {
  error: Error | null;
}

/* A malformed/partial API payload used to unmount the whole React tree —
   the operator saw a blank page with the error only in the console.
   This keeps the shell alive and shows the crash on screen instead. */
export default class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('UI crashed:', error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', padding: 24 }}>
          <div
            className="panel"
            style={{ maxWidth: 460, width: '100%', borderColor: 'rgba(239,68,68,.4)' }}
          >
            <h3 style={{ margin: '0 0 8px' }}>Интерфейс упал</h3>
            <p className="muted" style={{ wordBreak: 'break-word' }}>
              {this.state.error.message}
            </p>
            <button className="button primary" onClick={() => window.location.reload()}>
              Перезагрузить
            </button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
