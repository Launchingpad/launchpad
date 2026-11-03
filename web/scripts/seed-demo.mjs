// Seeds realistic demo data through the API (for screenshots and manual testing).
// Usage: DEV_OPS_URL=http://localhost:18091 DEV_OPS_TOKEN=odt_... node scripts/seed-demo.mjs
const base = process.env.DEV_OPS_URL ?? 'http://localhost:18091';
const token = process.env.DEV_OPS_TOKEN;
const org = process.env.DEV_OPS_ORG ?? 'orbit';
if (!token) throw new Error('set DEV_OPS_TOKEN');

async function api(method, path, body) {
  const res = await fetch(`${base}/api/v1/orgs/${org}${path}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`${method} ${path} -> ${res.status} ${text}`);
  return text ? JSON.parse(text) : undefined;
}

const labels = [
  ['bug', '#d6481f'],
  ['feature', '#4a4acf'],
  ['ios', '#2f9e8f'],
  ['android', '#5a9e2f'],
  ['backend', '#c27b0e'],
  ['design', '#b0409a'],
  ['security', '#c0392b'],
];
for (const [name, color] of labels) {
  try {
    await api('POST', '/labels', { name, color });
  } catch (e) {
    if (!String(e).includes('409')) throw e;
  }
}
try {
  await api('POST', '/teams', { key: 'WEB', name: 'Web' });
} catch (e) {
  if (!String(e).includes('409')) throw e;
}

const titles = [
  ['Chat list flickers when a new message arrives', 'bug,ios', 2, 'In Progress', 'chen'],
  ['Push notification deep links open the wrong child', 'bug,backend', 1, 'In Review', 'claude'],
  ['Add presence indicator to the contact card', 'feature,design', 3, 'Todo', 'chen'],
  ['Migrate Android chat screen to the new theme', 'android,design', 3, 'In Progress', 'chen'],
  ['Rotate admin tokens after first boot', 'security,backend', 2, 'Todo', null],
  ['OTP rate limit returns 500 under load', 'bug,backend,security', 1, 'In Testing', 'claude'],
  ['Parent dashboard: show last seen time', 'feature,ios', 4, 'Backlog', null],
  ['Export chat transcript as PDF', 'feature', 4, 'Backlog', null],
  ['Crash on rotate in the child profile (iPad)', 'bug,ios', 2, 'Todo', 'chen'],
  ['Write the moderation appeals runbook', 'design', 3, 'Done', 'claude'],
  ['Tighten the CSP on the admin console', 'security', 2, 'Done', 'chen'],
  ['Onboarding copy review for the Welsh locale', 'design', 4, 'Backlog', null],
  ['Group chats: limit to 8 participants', 'feature,backend', 3, 'In Progress', 'claude'],
  ['Android: keyboard covers the send button on small phones', 'bug,android', 2, 'Todo', null],
  ['Speed up the contacts query on large orbits', 'backend', 3, 'In Review', 'chen'],
  ['Remove the legacy invite flow', 'backend', 4, 'Canceled', null],
  ['Duplicate: chat flicker on iOS 26', 'bug,ios', 0, 'Canceled', null],
  ['Add dark mode to the marketing site', 'design,feature', 4, 'Backlog', null],
  ['Alert when a child leaves a safe zone', 'feature,ios,android', 2, 'In Progress', 'chen'],
  ['Audit logging for admin role changes', 'security,backend', 2, 'Todo', 'claude'],
  ['Settings: manage devices screen', 'feature,android', 3, 'Todo', null],
  ['Fix typos in the privacy policy page', 'design', 0, 'Done', null],
  ['Flaky J14 journey on the Pixel tablet', 'bug,android', 3, 'In Review', 'claude'],
  ['Support attachments up to 100 MB', 'feature,backend', 3, 'Backlog', null],
  ['Reduce app size by dropping unused fonts', 'ios', 4, 'Backlog', 'chen'],
  ['Weekly digest email for parents', 'feature,backend', 3, 'Todo', null],
  ['Rate-limit the OTP verify endpoint per IP', 'security,backend', 1, 'In Progress', 'claude'],
  ['Child can see deleted contact in the picker', 'bug,android,ios', 2, 'In Testing', 'chen'],
];

const made = [];
for (const [title, lbls, priority, state, assignee] of titles) {
  const body = {
    team: 'ORB',
    title,
    description: `## Summary\n\n${title}.\n\n- Reproduce on a clean install\n- Check the **release notes** before shipping\n\nSee [the docs](https://example.com/docs) for background.`,
    priority,
    state,
    labels: lbls.split(','),
  };
  if (assignee) body.assignee = assignee;
  made.push(await api('POST', '/issues', body));
}

// Sub-issues, relations and comments.
const parent = made[18];
for (const t of [
  'Define safe zone geometry',
  'Server: zone entry/exit events',
  'iOS: map overlay for zones',
]) {
  await api('POST', '/issues', {
    team: 'ORB',
    title: t,
    parent: parent.identifier,
    state: t.startsWith('Define') ? 'Done' : 'Todo',
    labels: ['feature'],
  });
}
await api('POST', `/issues/${made[1].identifier}/relations`, { type: 'blocks', issue: made[0].identifier });
await api('POST', `/issues/${made[5].identifier}/relations`, { type: 'related', issue: made[26].identifier });
await api('POST', `/issues/${made[16].identifier}/relations`, {
  type: 'duplicateOf',
  issue: made[0].identifier,
});

const c1 = await api('POST', `/issues/${made[0].identifier}/comments`, {
  body: 'Reproduced on iOS 26.1. It happens only when **two** messages land in the same frame.',
});
await api('POST', `/issues/${made[0].identifier}/comments`, {
  body: 'I think the diffing key is the sent timestamp. Switching to the message id.',
  parentId: c1.id,
});
await api('POST', `/issues/${made[0].identifier}/comments`, {
  body: 'Related log line:\n\n```\nlist diff: duplicate id 7f2a\n```',
});

for (const t of [
  'Login screen: no error shown for a wrong code',
  'Board view: columns should scroll on phones',
]) {
  await api('POST', '/issues', { team: 'WEB', title: t, priority: 3, labels: ['bug'], assignee: 'chen' });
}
console.log(`seeded ${made.length + 5} issues`);
