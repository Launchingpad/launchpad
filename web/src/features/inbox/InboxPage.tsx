import { useSearchParams } from 'react-router-dom';
import { flatRows, useInbox } from '../../api/notifications';
import { usePageTitle } from '../../lib/documentTitle';
import { FilterTabs, MarkAllRead } from './InboxControls';
import { InboxList } from './InboxList';
import { parseFilter } from './inboxModel';
import styles from './inbox.module.css';

/** `/:org/inbox`. The filter is in the address (`?filter=all`), so Back and a reload keep it. */
export function InboxPage() {
  const [params, setParams] = useSearchParams();
  const filter = parseFilter(params.get('filter'));
  // A place for the unread prefix of the tab title is `useDocumentTitle(parts, prefix)` (LAUN-30). Not built here.
  usePageTitle('Inbox');
  const query = useInbox(filter);
  return (
    <section className={styles.page} aria-labelledby="inbox-h">
      <header className={styles.head}>
        <h1 id="inbox-h" className={styles.h1}>
          Inbox
        </h1>
        <FilterTabs
          value={filter}
          onChange={(f) => setParams(f === 'unread' ? {} : { filter: f }, { replace: true })}
        />
        <span className={styles.grow} />
        {filter !== 'archived' && <MarkAllRead rows={flatRows(query.data)} />}
      </header>
      <InboxList query={query} filter={filter} keys="global" className={styles.pageList} />
      <p className={styles.hint}>
        <kbd>J</kbd> <kbd>K</kbd> move, <kbd>Enter</kbd> open, <kbd>E</kbd> archive, <kbd>U</kbd> read or
        unread.
      </p>
    </section>
  );
}
