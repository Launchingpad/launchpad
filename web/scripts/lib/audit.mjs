/* global document, window, getComputedStyle, */
// Real overflow audit (not just documentElement.scrollWidth, which a clipped
// `overflow-x: hidden` ancestor hides). Runs inside the page. For every
// rendered element it checks that the element's right edge is inside the
// viewport AND inside every ancestor that clips it. An ancestor with
// overflow-x auto/scroll is an intentional scroll container: whatever sits
// inside it is allowed to be wider than the viewport, but the container's own
// box must still fit. `.sr-only` and elements with data-audit-ignore are skipped.
export async function auditOverflow(page) {
  return page.evaluate(() => {
    const vw = window.innerWidth;
    const EPS = 0.5;
    const bad = [];
    const name = (el) => {
      const cls = typeof el.className === 'string' ? el.className.split(/\s+/)[0] : '';
      const txt = (el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 40);
      return `${el.tagName.toLowerCase()}${el.id ? '#' + el.id : ''}${cls ? '.' + cls : ''} "${txt}"`;
    };
    for (const el of document.body.querySelectorAll('*')) {
      if (el.closest('.sr-only,[data-audit-ignore]')) continue;
      const cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden') continue;
      const r = el.getBoundingClientRect();
      if (r.width === 0 || r.height === 0) continue;
      // Intentional horizontal scrollers are opt-in: [data-hscroll], code blocks, tables.
      // Anything inside one may be wider than the viewport, but the scroller's own box may not.
      // Every other ancestor that clips (overflow-x not visible, auto and scroll included)
      // must contain the element, or it is clipped or scrolled away without the user asking.
      const isHScroll = (a) => a.matches('[data-hscroll],pre,table');
      let inScroller = null;
      let clipper = null;
      for (let a = el.parentElement; a && a !== document.documentElement; a = a.parentElement) {
        if (isHScroll(a)) {
          inScroller = a;
          break;
        }
        const ox = getComputedStyle(a).overflowX;
        const ar = a.getBoundingClientRect();
        if (ox !== 'visible' && r.right > ar.right + EPS && !clipper) clipper = a;
      }
      if (inScroller) {
        const sr = inScroller.getBoundingClientRect();
        if (sr.right > vw + EPS && !bad.some((b) => b.el === name(inScroller)))
          bad.push({
            el: name(inScroller),
            why: 'scroll container wider than viewport',
            right: Math.round(sr.right),
            vw,
          });
        continue;
      }
      if (clipper) {
        bad.push({ el: name(el), why: `clipped by ${name(clipper)}`, right: Math.round(r.right), vw });
        continue;
      }
      if (r.right > vw + EPS)
        bad.push({ el: name(el), why: 'outside viewport', right: Math.round(r.right), vw });
    }
    return bad;
  });
}
