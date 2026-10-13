import { Component, type ErrorInfo, type ReactNode } from 'react';

/**
 * Exhibition safety net: a rendering bug in one region must not blank the whole
 * screen. The region shows a calm fallback and retries on the next data update.
 */
interface Props {
  name: string;
  /** Changes on every snapshot; a new value retries rendering after an error. */
  resetKey: unknown;
  children: ReactNode;
}

interface State {
  error: Error | null;
  failedAt: unknown;
}

const RETRY_AFTER_MS = 3000;

export class RegionBoundary extends Component<Props, State> {
  state: State = { error: null, failedAt: null };
  private erroredAt = 0;

  static getDerivedStateFromError(error: Error): Partial<State> {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    this.erroredAt = Date.now();
    this.setState({ failedAt: this.props.resetKey });
    console.error(`[OpsLoop dashboard] region "${this.props.name}" failed`, error, info.componentStack);
  }

  componentDidUpdate(prev: Props): void {
    if (
      this.state.error &&
      prev.resetKey !== this.props.resetKey &&
      this.props.resetKey !== this.state.failedAt &&
      Date.now() - this.erroredAt > RETRY_AFTER_MS
    ) {
      this.setState({ error: null, failedAt: null });
    }
  }

  render(): ReactNode {
    if (this.state.error) {
      return (
        <div role="status" style={{ padding: 'var(--space-4)', color: 'var(--c-ink-2)' }}>
          {this.props.name} 영역을 다시 그리는 중입니다.
        </div>
      );
    }
    return this.props.children;
  }
}
