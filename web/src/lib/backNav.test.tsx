import { useRef } from 'react';
import { act, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes, useNavigate } from 'react-router-dom';
import { describe, expect, it } from 'vitest';
import { useScrollRestore } from './backNav';

function List() {
  const ref = useScrollRestore<HTMLDivElement>('t');
  const nav = useNavigate();
  return (
    <div>
      <div ref={ref} data-testid="box" />
      <button onClick={() => nav('/detail')}>open</button>
    </div>
  );
}
function Detail() {
  const nav = useNavigate();
  return <button onClick={() => nav(-1)}>back</button>;
}

function Board() {
  const ref = useRef<HTMLDivElement>(null);
  useScrollRestore('board', ref);
  const nav = useNavigate();
  return (
    <div>
      <div ref={ref} data-testid="board" />
      <button onClick={() => nav('/detail')}>open</button>
    </div>
  );
}

describe('useScrollRestore (LAUN-89)', () => {
  it('puts the scroll offset back when the entry comes back', async () => {
    render(
      <MemoryRouter initialEntries={['/list']}>
        <Routes>
          <Route path="/list" element={<List />} />
          <Route path="/detail" element={<Detail />} />
        </Routes>
      </MemoryRouter>,
    );
    const box = screen.getByTestId('box');
    box.scrollTop = 300;
    act(() => {
      box.dispatchEvent(new Event('scroll'));
    });
    act(() => screen.getByText('open').click());
    act(() => screen.getByText('back').click());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.getByTestId('box').scrollTop).toBe(300);
  });

  it('puts the sideways scroll of a board (an external ref) back', async () => {
    render(
      <MemoryRouter initialEntries={['/board']}>
        <Routes>
          <Route path="/board" element={<Board />} />
          <Route path="/detail" element={<Detail />} />
        </Routes>
      </MemoryRouter>,
    );
    const board = screen.getByTestId('board');
    board.scrollLeft = 420;
    act(() => {
      board.dispatchEvent(new Event('scroll'));
    });
    act(() => screen.getByText('open').click());
    act(() => screen.getByText('back').click());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.getByTestId('board').scrollLeft).toBe(420);
  });
});
