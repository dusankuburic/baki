import type {ThemeMode} from '@/types'

export type ThemeCategory = 'system' | 'dark' | 'light'

export interface ThemeMeta {
  id: ThemeMode
  label: string
  description: string
  category: Exclude<ThemeCategory, 'system'>
}

export const THEME_REGISTRY: ThemeMeta[] = [
  {id: 'dark', label: 'Deep Dark', description: 'The standard high-contrast dark mode.', category: 'dark'},
  {id: 'midnight', label: 'Midnight', description: 'A cool, deep navy palette for night owls.', category: 'dark'},
  {id: 'warm', label: 'Warm Sand', description: 'A soft, sepia-toned dark theme.', category: 'dark'},
  {id: 'tokyo-night', label: 'Tokyo Night', description: 'Deep purples and neon cyan accents.', category: 'dark'},
  {id: 'one-dark', label: 'One Dark', description: 'Classic soft gray with balanced contrast.', category: 'dark'},
  {id: 'dracula', label: 'Dracula', description: 'Vibrant colors on a vampiric purple base.', category: 'dark'},
  {id: 'nord', label: 'Nord', description: 'An elegant arctic-bluish clean aesthetic.', category: 'dark'},
  {
    id: 'gruvbox-dark',
    label: 'Gruvbox Dark',
    description: 'Warm retro earth tones with strong contrast.',
    category: 'dark',
  },
  {
    id: 'catppuccin-mocha',
    label: 'Catppuccin Mocha',
    description: 'Soft pastel dark theme, warm and cozy.',
    category: 'dark',
  },
  {id: 'rose-pine', label: 'Rose Pine', description: 'Muted rose and pine, naturally soft.', category: 'dark'},
  {
    id: 'rose-pine-moon',
    label: 'Rose Pine Moon',
    description: 'A slightly brighter Rose Pine variant.',
    category: 'dark',
  },
  {id: 'github-dark', label: 'GitHub Dark', description: 'The familiar GitHub dark default.', category: 'dark'},
  {id: 'kanagawa', label: 'Kanagawa', description: 'Inspired by The Great Wave — calm blues.', category: 'dark'},
  {
    id: 'everforest',
    label: 'Everforest',
    description: 'Calm green-based, low-stress for long sessions.',
    category: 'dark',
  },
  {id: 'light', label: 'Clean Light', description: 'Bright and airy for high-light environments.', category: 'light'},
  {
    id: 'gruvbox-light',
    label: 'Gruvbox Light',
    description: 'Retro cream and brown for daytime coding.',
    category: 'light',
  },
  {
    id: 'catppuccin-latte',
    label: 'Catppuccin Latte',
    description: 'Gentle pastel light theme, easy on the eyes.',
    category: 'light',
  },
  {
    id: 'rose-pine-dawn',
    label: 'Rose Pine Dawn',
    description: 'Rose Pine for daytime — warm paper tones.',
    category: 'light',
  },
  {id: 'github-light', label: 'GitHub Light', description: 'The familiar GitHub light default.', category: 'light'},
]

export const DARK_THEMES = THEME_REGISTRY.filter(t => t.category === 'dark')
export const LIGHT_THEMES = THEME_REGISTRY.filter(t => t.category === 'light')

/**
 * Whether a resolved theme is a light one, by the same registry the theme picker and the `dark:`
 * variant in `index.css` use. Components that must tell a library (React Flow's `colorMode`) which
 * scheme it is in ask here instead of keeping their own list.
 */
export function isLightTheme(theme: ThemeMode | string): boolean {
  return THEME_REGISTRY.find(t => t.id === theme)?.category === 'light'
}

export const SYSTEM_THEME = {
  id: 'system' as ThemeMode,
  label: 'System',
  description: 'Automatically follow your OS preferences.',
}
