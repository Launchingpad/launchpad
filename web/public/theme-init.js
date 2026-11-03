// Runs before first paint so a saved manual theme does not flash. Kept as a
// separate file because the CSP forbids inline scripts.
try {
  var t = localStorage.getItem('devops-theme');
  if (t === 'light' || t === 'dark') document.documentElement.setAttribute('data-theme', t);
  // LAUN-235: the look (Paper, Terminal; none is Linear), mirrored from the server by src/lib/theme.ts.
  var l = localStorage.getItem('launchpad-look');
  if (l === 'paper' || l === 'terminal') document.documentElement.setAttribute('data-look', l);
} catch (e) {
  /* storage blocked: follow the OS */
}
