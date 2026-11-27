// Import Preact only through this module: a second copy of it would break hooks.
import './time-tip.js'; // the tooltip a time carries (data-time-tip) works wherever a page renders one
import { h, render, Fragment, Component, cloneElement, createContext, createPortal } from 'preact';
import htm from 'htm';

export { h, render, Fragment, Component, cloneElement, createContext, createPortal };
export { useState, useEffect, useLayoutEffect, useRef, useMemo, useCallback, useReducer, useContext } from 'preact/hooks';
export { signal, computed, effect, batch, untracked, useSignal, useComputed, useSignalEffect } from '@preact/signals';
export const html = htm.bind(h);
