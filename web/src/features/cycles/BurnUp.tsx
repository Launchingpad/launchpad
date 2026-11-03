import { useEffect, useId, useState } from 'react';
import type { Cycle } from '../../api/types';
import { burnModel, burnSummary, dayLabel, yTicks, type BurnDay } from './burnModel';
import type { CycleSnapshot } from './queries';
import styles from './cycles.module.css';

const H = 180;
/** LAUN-164: one remembered choice for the chart on every cycle page, in this browser only. */
export const CHART_KEY = 'launchpad.cycle.chart.v1';

function readShown(): boolean {
  try {
    return localStorage.getItem(CHART_KEY) !== 'hidden';
  } catch {
    return true; // storage blocked: shown
  }
}

const PAD = { l: 34, r: 20, t: 14, b: 22 };

function path(pts: { x: number; y: number }[]): string {
  return pts.map((p, i) => `${i ? 'L' : 'M'}${p.x.toFixed(1)} ${p.y.toFixed(1)}`).join(' ');
}

/** The burn-up chart: the scope line and the done line, one point per day with a row. Inline SVG. */
export function BurnUp({
  cycle,
  rows,
  zone,
  now,
  upcoming = false,
}: {
  cycle: Pick<Cycle, 'startsAt' | 'endsAt'>;
  rows: CycleSnapshot[] | undefined;
  zone: string | undefined;
  now: number;
  upcoming?: boolean;
}) {
  // A state-held element (callback ref): the box exists only once there is data, so the observer must follow it.
  const [box, setBox] = useState<HTMLDivElement | null>(null);
  const [w, setW] = useState(640);
  const [hover, setHover] = useState<number | null>(null);
  const [shown, setShown] = useState(readShown);
  const regionId = useId();
  const toggle = () => {
    const next = !shown;
    setShown(next);
    setHover(null);
    try {
      localStorage.setItem(CHART_KEY, next ? 'shown' : 'hidden');
    } catch {
      /* storage blocked: the choice lasts until the page closes */
    }
  };
  useEffect(() => {
    if (!box || typeof ResizeObserver === 'undefined') return;
    const measure = () => setW(Math.max(200, Math.round(box.clientWidth)));
    const ro = new ResizeObserver(measure);
    ro.observe(box);
    measure();
    return () => ro.disconnect();
  }, [box]);

  const m = rows ? burnModel(cycle, rows, zone, now) : null;
  if (!m) {
    return (
      <section className={styles.burn} aria-labelledby="burn-h">
        <h2 id="burn-h" className={styles.burnH}>
          Burn-up
        </h2>
        <p className={styles.burnEmpty}>
          {upcoming
            ? 'The burn-up chart starts when the cycle starts.'
            : 'No burn-up data yet. The cycle job writes one row each day.'}
        </p>
      </section>
    );
  }

  const ticks = yTicks(m.max);
  const top = ticks[ticks.length - 1]!;
  const n = m.days.length;
  const iw = w - PAD.l - PAD.r;
  const ih = H - PAD.t - PAD.b;
  const x = (i: number) => PAD.l + (n > 1 ? (i / (n - 1)) * iw : iw / 2);
  const y = (v: number) => PAD.t + ih - (v / top) * ih;
  const have = m.days.filter(
    (d): d is BurnDay & { scope: number; done: number } => d.scope !== null && d.done !== null,
  );
  const scopePts = have.map((d) => ({ x: x(d.index), y: y(d.scope) }));
  const donePts = have.map((d) => ({ x: x(d.index), y: y(d.done) }));
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(iw / 64))));
  const hv = hover !== null ? m.days[hover] : undefined;

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    const i = Math.round(((e.clientX - r.left - PAD.l) / Math.max(1, iw)) * (n - 1));
    setHover(Math.min(n - 1, Math.max(0, i)));
  };

  return (
    <section className={styles.burn} aria-labelledby="burn-h">
      <div className={styles.burnTop}>
        <h2 id="burn-h" className={styles.burnH}>
          Burn-up
        </h2>
        {shown && (
          <ul className={styles.legend} aria-hidden="true">
            <li>
              <i className={styles.keyScope} /> Scope
            </li>
            <li>
              <i className={styles.keyDone} /> Done
            </li>
          </ul>
        )}
        <button
          type="button"
          className={styles.burnToggle}
          aria-expanded={shown}
          aria-controls={regionId}
          onClick={toggle}
        >
          <i className={shown ? styles.chevOpen : styles.chevShut} aria-hidden="true" />
          {shown ? 'Hide chart' : 'Show chart'}
        </button>
      </div>
      <div id={regionId} hidden={!shown}>
        {shown && (
          <>
            <div className={styles.burnBox} ref={setBox}>
              <svg
                width={w}
                height={H}
                role="img"
                aria-label={burnSummary(m)}
                className={styles.burnSvg}
                onPointerMove={onMove}
                onPointerDown={onMove}
                onPointerLeave={() => setHover(null)}
              >
                {ticks.map((t) => (
                  <g key={t}>
                    <line x1={PAD.l} x2={w - PAD.r} y1={y(t)} y2={y(t)} className={styles.grid} />
                    <text x={PAD.l - 6} y={y(t)} dy="0.32em" textAnchor="end" className={styles.axis}>
                      {t}
                    </text>
                  </g>
                ))}
                {m.days.map((d) =>
                  d.index % every === 0 ? (
                    <text key={d.time} x={x(d.index)} y={H - 6} textAnchor="middle" className={styles.axis}>
                      {dayLabel(d.time)}
                    </text>
                  ) : null,
                )}
                {m.today !== null && (
                  <g data-testid="burn-today">
                    <line
                      x1={x(m.today)}
                      x2={x(m.today)}
                      y1={PAD.t}
                      y2={PAD.t + ih}
                      className={styles.todayLine}
                    />
                    <text x={x(m.today)} y={PAD.t - 1} textAnchor="middle" className={styles.axis}>
                      Today
                    </text>
                  </g>
                )}
                <path d={path(scopePts)} className={styles.scopeLine} fill="none" />
                <path d={path(donePts)} className={styles.doneLine} fill="none" />
                {have.length === 1 && (
                  <>
                    <circle cx={scopePts[0]!.x} cy={scopePts[0]!.y} r="3" className={styles.scopeDot} />
                    <circle cx={donePts[0]!.x} cy={donePts[0]!.y} r="3" className={styles.doneDot} />
                  </>
                )}
                {hv && hv.scope !== null && hv.done !== null && (
                  <g>
                    <line
                      x1={x(hv.index)}
                      x2={x(hv.index)}
                      y1={PAD.t}
                      y2={PAD.t + ih}
                      className={styles.hoverLine}
                    />
                    <circle cx={x(hv.index)} cy={y(hv.scope)} r="3.5" className={styles.scopeDot} />
                    <circle cx={x(hv.index)} cy={y(hv.done)} r="3.5" className={styles.doneDot} />
                  </g>
                )}
              </svg>
              {hv && hv.scope !== null && (
                <div
                  className={styles.tip}
                  aria-hidden="true"
                  style={{ left: Math.min(Math.max(x(hv.index), 70), w - 70) }}
                >
                  <strong>{dayLabel(hv.time)}</strong>
                  <span>
                    Scope {hv.scope}, done {hv.done}
                  </span>
                </div>
              )}
            </div>
            <table className={styles.srOnly}>
              <caption>Burn-up by day, in issues</caption>
              <thead>
                <tr>
                  <th scope="col">Day</th>
                  <th scope="col">Scope</th>
                  <th scope="col">Done</th>
                </tr>
              </thead>
              <tbody>
                {have.map((d) => (
                  <tr key={d.time}>
                    <th scope="row">{dayLabel(d.time)}</th>
                    <td>{d.scope}</td>
                    <td>{d.done}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </>
        )}
      </div>
    </section>
  );
}
