import '@testing-library/jest-dom'

// jsdom in this environment exposes localStorage/sessionStorage as inert empty
// objects. Stores (e.g. zustand persist in orgStore) touch storage at import
// time, so install a functional in-memory implementation before test modules
// load. Individual tests may still stub their own via vi.stubGlobal.
class MemoryStorage implements Storage {
    private data = new Map<string, string>()
    get length() { return this.data.size }
    clear() { this.data.clear() }
    getItem(key: string) { return this.data.has(key) ? this.data.get(key)! : null }
    key(index: number) { return [...this.data.keys()][index] ?? null }
    removeItem(key: string) { this.data.delete(key) }
    setItem(key: string, value: string) { this.data.set(key, String(value)) }
}

Object.defineProperty(globalThis, 'localStorage', { value: new MemoryStorage(), writable: true, configurable: true })
Object.defineProperty(globalThis, 'sessionStorage', { value: new MemoryStorage(), writable: true, configurable: true })
