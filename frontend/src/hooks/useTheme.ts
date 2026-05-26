import {useCallback, useEffect} from 'react'
import {useSettingsStore} from '@/stores/settingsStore'
import {useUIStore} from '@/stores/uiStore'

export function useTheme() {
    const theme = useSettingsStore(s => s.settings.appearance.theme)
    const resolvedTheme = useUIStore(s => s.resolvedTheme)
    const setResolvedTheme = useUIStore(s => s.setResolvedTheme)
    const updateAppearance = useSettingsStore(s => s.updateAppearance)

    useEffect(() => {
        const resolved = resolveTheme(theme)
        document.documentElement.dataset.theme = resolved
        setResolvedTheme(resolved)
        try { localStorage.setItem('pad-theme', resolved) } catch (_e) { /* localStorage unavailable */ }
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
        const next = resolvedTheme === 'dark' ? 'light' : 'dark'
        updateAppearance({theme: next as 'dark' | 'light'})
    }, [resolvedTheme, updateAppearance])

    return {theme, resolvedTheme, toggleTheme}
}

function resolveTheme(theme: 'dark' | 'light' | 'system'): 'dark' | 'light' {
    if (theme !== 'system') return theme
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
}
