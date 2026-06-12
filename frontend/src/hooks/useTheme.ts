import {useCallback, useEffect} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'
import type {ThemeMode} from '@/types/domain'

export function useTheme() {
    const theme = useSettingsStore(s => s.settings.appearance.theme)
    const resolvedTheme = useUIStore(s => s.resolvedTheme)
    const setResolvedTheme = useUIStore(s => s.setResolvedTheme)
    const updateAppearance = useSettingsStore(s => s.updateAppearance)

    useEffect(() => {
        const resolved = resolveTheme(theme)
        document.documentElement.dataset.theme = resolved
        if (resolved !== 'system') setResolvedTheme(resolved)
        try { localStorage.setItem('pad-theme', resolved) } catch { /* localStorage unavailable */ }
    }, [theme, setResolvedTheme])

    useEffect(() => {
        if (theme !== 'system') return
        const mq = window.matchMedia('(prefers-color-scheme: light)')
        const handler = () => {
            const resolved = mq.matches ? 'light' : 'dark'
            document.documentElement.dataset.theme = resolved
            setResolvedTheme(resolved)
        }
        mq.addEventListener('change', handler)
        return () => mq.removeEventListener('change', handler)
    }, [theme, setResolvedTheme])

    const toggleTheme = useCallback(() => {
        const next: ThemeMode = resolvedTheme === 'light' ? 'dark' : 'light'
        updateAppearance({theme: next})
    }, [resolvedTheme, updateAppearance])

    return {theme, resolvedTheme, toggleTheme}
}

function resolveTheme(theme: ThemeMode): ThemeMode {
    if (theme === 'system')
        return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
    return theme
}
