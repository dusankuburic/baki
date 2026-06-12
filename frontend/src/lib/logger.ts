const isDev = import.meta.env.DEV

export const logger = {
  warn: (...args: unknown[]) => {
    if (isDev) console.warn(...args)
  },
  error: (...args: unknown[]) => {
    console.error(...args)
  },
}
