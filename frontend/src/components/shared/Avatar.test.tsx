import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen, fireEvent, waitFor} from '@testing-library/react'

vi.mock('@/api/client', () => ({requestBlob: vi.fn()}))

import {requestBlob} from '@/api/client'
import {clearAvatarCache, invalidateAvatar} from '@/lib/avatarCache'
import Avatar from './Avatar'

beforeEach(() => {
  vi.clearAllMocks()
  clearAvatarCache()
  URL.createObjectURL = vi.fn(() => 'blob:uploaded')
  URL.revokeObjectURL = vi.fn()
})

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

  it('renders an uploaded avatar from the authenticated fetch, initials until it arrives', async () => {
    vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
    render(<Avatar name="Alice" avatarUrl="/api/users/u1/avatar" />)
    expect(screen.getByText('AL')).toBeInTheDocument()
    const img = (await screen.findByRole('img')) as HTMLImageElement
    expect(img.src).toBe('blob:uploaded')
    expect(requestBlob).toHaveBeenCalledWith('/api/users/u1/avatar')
  })

  it('stays on initials for an uploaded avatar the server does not have', async () => {
    vi.mocked(requestBlob).mockRejectedValue(Object.assign(new Error('not found'), {status: 404}))
    render(<Avatar name="Alice" avatarUrl="/api/users/u2/avatar" />)
    await waitFor(() => expect(requestBlob).toHaveBeenCalled())
    expect(screen.getByText('AL')).toBeInTheDocument()
    expect(screen.queryByRole('img')).not.toBeInTheDocument()
  })

  it('drops the image when the cache entry is invalidated (an upload was removed)', async () => {
    vi.mocked(requestBlob).mockResolvedValue(new Blob(['x']))
    render(<Avatar name="Alice" avatarUrl="/api/users/u3/avatar" />)
    await screen.findByRole('img')
    vi.mocked(requestBlob).mockRejectedValue(Object.assign(new Error('not found'), {status: 404}))
    invalidateAvatar('/api/users/u3/avatar')
    await waitFor(() => expect(screen.queryByRole('img')).not.toBeInTheDocument())
  })
})
