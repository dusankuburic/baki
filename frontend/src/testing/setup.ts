import '@testing-library/jest-dom'
import '../i18n'

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

if (typeof Element !== 'undefined' && !Element.prototype.scrollIntoView) {
  Element.prototype.scrollIntoView = function scrollIntoView() {}
}
