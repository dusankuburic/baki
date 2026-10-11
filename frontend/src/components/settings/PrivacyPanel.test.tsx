import {describe, it, expect, beforeEach} from 'vitest'
import {render, screen} from '@testing-library/react'
import PrivacyPanel from './PrivacyPanel'
import {useSystemStore} from '@/stores/systemStore'
import type {AppInfo} from '@/types'

const info = (sessionAnalytics: boolean): AppInfo => ({
  version: 'dev',
  platform: 'linux',
  arch: 'amd64',
  buildDate: '',
  gitCommit: '',
  capabilities: {sessionAnalytics},
})

beforeEach(() => {
  useSystemStore.setState({info: null, isLoaded: false})
})

describe('PrivacyPanel', () => {
  it('describes the OS keychain on a local backend', () => {
    useSystemStore.setState({info: info(true), isLoaded: true})
    render(<PrivacyPanel />)
    expect(screen.getByText(/operating system's keychain/)).toBeInTheDocument()
    expect(screen.queryByText(/stored there encrypted/)).toBeNull()
  })

  it('says keys are sent to and stored encrypted by a server backend', () => {
    useSystemStore.setState({info: info(false), isLoaded: true})
    render(<PrivacyPanel />)
    expect(screen.getByText(/sent to this PAD Analyzer server and stored there encrypted/)).toBeInTheDocument()
    expect(screen.queryByText(/operating system's keychain/)).toBeNull()
  })
})
