import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { LabelPill } from './LabelPill';

describe('LabelPill (LAUN-46)', () => {
  it('shows the name and a hidden colour dot, and no button without onRemove', () => {
    const { container } = render(<LabelPill label={{ name: 'bug', color: '#ff0000' }} />);
    expect(screen.getByText('bug')).toBeInTheDocument();
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(container.querySelector('[aria-hidden="true"]')).toHaveStyle({ background: '#ff0000' });
  });

  it('removes through a named button and does not fire onClick', async () => {
    const onRemove = vi.fn();
    const onClick = vi.fn();
    render(<LabelPill label={{ name: 'bug', color: '#f00' }} onRemove={onRemove} onClick={onClick} />);
    await userEvent.click(screen.getByRole('button', { name: 'Remove label bug' }));
    expect(onRemove).toHaveBeenCalledTimes(1);
    expect(onClick).not.toHaveBeenCalled();
  });

  it('keeps the full name in a title, so a truncated pill still shows it (LAUN-230)', () => {
    const name = 'Customer reported regression in the notification settings page';
    render(<LabelPill label={{ name, color: '#f00' }} />);
    expect(screen.getByText(name)).toHaveAttribute('title', name);
  });
});
