import { createContext, useContext } from '../../ui.js';

// The onboarding step embeds the page next to Settings' own copy, so its element ids carry a prefix.
export const IdPrefix = createContext('');
export const useIdPrefix = () => useContext(IdPrefix);
