import { useUi } from './ui-context';
import { Dialog } from '../../ui/Dialog';
import styles from './shell.module.css';

const ROWS: [string, string][] = [
  ['⌘/Ctrl K', 'Command palette'],
  ['C', 'Create issue'],
  ['J / K', 'Move down / up the list'],
  ['Enter', 'Open the focused issue'],
  ['E', 'In the inbox: archive (or unarchive) the focused notification'],
  ['U', 'In the inbox: mark the focused notification read or unread'],
  ['X', 'Select or unselect the focused issue'],
  ['S', 'Change status'],
  ['A', 'Assign'],
  ['L', 'Toggle label'],
  ['P', 'Set priority'],
  ['⌘/Ctrl Enter', 'Create the issue (in the create dialog)'],
  ['S P A L', 'In the create dialog: open Status, Priority, Assignee, Labels'],
  [
    '⌘/Ctrl Z',
    'On the board or the list: undo your last change (move, create, status, priority, assignee, labels)',
  ],
  ['⌘/Ctrl Shift Z', 'On the board or the list: redo (Ctrl Y too, not on a Mac)'],
  ['Esc', 'Close a popover, then a dialog; clear selection; go back'],
  ['Alt K / Alt J', 'On a cycle page: the next / the previous cycle'],
  ['?', 'Show this help'],
];

export function ShortcutHelp() {
  const { helpOpen, setHelpOpen } = useUi();
  if (!helpOpen) return null;
  return (
    <Dialog title="Keyboard shortcuts" onClose={() => setHelpOpen(false)} width={460}>
      <table className={styles.keys}>
        <tbody>
          {ROWS.map(([k, d]) => (
            <tr key={k}>
              <th scope="row">
                <kbd>{k}</kbd>
              </th>
              <td>{d}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <p style={{ color: 'var(--muted)' }}>Shortcuts do not fire while you type in a field.</p>
    </Dialog>
  );
}
