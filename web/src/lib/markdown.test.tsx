import { cleanup, fireEvent, render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Markdown, isOwnImage, safeUrl } from './markdown';

const html = (md: string) => render(<Markdown text={md} />).container;

describe('markdown sanitising', () => {
  it('never turns a script tag into an element', () => {
    const c = html('hello <script>alert(1)</script> world');
    expect(c.querySelector('script')).toBeNull();
    expect(c.innerHTML.toLowerCase()).not.toContain('<script');
  });

  it('neutralises javascript: links', () => {
    const c = html('[click](javascript:alert(1)) and [also](JaVaScRiPt:alert(2))');
    for (const a of c.querySelectorAll('a')) expect(a.getAttribute('href') ?? '').not.toMatch(/javascript/i);
    expect(c.innerHTML.toLowerCase()).not.toContain('href="javascript');
  });

  it('neutralises data: and vbscript: links', () => {
    const c = html('[x](data:text/html;base64,PHNjcmlwdD4=) [y](vbscript:msgbox)');
    for (const a of c.querySelectorAll('a')) expect(a.getAttribute('href') ?? '').toBe('');
  });

  it('drops onerror and other event-handler attributes', () => {
    const c = html('<img src="x" onerror="alert(1)"> <a href="https://e.com" onclick="alert(2)">a</a>');
    expect(c.innerHTML).not.toMatch(/onerror|onclick/i);
    expect(c.querySelector('img')).toBeNull();
  });

  it('does not render raw html elements', () => {
    const c = html('<iframe src="https://evil.example"></iframe><style>*{display:none}</style><b>bold</b>');
    expect(c.querySelector('iframe')).toBeNull();
    expect(c.querySelector('style')).toBeNull();
    expect(c.querySelector('b')).toBeNull();
  });

  it('blocks images from other origins and keeps them as text', () => {
    const c = html('![tracker](https://evil.example/pixel.png)');
    expect(c.querySelector('img')).toBeNull();
    expect(c.textContent).toContain('image');
  });

  it('blocks data: and protocol-relative images', () => {
    expect(html('![d](data:image/png;base64,AAAA)').querySelector('img')).toBeNull();
    expect(html('![p](//evil.example/x.png)').querySelector('img')).toBeNull();
  });

  it('allows images from our own API origin', () => {
    const c = html(`![ok](${window.location.origin}/api/v1/orgs/orbit/attachments/1/download)`);
    const img = c.querySelector('img');
    expect(img).not.toBeNull();
    expect(img!.getAttribute('src')).toContain('/api/v1/orgs/orbit/attachments/1/download');
    expect(img!.getAttribute('referrerpolicy')).toBe('no-referrer');
  });

  it('opens real links safely', () => {
    const a = html('[docs](https://example.com/x)').querySelector('a')!;
    expect(a.getAttribute('href')).toBe('https://example.com/x');
    expect(a.getAttribute('rel')).toContain('noopener');
    expect(a.getAttribute('target')).toBe('_blank');
  });

  it('renders ordinary markdown', () => {
    const c = html('# Title\n\n- one\n- **two**\n\n`code`');
    expect(c.querySelector('h1')?.textContent).toBe('Title');
    expect(c.querySelectorAll('li')).toHaveLength(2);
    expect(c.querySelector('strong')?.textContent).toBe('two');
  });
});

describe('url helpers', () => {
  it('safeUrl only passes http, https and mailto', () => {
    expect(safeUrl('https://a.b/c')).toBe('https://a.b/c');
    expect(safeUrl('mailto:a@b.c')).toBe('mailto:a@b.c');
    expect(safeUrl('/relative')).toBe('/relative');
    expect(safeUrl('javascript:alert(1)')).toBe('');
    expect(safeUrl(' javascript:alert(1)')).toBe('');
  });
  it('isOwnImage compares origins exactly', () => {
    expect(isOwnImage(`${window.location.origin}/api/x.png`)).toBe(true);
    expect(isOwnImage('/api/v1/x.png')).toBe(true);
    expect(isOwnImage(`${window.location.origin}.evil.example/x.png`)).toBe(false);
    expect(isOwnImage('https://evil.example/x.png')).toBe(false);
    expect(isOwnImage(undefined)).toBe(false);
  });
});

describe('task lists (LAUN-47)', () => {
  const boxes = (c: HTMLElement) => [...c.querySelectorAll<HTMLInputElement>('input[type=checkbox]')];

  function setup(md: string, onToggle?: (s: string) => void) {
    const r = render(<Markdown text={md} onToggleTask={onToggle} />);
    return r.container;
  }

  it('renders checkboxes with checked state, no stray marker text, accessible names', () => {
    const c = setup('- [ ] one\n- [x] two\n- [X] three\n- plain');
    const b = boxes(c);
    expect(b.map((x) => x.checked)).toEqual([false, true, true]);
    expect(b.map((x) => x.getAttribute('aria-label'))).toEqual(['one', 'two', 'three']);
    expect(c.textContent).not.toContain('[');
    expect(c.querySelectorAll('li').length).toBe(4);
  });

  it('is read-only without a toggle handler', () => {
    const c = setup('- [ ] one');
    expect(boxes(c)[0]!.disabled).toBe(true);
  });

  it('flips exactly the clicked item (nested, * and + bullets, numbered, CRLF)', () => {
    const md = '- [ ] a\r\n  - [ ] b\r\n* [x] c\r\n+ [ ] d\r\n1. [ ] e\r\n';
    for (let i = 0; i < 5; i++) {
      const calls: string[] = [];
      const c = setup(md, (s) => calls.push(s));
      expect(boxes(c)).toHaveLength(5);
      fireEvent.click(boxes(c)[i]!);
      expect(calls).toHaveLength(1);
      const before = md.split('\r\n');
      const after = calls[0]!.split('\r\n');
      const changed = before.map((l, k) => (l !== after[k] ? k : -1)).filter((k) => k >= 0);
      expect(changed).toEqual([i]);
      expect(calls[0]!.length).toBe(md.length);
      cleanup();
    }
  });

  it('turns [x] and [X] back into [ ]', () => {
    for (const mk of ['x', 'X']) {
      const calls: string[] = [];
      const c = setup(`- [${mk}] done`, (s) => calls.push(s));
      fireEvent.click(boxes(c)[0]!);
      expect(calls).toEqual(['- [ ] done']);
      cleanup();
    }
  });

  it('leaves code blocks, inline code and escaped markers alone and counts around them', () => {
    const md = '```\n- [ ] in fence\n```\n\n- `[ ]` inline\n- \\[ ] escaped\n- [ ] real\n';
    const calls: string[] = [];
    const c = setup(md, (s) => calls.push(s));
    expect(boxes(c)).toHaveLength(1);
    expect(c.querySelector('pre')!.textContent).toContain('- [ ] in fence');
    fireEvent.click(boxes(c)[0]!);
    expect(calls[0]).toBe(md.replace('- [ ] real', '- [x] real'));
  });

  it('works in loose lists and mixed lists', () => {
    const md = '- [ ] one\n\n- not a task\n\n- [x] three\n';
    const calls: string[] = [];
    const c = setup(md, (s) => calls.push(s));
    expect(boxes(c)).toHaveLength(2);
    fireEvent.click(boxes(c)[1]!);
    expect(calls[0]).toBe(md.replace('[x]', '[ ]'));
  });

  it('keeps the sanitiser strict: raw inputs in text never become inputs', () => {
    const c = setup('<input type="checkbox" checked> hi\n\n<input type="text" onfocus="x()">');
    expect(c.querySelector('input')).toBeNull();
  });

  it('counts offsets right after emoji and CJK text and with a tab before the marker', () => {
    const md = '😀 intro 日本語\n\n- [ ] 日本 😀 one\n-\t[ ] two\n- [x] 🎉\n';
    for (let i = 0; i < 3; i++) {
      const calls: string[] = [];
      const c = setup(md, (s) => calls.push(s));
      expect(boxes(c)).toHaveLength(3);
      fireEvent.click(boxes(c)[i]!);
      const a = md.split('\n');
      const b = calls[0]!.split('\n');
      expect(a.map((l, k) => (l !== b[k] ? k : -1)).filter((k) => k >= 0)).toEqual([2 + i]);
      cleanup();
    }
  });

  it('gives a bare item an accessible name and keeps the box inside a 24px hit area', () => {
    const c = setup('- [ ]\n- [x] with [a link](https://e.com)');
    expect(boxes(c)[0]!.getAttribute('aria-label')).toBe('Task');
    expect(boxes(c)[1]!.getAttribute('aria-label')).toBe('with a link');
    const label = boxes(c)[1]!.parentElement!;
    expect(label.tagName).toBe('LABEL');
    expect(label.textContent).toBe(''); // the item text and its links are outside the label
    expect(c.querySelector('a')!.closest('label')).toBeNull();
  });
});
