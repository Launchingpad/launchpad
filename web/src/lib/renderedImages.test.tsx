import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { Markdown, renderedImageSources } from './markdown';

// LAUN-132: renderedImageSources walks the hast tree instead of rendering with react-dom/server. These tests keep it
// equal to what the Markdown component really puts on screen.
afterEach(cleanup);

const U = '/api/v1/orgs/orbit/attachments/a1/download';
const ORIGIN = window.location.origin;

/** What the real component renders: the src of every <img> in the DOM. */
function onScreen(text: string): string[] {
  const { container, unmount } = render(<Markdown text={text} />);
  const out = [...container.querySelectorAll('img')].map((i) => i.getAttribute('src') ?? '');
  unmount();
  return out;
}

const CASES: Record<string, string> = {
  'plain image': `![x](${U})`,
  'absolute own origin': `![x](${ORIGIN}${U})`,
  'fenced code': '```\n![x](' + U + ')\n```',
  'inline code': '`![x](' + U + ')`',
  escaped: '\\![x](' + U + ')',
  'indented code': '    ![x](' + U + ')',
  'plain link': `[x](${U})`,
  title: `![x](${U} "title")`,
  'angle brackets': `![x](<${U}>)`,
  'query and hash': `![x](${U}?a=1#h)`,
  uppercase: `![x](${U.toUpperCase().replace('/API/', '/api/')})`,
  'off origin': `![x](https://evil.test${U})`,
  'reference style': `![x][r]\n\n[r]: ${U}`,
  'reference style, undefined label': '![x][nope]',
  'collapsed reference': `![x][]\n\n[x]: ${U}`,
  'image in a link': `[![x](${U})](https://example.com)`,
  'image in a link to the same file': `[![x](${U})](${U})`,
  'raw html img': `<img src="${U}" alt="x">`,
  'raw html img next to a markdown one': `<img src="${U}"> ![y](${ORIGIN}/other.png)`,
  'data url': '![x](data:image/png;base64,iVBORw0KGgo=)',
  'javascript url': '![x](javascript:alert(1))',
  'blob url': '![x](blob:http://localhost/abc)',
  'protocol relative off origin': `![x](//evil.test${U})`,
  'same image twice': `![a](${U})\n\n![b](${U})`,
  'in a list and a quote': `- ![a](${U})\n\n> ![b](${ORIGIN}/other.png)`,
  'in a heading': `# ![a](${U})`,
  'empty src': '![x]()',
  'no images': 'just words, an exclamation! and [brackets]',
};

describe('renderedImageSources (LAUN-132)', () => {
  for (const [name, text] of Object.entries(CASES)) {
    it(`agrees with the rendered Markdown: ${name}`, () => {
      expect(renderedImageSources(text)).toEqual(onScreen(text));
    });
  }

  it('finds the images that render', () => {
    expect(renderedImageSources(`![x](${U})`)).toEqual([U]);
    expect(renderedImageSources(`![x][r]\n\n[r]: ${U}`)).toEqual([U]);
    expect(renderedImageSources(`[![x](${U})](https://example.com)`)).toEqual([U]);
    expect(renderedImageSources(`![x](${U}?a=1#h)`)).toEqual([`${U}?a=1#h`]);
  });

  it('lists the same image twice when it is shown twice', () => {
    expect(renderedImageSources(`![a](${U})\n\n![b](${U})`)).toEqual([U, U]);
  });

  it('finds none for code, escapes, plain links, off-origin, data: and raw html', () => {
    for (const name of [
      'fenced code',
      'inline code',
      'escaped',
      'indented code',
      'plain link',
      'off origin',
      'data url',
      'javascript url',
      'raw html img',
      'reference style, undefined label',
      'empty src',
    ]) {
      expect(renderedImageSources(CASES[name]!), name).toEqual([]);
    }
  });

  it('a raw html img is dropped but the markdown image beside it counts', () => {
    expect(renderedImageSources(CASES['raw html img next to a markdown one']!)).toEqual([
      `${ORIGIN}/other.png`,
    ]);
  });

  it('is cheap for text with no image syntax', () => {
    expect(renderedImageSources('')).toEqual([]);
    expect(renderedImageSources('# title\n\ntext')).toEqual([]);
  });
});

describe('the entry chunk stays free of react-dom/server (LAUN-132)', () => {
  function* sources(dir: string): Generator<string> {
    for (const f of readdirSync(dir)) {
      const p = join(dir, f);
      if (statSync(p).isDirectory()) yield* sources(p);
      else if (/\.(ts|tsx)$/.test(f) && !/\.test\./.test(f)) yield p;
    }
  }
  it('no production source imports react-dom/server', () => {
    const bad = [...sources('src')].filter((f) =>
      /(from|import\()\s*['"]react-dom\/server['"]/.test(readFileSync(f, 'utf8')),
    );
    expect(bad).toEqual([]);
  });
});
