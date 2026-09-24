// isImeKeydown reports a keydown that belongs to an IME composition (CJK
// input). Two shapes: the event fired while composing (`isComposing`), or —
// WebKit, which delivers the candidate-confirming Enter AFTER compositionend
// with isComposing already false — it carries the legacy 229 "process"
// keyCode. Such a key must not send, pick a menu item, commit a rename or
// stop a reply. Accepts a React synthetic event or a native KeyboardEvent.
export function isImeKeydown(e: {
  isComposing?: boolean
  keyCode?: number
  nativeEvent?: {isComposing?: boolean; keyCode?: number}
}): boolean {
  return (
    e.isComposing === true || e.nativeEvent?.isComposing === true || e.keyCode === 229 || e.nativeEvent?.keyCode === 229
  )
}
