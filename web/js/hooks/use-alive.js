import { useRef, useLayoutEffect, useCallback } from '../ui.js';

// A layout effect: Preact may run a plain effect's cleanup only after the next paint, which would let a late answer through.
export function useAlive() {
  const alive = useRef(true);
  useLayoutEffect(() => { alive.current = true; return () => { alive.current = false; }; }, []);
  return useCallback(() => alive.current, []);
}
