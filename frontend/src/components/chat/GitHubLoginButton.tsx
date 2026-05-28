import {providersApi} from '@/api'
import DeviceAuthLoginButton, {type DeviceAuthProvider} from './DeviceAuthLoginButton'

interface Props {
  onAuthComplete?: () => void
}

const githubProvider: DeviceAuthProvider = {
  name: 'GitHub',
  label: 'Sign in with GitHub',
  buttonClassName: 'bg-[#24292e] hover:bg-[#2f363d]',
  getUser: () => providersApi.getGitHubUser(),
  startAuth: () => providersApi.startGitHubAuth(),
  pollAuth: (deviceCode: string) => providersApi.pollGitHubAuth(deviceCode),
  revokeAuth: () => providersApi.revokeGitHubAuth(),
}

export default function GitHubLoginButton({onAuthComplete}: Props) {
  return <DeviceAuthLoginButton provider={githubProvider} onAuthComplete={onAuthComplete} />
}
