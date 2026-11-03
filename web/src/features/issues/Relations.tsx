import { useRef, useState } from 'react';
import { Link } from 'react-router-dom';
import { useOrgSlug } from '../../api/org';
import { useCreateRelation, useDeleteRelation } from '../../api/queries';
import { errorMessage } from '../../api/errors';
import type { Issue, Relation, RelationType } from '../../api/types';
import { uuid } from '../../lib/uuid';
import { StatusIcon } from '../../ui/Icons';
import { IssueSearchPicker } from './IssueSearchPicker';
import ui from '../../styles/ui.module.css';
import styles from './issues.module.css';
import detail from './detail.module.css';

const GROUPS: { key: 'blocks' | 'blockedBy' | 'related' | 'duplicateOf' | 'duplicates'; label: string }[] = [
  { key: 'blocks', label: 'Blocks' },
  { key: 'blockedBy', label: 'Blocked by' },
  { key: 'related', label: 'Related' },
  { key: 'duplicateOf', label: 'Duplicate of' },
  { key: 'duplicates', label: 'Duplicated by' },
];
const ADDABLE: { value: RelationType; label: string }[] = [
  { value: 'blocks', label: 'Blocks' },
  { value: 'blockedBy', label: 'Blocked by' },
  { value: 'related', label: 'Related to' },
  { value: 'duplicateOf', label: 'Duplicate of' },
];

function Row({ rel, identifier }: { rel: Relation; identifier: string }) {
  const org = useOrgSlug();
  const del = useDeleteRelation(identifier);
  const [error, setError] = useState<string | null>(null);
  return (
    <li className={styles.relRow}>
      {rel.issue.stateType && <StatusIcon type={rel.issue.stateType} color="currentColor" />}
      <Link to={`/${org}/issue/${rel.issue.identifier}`} className={styles.relLink}>
        <span className={styles.ident}>{rel.issue.identifier}</span> {rel.issue.title}
      </Link>
      <button
        type="button"
        className={`${ui.btn} ${ui.ghost}`}
        aria-label={`Remove relation to ${rel.issue.identifier}`}
        disabled={del.isPending}
        onClick={() => del.mutate(rel.id, { onError: (e) => setError(errorMessage(e)) })}
      >
        ×
      </button>
      {error && (
        <span role="alert" className={ui.hint}>
          {error}
        </span>
      )}
    </li>
  );
}

export function RelationsSection({ issue }: { issue: Issue }) {
  const create = useCreateRelation(issue.identifier);
  const [type, setType] = useState<RelationType>('blocks');
  const [error, setError] = useState<string | null>(null);
  const key = useRef(uuid());
  const rel = issue.relations;
  const total = rel ? GROUPS.reduce((n, g) => n + rel[g.key].length, 0) : 0;
  return (
    <section aria-labelledby="rel-h" className={detail.propSection}>
      <h2 id="rel-h" className={detail.sectionTitle}>
        Related {total ? <span className={styles.count}>{total}</span> : null}
      </h2>
      {GROUPS.map((g) =>
        rel && rel[g.key].length ? (
          <div key={g.key}>
            <h3 className={detail.groupTitle}>{g.label}</h3>
            <ul className={styles.relList}>
              {rel[g.key].map((r) => (
                <Row key={r.id} rel={r} identifier={issue.identifier} />
              ))}
            </ul>
          </div>
        ) : null,
      )}
      <div className={detail.relAdd}>
        <label className="sr-only" htmlFor="rel-type">
          Relation type
        </label>
        <select
          id="rel-type"
          className={detail.relType}
          value={type}
          onChange={(e) => setType(e.target.value as RelationType)}
        >
          {ADDABLE.map((a) => (
            <option key={a.value} value={a.value}>
              {a.label}
            </option>
          ))}
        </select>
        <IssueSearchPicker
          ariaLabel="Issue to relate"
          triggerClassName={detail.ghostBtn}
          display="Add relation…"
          exclude={[issue.identifier]}
          onSelect={(other) => {
            if (!other) return;
            setError(null);
            create.mutate(
              { type, issue: other.identifier, idempotencyKey: key.current },
              { onSuccess: () => (key.current = uuid()), onError: (e) => setError(errorMessage(e)) },
            );
          }}
        />
      </div>
      {error && (
        <p role="alert" className={`${ui.banner} ${ui.errorBanner}`}>
          {error}
        </p>
      )}
    </section>
  );
}
