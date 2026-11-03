import { useEffect, useRef, useState } from 'react';
import { Link, useParams } from 'react-router-dom';
import { unsubscribeEmail } from '../../api/queries';
import { ApiError } from '../../api/errors';
import { useAppTitle } from '../../lib/documentTitle';
import styles from './setup.module.css';
import ui from '../../styles/ui.module.css';

// LAUN-36. The public `/unsubscribe/<token>` page (decisions-T13 #25). The link in a notification email opens
// it. Opening it NEVER acts: mail scanners fetch links. Only the button sends the POST. The token is the
// credential and sits in the path; it is sent only in that POST and never shown, stored or put in the title.
// The server answers 204 for any well-formed token and says nothing about who has an account, so the page
// cannot show which events a token covers. No text from the server reaches the screen.

const TOKEN_RE = /^[A-Za-z0-9_-]{43}$/;

type Phase = 'ask' | 'busy' | 'done' | 'bad' | 'retry' | 'limited';

export function UnsubscribePage() {
  useAppTitle('Unsubscribe');
  const { token = '' } = useParams();
  const valid = TOKEN_RE.test(token);
  const [phase, setPhase] = useState<Phase>(valid ? 'ask' : 'bad');
  const heading = useRef<HTMLHeadingElement>(null);
  useEffect(() => heading.current?.focus(), [phase]);

  async function go() {
    setPhase('busy');
    try {
      await unsubscribeEmail(token);
      setPhase('done');
    } catch (e) {
      if (e instanceof ApiError && e.status === 400) setPhase('bad');
      else if (e instanceof ApiError && e.status === 429) setPhase('limited');
      else setPhase('retry');
    }
  }

  return (
    <main className={styles.page}>
      <div className={styles.card}>
        {(phase === 'ask' || phase === 'busy' || phase === 'retry' || phase === 'limited') && (
          <>
            <h1 className={styles.title} tabIndex={-1} ref={heading}>
              Turn off these emails?
            </h1>
            <p>
              This turns off email for the kind of notification that sent you this link. It changes nothing
              else: your inbox, your other emails and your account stay as they are.
            </p>
            {phase === 'retry' && (
              <p role="alert">Could not reach the server. Check your connection and try again.</p>
            )}
            {phase === 'limited' && <p role="alert">Too many requests. Wait a minute and try again.</p>}
            <div className={styles.actions}>
              <button
                type="button"
                className={`${ui.btn} ${ui.primary}`}
                disabled={phase === 'busy'}
                onClick={() => void go()}
              >
                {phase === 'busy' ? 'Turning off…' : 'Turn off these emails'}
              </button>
            </div>
          </>
        )}
        {phase === 'done' && (
          <>
            <h1 className={styles.title} tabIndex={-1} ref={heading}>
              Done
            </h1>
            <p role="status">
              If this link was valid, those emails are off now. You can change what you get at any time in
              Settings, Notifications.
            </p>
            <p>
              <Link to="/">Open Launchpad</Link>
            </p>
          </>
        )}
        {phase === 'bad' && (
          <>
            <h1 className={styles.title} tabIndex={-1} ref={heading}>
              This link does not work
            </h1>
            <p>
              The link may be incomplete or too old. You can choose what you get at any time in Settings,
              Notifications.
            </p>
            <p>
              <Link to="/">Open Launchpad</Link>
            </p>
          </>
        )}
      </div>
    </main>
  );
}
