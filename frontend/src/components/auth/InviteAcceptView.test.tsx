import {describe, it, expect, vi, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import InviteAcceptView from './InviteAcceptView'
import {useOrgStore} from '@/stores/orgStore'

const initialState = useOrgStore.getState()

beforeEach(() => {
  useOrgStore.setState(initialState, true)
  vi.resetAllMocks()
})

describe('InviteAcceptView', () => {
  it('redeems the token on mount and shows success', async () => {
    const acceptInvite = vi.fn().mockResolvedValue(undefined)
    useOrgStore.setState({acceptInvite})

    render(<InviteAcceptView token="tok-1" onDone={() => {}} />)

    expect(await screen.findByText(/invitation accepted/i)).toBeInTheDocument()
    expect(acceptInvite).toHaveBeenCalledWith('tok-1')
  })

  it('shows an error state when acceptance fails', async () => {
    const acceptInvite = vi.fn().mockRejectedValue(new Error('invite not found'))
    useOrgStore.setState({acceptInvite})

    render(<InviteAcceptView token="bad" onDone={() => {}} />)

    expect(await screen.findByText(/invitation failed/i)).toBeInTheDocument()
    expect(await screen.findByText(/invite not found/i)).toBeInTheDocument()
  })
})
