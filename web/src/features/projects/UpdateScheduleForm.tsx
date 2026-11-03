import { useState } from 'react';
import type { Project } from '../../api/types';
import { useOrgDetail } from '../../api/queries';
import { useUpdateProject } from './queries';
import ui from '../../styles/ui.module.css';
import styles from './projects.module.css';

type Frequency = Project['updateSchedule']['frequency'];

export const FREQUENCY_LABEL: Record<Frequency, string> = {
  none: 'No reminders',
  weekly: 'Every week',
  biweekly: 'Every two weeks',
  monthly: 'Every month (first weekday of the month)',
};
export const WEEKDAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];

/**
 * The update reminder schedule of a project (LAUN-196): how often, which weekday and which hour (in the org's time
 * zone). The lead gets a reminder when no update was posted since the last due time. It sends the three fields as
 * they stand in the boxes, without the project `version` (a schedule is its own field; the server merges it).
 */
export function UpdateScheduleForm({ project }: { project: Project }) {
  const save = useUpdateProject(project.id);
  const zone = useOrgDetail().data?.timeZone;
  const cur = project.updateSchedule;
  const [frequency, setFrequency] = useState<Frequency>(cur.frequency);
  const [weekday, setWeekday] = useState(cur.weekday);
  const [hour, setHour] = useState(cur.hour);
  const dirty = frequency !== cur.frequency || weekday !== cur.weekday || hour !== cur.hour;

  return (
    <form
      className={styles.scheduleForm}
      aria-label="Update reminders"
      onSubmit={(e) => {
        e.preventDefault();
        save.mutate({ updateSchedule: { frequency, weekday, hour } });
      }}
    >
      <div className={ui.field}>
        <label htmlFor="us-frequency">Remind the lead</label>
        <select
          id="us-frequency"
          className={ui.select}
          value={frequency}
          onChange={(e) => setFrequency(e.target.value as Frequency)}
        >
          {(Object.keys(FREQUENCY_LABEL) as Frequency[]).map((f) => (
            <option key={f} value={f}>
              {FREQUENCY_LABEL[f]}
            </option>
          ))}
        </select>
      </div>
      {frequency !== 'none' && (
        <>
          <div className={ui.field}>
            <label htmlFor="us-weekday">Day</label>
            <select
              id="us-weekday"
              className={ui.select}
              value={weekday}
              onChange={(e) => setWeekday(Number(e.target.value))}
            >
              {WEEKDAYS.map((d, i) => (
                <option key={d} value={i}>
                  {d}
                </option>
              ))}
            </select>
          </div>
          <div className={ui.field}>
            <label htmlFor="us-hour">
              Time ({zone ? `organisation time zone: ${zone}` : "in the organisation's time zone"})
            </label>
            <select
              id="us-hour"
              className={ui.select}
              value={hour}
              onChange={(e) => setHour(Number(e.target.value))}
            >
              {Array.from({ length: 24 }, (_, h) => (
                <option key={h} value={h}>
                  {String(h).padStart(2, '0')}:00
                </option>
              ))}
            </select>
          </div>
        </>
      )}
      {save.isError && <div role="alert">Could not save the reminder schedule.</div>}
      <button type="submit" className={`${ui.btn} ${ui.primary}`} disabled={!dirty || save.isPending}>
        Save reminders
      </button>
    </form>
  );
}
