import styles from '../styles/ui.module.css';

function hue(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360;
  return h;
}

export function Avatar({ name, username }: { name: string; username?: string }) {
  const initials = name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]!.toUpperCase())
    .join('');
  return (
    <span
      className={styles.avatar}
      style={{ background: `hsl(${hue(username ?? name)} 70% 82%)` }}
      aria-hidden="true"
    >
      {initials || '?'}
    </span>
  );
}
