// Import Preact only through this module: a second copy of it would break hooks.
import { h, render, Fragment, Component, cloneElement, createContext } from 'preact';
import htm from 'htm';

export { h, render, Fragment, Component, cloneElement, createContext };
export { useState, useEffect, useLayoutEffect, useRef, useMemo, useCallback, useReducer, useContext } from 'preact/hooks';
export { signal, computed, effect, batch, untracked, useSignal, useComputed, useSignalEffect } from '@preact/signals';
export const html = htm.bind(h);
