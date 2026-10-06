import {
  LogIn,
  LogOut,
  Upload,
  Save,
  Trash2,
  Activity as ActivityIcon,
  Share2,
  Bot,
  Key,
  Settings,
  UserPlus,
  ShieldAlert,
  ClipboardCheck,
  MessageSquare,
  Users,
  FileText,
  History,
  type LucideIcon,
} from 'lucide-react'

// The icon for an audit action, by its first segment or exact id.
const EXACT: Record<string, LucideIcon> = {
  'auth.login': LogIn,
  'auth.sso_login': LogIn,
  'auth.logout': LogOut,
  'auth.register': UserPlus,
  'auth.login_failure': ShieldAlert,
  'auth.account_lock': ShieldAlert,
  'flow.upload': Upload,
  'flow.save': Save,
  'flow.delete': Trash2,
  'flow.analyze': ActivityIcon,
  'flow.share': Share2,
  'flow.share_link_create': Share2,
  'flow.share_link_revoke': Share2,
  'flow.version_save': History,
  'flow.version_restore': History,
  'finding.triage': ClipboardCheck,
  'finding.comment_add': MessageSquare,
  'finding.comment_delete': MessageSquare,
  'chat.stream': Bot,
  'keys.save': Key,
  'token.create': Key,
  'token.revoke': Key,
  // Azure AI Foundry resources hold credentials; models are what chats run on.
  'azure.connection_save': Key,
  'azure.connection_delete': Key,
}

const BY_PREFIX: Record<string, LucideIcon> = {
  auth: Key,
  flow: FileText,
  finding: ClipboardCheck,
  chat: Bot,
  azure: Bot,
  settings: Settings,
  user: Settings,
  org: Users,
  admin: ShieldAlert,
  policy: ClipboardCheck,
  governance: ShieldAlert,
}

export function actionIcon(action: string): LucideIcon {
  const prefix = action.split('.')[0] ?? ''
  return EXACT[action] ?? BY_PREFIX[prefix] ?? ActivityIcon
}

// actionLabel looks the action up in the translated table (keys are the id
// with dots as underscores) and otherwise humanises the id: "flow.baseline_set"
// → "Flow baseline set".
export function actionLabel(table: Record<string, string>, action: string): string {
  const key = action.replace(/[.-]/g, '_')
  const hit = table[key]
  if (hit) return hit
  const words = action.replace(/[._-]+/g, ' ').trim()
  return words ? words.charAt(0).toUpperCase() + words.slice(1) : action
}
