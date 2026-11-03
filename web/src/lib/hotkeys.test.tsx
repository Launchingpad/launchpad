import { fireEvent, render } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';
import { registerModal, useHotkeys } from './hotkeys';

function Harness({ onJ, onK }: { onJ: () => void; onK: () => void }) {
  useHotkeys({ j: onJ, 'mod+k': onK });
  return (
    <div>
      <input aria-label="field" />
      <textarea aria-label="area" />
      <div contentEditable suppressContentEditableWarning aria-label="rich" />
      <select aria-label="sel">
        <option>a</option>
      </select>
      <button>btn</button>
    </div>
  );
}

describe('hotkeys', () => {
  it('fire on the page', () => {
    const j = vi.fn();
    render(<Harness onJ={j} onK={vi.fn()} />);
    fireEvent.keyDown(document.body, { key: 'j' });
    expect(j).toHaveBeenCalledTimes(1);
  });

  it('do not fire while typing in inputs, textareas, selects or editable content', () => {
    const j = vi.fn();
    const { getByLabelText } = render(<Harness onJ={j} onK={vi.fn()} />);
    for (const label of ['field', 'area', 'sel']) fireEvent.keyDown(getByLabelText(label), { key: 'j' });
    const rich = getByLabelText('rich');
    Object.defineProperty(rich, 'isContentEditable', { value: true });
    fireEvent.keyDown(rich, { key: 'j' });
    expect(j).not.toHaveBeenCalled();
  });

  it('do not fire while a dialog is open', () => {
    const j = vi.fn();
    render(<Harness onJ={j} onK={vi.fn()} />);
    const unregister = registerModal();
    fireEvent.keyDown(document.body, { key: 'j' });
    unregister();
    expect(j).not.toHaveBeenCalled();
    fireEvent.keyDown(document.body, { key: 'j' });
    expect(j).toHaveBeenCalledTimes(1);
  });

  it('plain keys ignore Ctrl, Meta and Alt', () => {
    const j = vi.fn();
    render(<Harness onJ={j} onK={vi.fn()} />);
    fireEvent.keyDown(document.body, { key: 'j', ctrlKey: true });
    fireEvent.keyDown(document.body, { key: 'j', metaKey: true });
    fireEvent.keyDown(document.body, { key: 'j', altKey: true });
    expect(j).not.toHaveBeenCalled();
  });

  it('Cmd/Ctrl+K works even inside an input', () => {
    const k = vi.fn();
    const { getByLabelText } = render(<Harness onJ={vi.fn()} onK={k} />);
    fireEvent.keyDown(getByLabelText('field'), { key: 'k', metaKey: true });
    fireEvent.keyDown(getByLabelText('field'), { key: 'k', ctrlKey: true });
    expect(k).toHaveBeenCalledTimes(2);
  });

  it('Enter on a focused button is left to the button', () => {
    const enter = vi.fn();
    function H() {
      useHotkeys({ Enter: enter });
      return <button>x</button>;
    }
    const { getByText } = render(<H />);
    fireEvent.keyDown(getByText('x'), { key: 'Enter' });
    expect(enter).not.toHaveBeenCalled();
  });
});

describe('data-no-hotkeys (LAUN-163)', () => {
  it('ignores plain keys from inside a marked container and fires outside it', () => {
    const j = vi.fn();
    function H() {
      useHotkeys({ j });
      return (
        <div>
          <div data-no-hotkeys="">
            <input type="checkbox" aria-label="inside" />
          </div>
          <input type="checkbox" aria-label="outside" />
        </div>
      );
    }
    const { getByLabelText } = render(<H />);
    fireEvent.keyDown(getByLabelText('inside'), { key: 'j' });
    expect(j).not.toHaveBeenCalled();
    fireEvent.keyDown(getByLabelText('outside'), { key: 'j' });
    expect(j).toHaveBeenCalledTimes(1);
  });
});
