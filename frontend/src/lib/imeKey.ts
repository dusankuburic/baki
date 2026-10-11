export function isImeKeydown(e: {
  isComposing?: boolean
  keyCode?: number
  nativeEvent?: {isComposing?: boolean; keyCode?: number}
}): boolean {
  return (
    e.isComposing === true || e.nativeEvent?.isComposing === true || e.keyCode === 229 || e.nativeEvent?.keyCode === 229
  )
}
