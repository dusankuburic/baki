import React, { useState, useEffect } from 'react'
import { adminApi, type MigrationStatus } from '@/api/admin'
import { type AuthUser } from '@/api/auth'
import { useAuthStore } from '@/stores/authStore'

export const AdminDashboard: React.FC = () => {
  const { user: currentUser } = useAuthStore()
  const [migrationStatus, setMigrationStatus] = useState<MigrationStatus | null>(null)
  const [users, setUsers] = useState<AuthUser[]>([])
  const [isLoading, setIsLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const fetchStatus = async () => {
    try {
      const status = await adminApi.getMigrationStatus()
      setMigrationStatus(status)
    } catch (err) {
      console.error('Failed to fetch migration status', err)
    }
  }

  const fetchUsers = async () => {
    try {
      const u = await adminApi.listUsers()
      setUsers(u)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to fetch users')
    }
  }

  const fetchData = async () => {
    setIsLoading(true)
    await Promise.all([fetchStatus(), fetchUsers()])
    setIsLoading(false)
  }

  useEffect(() => {
    if (currentUser?.role === 'admin') {
      fetchData()
      const interval = setInterval(fetchStatus, 5000)
      return () => clearInterval(interval)
    }
  }, [currentUser])

  const handleStartMigration = async () => {
    if (!confirm('Start data migration from filesystem to database? This will skip already migrated flows.')) return
    
    setError(null)
    try {
      await adminApi.startMigration()
      await fetchStatus()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to start migration')
    }
  }

  const handleRoleChange = async (userId: string, newRole: string) => {
    if (userId === currentUser?.id && newRole !== 'admin') {
      if (!confirm('You are about to remove your own administrator privileges. Are you sure?')) return
    }

    try {
      await adminApi.setUserRole(userId, newRole)
      await fetchUsers()
    } catch (err) {
      alert('Failed to change role: ' + (err instanceof Error ? err.message : 'Unknown error'))
    }
  }

  if (currentUser?.role !== 'admin') {
    return <div className="p-4 text-red-600">Access Denied: Administrator role required.</div>
  }

  return (
    <div className="p-6 space-y-8 max-w-5xl mx-auto">
      <div className="flex items-center justify-between">
        <h1 className="text-3xl font-bold">Admin Dashboard</h1>
        <button onClick={fetchData} className="text-blue-600 hover:underline" disabled={isLoading}>
          {isLoading ? 'Refreshing...' : 'Refresh All'}
        </button>
      </div>

      <section className="bg-white p-6 rounded-xl shadow-md border border-gray-200">
        <h2 className="text-xl font-semibold mb-4">User Management</h2>
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-gray-200">
            <thead className="bg-gray-50">
              <tr>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Email</th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">ID</th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Role</th>
                <th className="px-6 py-3 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Actions</th>
              </tr>
            </thead>
            <tbody className="bg-white divide-y divide-gray-200">
              {users.map(u => (
                <tr key={u.id}>
                  <td className="px-6 py-4 whitespace-nowrap text-sm font-medium text-gray-900">{u.email}</td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">{u.id}</td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                    <span className={`px-2 py-1 rounded-md text-xs font-bold uppercase ${
                      u.role === 'admin' ? 'bg-purple-100 text-purple-800' :
                      u.role === 'member' ? 'bg-blue-100 text-blue-800' :
                      'bg-gray-100 text-gray-800'
                    }`}>
                      {u.role}
                    </span>
                  </td>
                  <td className="px-6 py-4 whitespace-nowrap text-sm text-gray-500">
                    <select
                      value={u.role}
                      onChange={(e) => handleRoleChange(u.id, e.target.value)}
                      className="bg-gray-50 border border-gray-300 text-gray-900 text-xs rounded-lg focus:ring-blue-500 focus:border-blue-500 block w-full p-1"
                    >
                      <option value="admin">Admin</option>
                      <option value="member">Member</option>
                      <option value="viewer">Viewer</option>
                      <option value="guest">Guest</option>
                    </select>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="bg-white p-6 rounded-xl shadow-md border border-gray-200">
        <h2 className="text-xl font-semibold mb-4">Data Migration (Local to Cloud)</h2>
        <div className="space-y-4">
          <div className="flex items-center space-x-4">
            <span className="font-medium">Status:</span>
            <span className={`px-3 py-1 rounded-full text-sm font-bold uppercase ${
              migrationStatus?.status === 'running' ? 'bg-blue-100 text-blue-800' :
              migrationStatus?.status === 'completed' ? 'bg-green-100 text-green-800' :
              'bg-gray-100 text-gray-800'
            }`}>
              {migrationStatus?.status || 'Unknown'}
            </span>
          </div>

          {migrationStatus?.result && (
            <div className="grid grid-cols-2 md:grid-cols-4 gap-4 text-sm bg-gray-50 p-4 rounded-md border border-gray-100">
              <div><strong>Migrated:</strong> {migrationStatus.result.FlowsMigrated}</div>
              <div><strong>Skipped:</strong> {migrationStatus.result.FlowsSkipped}</div>
              <div><strong>Failed:</strong> {migrationStatus.result.FlowsFailed}</div>
              <div><strong>Settings:</strong> {migrationStatus.result.SettingsMoved ? '✅' : '❌'}</div>
              <div className="col-span-full pt-2 border-t border-gray-200 mt-2">
                <strong>Duration:</strong> {(migrationStatus.result.Duration / 1e9).toFixed(2)}s
              </div>
            </div>
          )}

          <div className="pt-4">
            <button
              onClick={handleStartMigration}
              disabled={migrationStatus?.status === 'running' || isLoading}
              className="px-6 py-2 bg-blue-600 text-white rounded-md hover:bg-blue-700 disabled:opacity-50 transition-colors shadow-sm font-medium"
            >
              Start Migration
            </button>
            <p className="mt-2 text-xs text-text-tertiary">
              This process scans the server's local 'data' directory and uploads flows to the database.
            </p>
          </div>
        </div>
      </section>

      {error && (
        <div className="bg-red-50 border-l-4 border-red-400 p-4 text-red-700">
          <p className="font-bold">Error</p>
          <p>{error}</p>
        </div>
      )}
    </div>
  )
}

