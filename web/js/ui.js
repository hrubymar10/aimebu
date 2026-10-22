// Import Preact only through this module: a second copy of it would break hooks.
import { h, render, Fragment, cloneElement, createContext } from 'preact';
import htm from 'htm';

export { h, render, Fragment, cloneElement, createContext };
export { useState, useEffect, useLayoutEffect, useRef, useMemo, useCallback, useReducer, useContext } from 'preact/hooks';
export const html = htm.bind(h);
