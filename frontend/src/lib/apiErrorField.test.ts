import {describe, it, expect} from 'vitest'
import {ApiError} from '@/api/client'
import {apiErrorField} from './apiErrorField'

describe('apiErrorField', () => {
  it('reads the field (and optional reason) out of the error envelope details', () => {
    const err = new ApiError('invalid severity', 400, 'INVALID_SEVERITY', 'req-1', {
      field: 'config.severity',
      reason: 'INVALID_SEVERITY',
    })
    expect(apiErrorField(err)).toEqual({field: 'config.severity', reason: 'INVALID_SEVERITY'})

    const bare = new ApiError('url is required', 400, 'INVALID_CHANNEL', 'req-2', {field: 'url'})
    expect(apiErrorField(bare)).toEqual({field: 'url', reason: undefined})
  })

  it('returns null for anything without a string details.field', () => {
    expect(apiErrorField(null)).toBeNull()
    expect(apiErrorField(new Error('plain'))).toBeNull()
    expect(apiErrorField(new ApiError('bad', 400))).toBeNull()
    expect(apiErrorField(new ApiError('bad', 400, 'BAD_REQUEST', 'r', {}))).toBeNull()
    expect(apiErrorField(new ApiError('bad', 400, 'BAD_REQUEST', 'r', {field: 42}))).toBeNull()
    expect(apiErrorField(new ApiError('bad', 400, 'BAD_REQUEST', 'r', {field: ''}))).toBeNull()
  })
})
