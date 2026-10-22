/**
 * jsdom stand-ins for the browser APIs React Flow needs (a ResizeObserver for its container, a
 * DOMMatrix for d3-zoom's transforms). Call `installReactFlowEnvironment()` at the top of a test
 * file that renders a graph view; the suite's shared setup stays free of them.
 *
 * Node sizes are not faked: the graph views give every node a fixed size and pass it as `measured`,
 * so a rendered graph is laid out in jsdom exactly as it is in a browser.
 */
export function installReactFlowEnvironment(): void {
  if (typeof globalThis === 'undefined') return

  if (!('ResizeObserver' in globalThis)) {
    class TestResizeObserver {
      observe() {}
      unobserve() {}
      disconnect() {}
    }
    Object.defineProperty(globalThis, 'ResizeObserver', {
      value: TestResizeObserver,
      writable: true,
      configurable: true,
    })
  }

  if (!('DOMMatrixReadOnly' in globalThis)) {
    class TestDOMMatrixReadOnly {
      m22: number
      constructor(transform?: string) {
        const scale = transform?.match(/scale\(([0-9.]+)\)/)?.[1]
        this.m22 = scale === undefined ? 1 : Number(scale)
      }
    }
    Object.defineProperty(globalThis, 'DOMMatrixReadOnly', {
      value: TestDOMMatrixReadOnly,
      writable: true,
      configurable: true,
    })
  }
}
