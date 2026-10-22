import { useLayoutEffect } from '../ui.js';

// A layout effect, so a page switch right after typing already sees the input.
export function useDirty(onDirty, key, dirty) {
  useLayoutEffect(() => {
    onDirty(key, dirty);
    return () => onDirty(key, false);
  }, [onDirty, key, dirty]);
}
