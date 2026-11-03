import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { Logo, avatarSrc } from './Logo';

describe('avatarSrc', () => {
  it('keeps only the path and query, on this origin', () => {
    expect(avatarSrc('https://api.example.com/api/v1/orgs/orbit/avatar?v=3')).toBe(
      '/api/v1/orgs/orbit/avatar?v=3',
    );
    expect(avatarSrc('/api/v1/orgs/orbit/avatar?v=1')).toBe('/api/v1/orgs/orbit/avatar?v=1');
    expect(avatarSrc(null)).toBeNull();
    expect(avatarSrc(undefined)).toBeNull();
  });
});

describe('Logo', () => {
  it('shows the first letter when there is no image', () => {
    render(<Logo name="orbit labs" />);
    expect(screen.getByRole('img', { name: 'orbit labs' })).toHaveTextContent('O');
  });

  it('shows the image with the name as its alt text', () => {
    render(<Logo name="Orbit" src="/api/v1/orgs/orbit/avatar?v=2" />);
    const img = screen.getByRole('img', { name: 'Orbit' });
    expect(img).toHaveAttribute('src', '/api/v1/orgs/orbit/avatar?v=2');
  });

  it('is decorative (empty alt) when the name is written next to it', () => {
    const { container } = render(<Logo name="Orbit" src="/a?v=1" decorative />);
    expect(container.querySelector('img')).toHaveAttribute('alt', '');
  });

  it('falls back to the letter when the image does not load', () => {
    const { container } = render(<Logo name="Orbit" src="/a?v=1" />);
    fireEvent.error(container.querySelector('img')!);
    expect(container.querySelector('img')).toBeNull();
    expect(screen.getByRole('img', { name: 'Orbit' })).toHaveTextContent('O');
  });

  it('tries a new URL again after a failure', () => {
    const { container, rerender } = render(<Logo name="Orbit" src="/a?v=1" />);
    fireEvent.error(container.querySelector('img')!);
    rerender(<Logo name="Orbit" src="/a?v=2" />);
    expect(container.querySelector('img')).toHaveAttribute('src', '/a?v=2');
  });
});
