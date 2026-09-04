import '@testing-library/jest-dom'
// Real i18n (sync init, bundled English resources): components render the
// same English strings tests assert on, and `useTranslation` works without
// per-test boilerplate.
import '../i18n'

// jsdom in this environment exposes localStorage/sessionStorage as inert empty
// objects. Stores (e.g. zustand persist in orgStore) touch storage at import
// time, so install a functional in-memory implementation before test modules
// load. Individual tests may still stub their own via vi.stubGlobal.
class MemoryStorage implements Storage {
  private data = new Map<string, string>()
  get length() {
    return this.data.size
  }
  clear() {
    this.data.clear()
  }
  getItem(key: string) {
    return this.data.has(key) ? this.data.get(key)! : null
  }
  key(index: number) {
    return [...this.data.keys()][index] ?? null
  }
  removeItem(key: string) {
    this.data.delete(key)
  }
  setItem(key: string, value: string) {
    this.data.set(key, String(value))
  }
}

Object.defineProperty(globalThis, 'localStorage', {value: new MemoryStorage(), writable: true, configurable: true})
Object.defineProperty(globalThis, 'sessionStorage', {value: new MemoryStorage(), writable: true, configurable: true})

// jsdom implements no layout, so Element.prototype.scrollIntoView is absent.
// Several components call it to keep the active row visible (CommandPalette,
// GlobalSearchOverlay, ChatMessageList); without this stub, merely rendering
// them throws "scrollIntoView is not a function" from inside an effect.
if (typeof Element !== 'undefined' && !Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = function scrollIntoView() {}
}
