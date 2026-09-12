import { Component, type ErrorInfo, type ReactNode } from "react";

/**
 * The last line before a white screen.
 *
 * §12 asks for structure immediately and never a blank page. A render fault in
 * one panel — a field the API stopped sending, a shape that changed — would
 * otherwise unmount the whole interface and leave a cashier looking at nothing
 * with a queue in front of them, which is indistinguishable from the server
 * being down and leads to exactly the wrong response.
 *
 * So a fault is contained and *named*: what broke, and the fact that the API is
 * unaffected. Reloading is offered because a stale bundle against a newer API
 * is the most common cause.
 */
interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<{ children: ReactNode }, State> {
  override state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  override componentDidCatch(error: Error, info: ErrorInfo): void {
    // Logged rather than swallowed: the stack is what makes this diagnosable,
    // and the console is where whoever is helping will look.
    console.error("interface fault", error, info.componentStack);
  }

  override render(): ReactNode {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <main className="screen">
        <div className="refusal refusal--invariant" role="alert">
          <p className="refusal__title">عطل في الواجهة</p>
          <p className="refusal__body">
            توقّفت هذه الشاشة عن العرض. <b>لم يتأثر الخادم ولا أي عملية مالية</b> — ما لم
            يُرحَّل لم يُرحَّل، وما رُحِّل فهو محفوظ بوصله.
          </p>
          <p className="refusal__remedy">
            أعد التحميل أولاً: النسخة القديمة من الواجهة أمام خادم أحدث هي السبب الأكثر شيوعاً.
            إن تكرّر العطل، أبلغ الدعم بالنص أدناه.
          </p>
          <div className="refusal__actions">
            <button type="button" className="btn btn--primary" onClick={() => window.location.reload()}>
              إعادة التحميل
            </button>
            <button
              type="button"
              className="btn"
              onClick={() => {
                window.location.href = "/app/";
              }}
            >
              العودة إلى البداية
            </button>
          </div>
          <div className="refusal__meta">
            <code>{error.message}</code>
          </div>
        </div>
      </main>
    );
  }
}
