import { useLayoutEffect } from '../ui.js';

// A layout effect, so a page switch right after typing already sees the input. The cleanup clears the key it set before
// the next run reports again (a new key, a new onDirty or unmount).
export function useDirty(onDirty, key, dirty) {
  useLayoutEffect(() => {
    onDirty(key, dirty);
    return () => onDirty(key, false);
  }, [onDirty, key, dirty]);
}
