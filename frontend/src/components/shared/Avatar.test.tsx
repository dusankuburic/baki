import {describe, it, expect} from 'vitest'
import {render, screen, fireEvent} from '@testing-library/react'
import Avatar from './Avatar'

describe('Avatar', () => {
  it('renders initials when there is no avatarUrl', () => {
    render(<Avatar name="Alice" />)
    expect(screen.getByText('AL')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('renders the image when avatarUrl is provided', () => {
    render(<Avatar name="Alice" avatarUrl="https://example.com/a.png" />)
    const img = screen.getByRole('img') as HTMLImageElement
    expect(img.src).toBe('https://example.com/a.png')
  })

  it('falls back to initials if the image fails to load', () => {
    render(<Avatar name="Alice" avatarUrl="https://example.com/broken.png" />)
    const img = screen.getByRole('img')
    fireEvent.error(img)
    expect(screen.getByText('AL')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('uses two-word initials for multi-word names', () => {
    render(<Avatar name="Jane Doe" />)
    expect(screen.getByText('JD')).toBeInTheDocument()
  })
})
