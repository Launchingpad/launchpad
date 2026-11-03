import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import styles from '../styles/ui.module.css';

interface ToastItem {
  id: number;
  text: string;
  kind: 'info' | 'error';
  action?: ReactNode;
}
interface ToastApi {
  show: (text: string, opts?: { kind?: 'info' | 'error'; action?: ReactNode; ms?: number }) => void;
}
const Ctx = createContext<ToastApi>({ show: () => {} });
export const useToast = () => useContext(Ctx);

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const next = useRef(1);
  const show = useCallback<ToastApi['show']>((text, opts = {}) => {
    const id = next.current++;
    setItems((x) => [...x.slice(-3), { id, text, kind: opts.kind ?? 'info', action: opts.action }]);
    setTimeout(() => setItems((x) => x.filter((i) => i.id !== id)), opts.ms ?? 5000);
  }, []);
  const api = useMemo(() => ({ show }), [show]);
  return (
    <Ctx.Provider value={api}>
      {children}
      {/* In a portal on <body>, marked as a live region: a dialog makes #root inert, and a toast inside it
          would not be announced and its link could not be used. The dialog leaves this node alone. */}
      {createPortal(
        <div className={styles.toasts} aria-live="polite" aria-atomic="false" data-live-region>
          {items.map((t) => (
            <div
              key={t.id}
              className={`${styles.toast} ${t.kind === 'error' ? styles.toastError : ''}`}
              role={t.kind === 'error' ? 'alert' : 'status'}
            >
              <span>{t.text}</span>
              {t.action}
            </div>
          ))}
        </div>,
        document.body,
      )}
    </Ctx.Provider>
  );
}
