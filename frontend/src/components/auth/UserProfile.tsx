import React, { useState } from 'react'
import { useAuthStore } from '@/stores/authStore'
import { authApi } from '@/api/auth'

export const UserProfile: React.FC = () => {
  const { user, logout } = useAuthStore()
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [status, setStatus] = useState<{ type: 'success' | 'error', message: string } | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  const handleChangePassword = async (e: React.FormEvent) => {
    e.preventDefault()
    if (newPassword !== confirmPassword) {
      setStatus({ type: 'error', message: 'New passwords do not match' })
      return
    }

    setIsSubmitting(true)
    setStatus(null)
    try {
      await authApi.changePassword(currentPassword, newPassword)
      setStatus({ type: 'success', message: 'Password updated successfully' })
      setCurrentPassword('')
      setNewPassword('')
      setConfirmPassword('')
    } catch (err) {
      setStatus({ type: 'error', message: err instanceof Error ? err.message : 'Failed to update password' })
    } finally {
      setIsSubmitting(false)
    }
  }

  if (!user) return null

  return (
    <div className="p-6 max-w-md mx-auto bg-white rounded-xl shadow-md space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-bold">User Profile</h1>
        <button
          onClick={() => logout()}
          className="px-4 py-2 bg-red-600 text-white rounded hover:bg-red-700 transition-colors"
        >
          Logout
        </button>
      </div>

      <div className="space-y-2">
        <p><strong>Email:</strong> {user.email}</p>
        <p><strong>Role:</strong> <span className="capitalize">{user.role}</span></p>
      </div>

      <hr className="my-4" />

      <h2 className="text-xl font-semibold">Change Password</h2>
      <form onSubmit={handleChangePassword} className="space-y-3">
        <div>
          <label className="block text-sm font-medium text-gray-700">Current Password</label>
          <input
            type="password"
            value={currentPassword}
            onChange={(e) => setCurrentPassword(e.target.value)}
            required
            className="mt-1 block w-full border border-gray-300 rounded-md shadow-sm p-2 focus:ring-blue-500 focus:border-blue-500"
          />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700">New Password</label>
          <input
            type="password"
            value={newPassword}
            onChange={(e) => setNewPassword(e.target.value)}
            required
            className="mt-1 block w-full border border-gray-300 rounded-md shadow-sm p-2 focus:ring-blue-500 focus:border-blue-500"
          />
        </div>
        <div>
          <label className="block text-sm font-medium text-gray-700">Confirm New Password</label>
          <input
            type="password"
            value={confirmPassword}
            onChange={(e) => setConfirmPassword(e.target.value)}
            required
            className="mt-1 block w-full border border-gray-300 rounded-md shadow-sm p-2 focus:ring-blue-500 focus:border-blue-500"
          />
        </div>

        {status && (
          <div className={`p-2 rounded text-sm ${status.type === 'success' ? 'bg-green-100 text-green-800' : 'bg-red-100 text-red-800'}`}>
            {status.message}
          </div>
        )}

        <button
          type="submit"
          disabled={isSubmitting}
          className="w-full py-2 px-4 bg-blue-600 text-white rounded-md hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-blue-500 disabled:opacity-50"
        >
          {isSubmitting ? 'Updating...' : 'Update Password'}
        </button>
      </form>
    </div>
  )
}
