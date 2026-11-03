import { useEffect, useRef, useState } from 'react';
import type { Issue, Label } from '../../api/types';
import { IconPlus } from '../../ui/Icons';
import { ChipPicker } from '../../ui/ChipPicker';
import { LabelPill } from '../../ui/LabelPill';
import { LabelList } from './LabelList';
import pill from '../../ui/labelPill.module.css';

/**
 * The Labels section of the issue detail page, as in Linear (LAUN-46): the pills in one wrapping row and
 * a quiet "+" at the end of it. "+" or a press on a pill opens the label picker (search, checkmarks,
 * create on the fly). Each pill has an "x" (hover or focus) to remove it.
 */
export function LabelsField({
  issue,
  all,
  onAdd,
  onRemove,
  onCreate,
  headingId,
  openRequest = 0,
}: {
  issue: Issue;
  all: Label[];
  onAdd: (l: Label) => void;
  onRemove: (l: Label) => void;
  /** Makes a label of the issue's team (decisions-T21 #7). */
  onCreate: (name: string) => void;
  headingId: string;
  /** Each increase opens the picker (the `L` key of the issue page). */
  openRequest?: number;
}) {
  const [open, setOpen] = useState(false);
  const list = useRef<HTMLUListElement>(null);
  // The pill that was removed: where focus goes once its <li> is gone (next "x", else the previous, else "+").
  const focusAfter = useRef<number | null>(null);
  // A press outside the panel closes it on mousedown; the click that follows on a pill must not reopen it.
  const closedAt = useRef(0);
  const setOpenTracked = (o: boolean) => {
    if (!o) closedAt.current = Date.now();
    setOpen(o);
  };
  const pressPill = () => {
    if (Date.now() - closedAt.current < 400) return;
    setOpenTracked(!open);
  };
  // Only a CHANGE of the request opens the picker: a field that mounts with an old count stays shut.
  const seenRequest = useRef(openRequest);
  useEffect(() => {
    if (openRequest === seenRequest.current) return;
    seenRequest.current = openRequest;
    setOpen(true);
  }, [openRequest]);
  useEffect(() => {
    const i = focusAfter.current;
    if (i === null) return;
    focusAfter.current = null;
    const xs = list.current?.querySelectorAll<HTMLElement>('button[data-remove]') ?? [];
    const target = xs[Math.min(i, xs.length - 1)];
    if (target) target.focus();
    else list.current?.querySelector<HTMLElement>('li:last-child button')?.focus();
  }, [issue.labels]);
  const none = issue.labels.length === 0;
  return (
    <ul ref={list} className={pill.row} aria-labelledby={headingId}>
      {issue.labels.map((l, i) => (
        <li key={l.id}>
          <LabelPill
            label={l}
            onClick={pressPill}
            onRemove={() => {
              focusAfter.current = i;
              onRemove(l as Label);
            }}
          />
        </li>
      ))}
      <li>
        <ChipPicker
          label="Labels"
          triggerName="Add label"
          triggerClassName={`${pill.add} ${none ? pill.addText : ''}`}
          display={
            <>
              <IconPlus width={12} height={12} />
              {none && <span>Add label</span>}
            </>
          }
          multi
          placeholder="Add labels…"
          open={open}
          onOpenChange={setOpenTracked}
          shortcut="L"
          options={[]}
          renderList={(list) => (
            <LabelList
              list={list}
              labels={all}
              idOf={(l) => l.id}
              issueId={issue.id}
              projectId={issue.projectId}
              parent={issue.parent?.identifier}
            />
          )}
          selected={issue.labels.map((l) => l.id)}
          onSelect={(o) => {
            const l = all.find((x) => x.id === o.id);
            if (!l) return;
            if (issue.labels.some((x) => x.id === l.id)) onRemove(l);
            else onAdd(l);
          }}
          onCreate={onCreate}
        />
      </li>
    </ul>
  );
}
