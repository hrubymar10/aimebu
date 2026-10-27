// Import-free: scripts/test-general-model.cjs runs this module in a vm.

export const THEMES = [['dark', 'Dark'], ['light', 'Light'], ['red-dark', 'Red Dark'], ['red-light', 'Red Light'], ['blue-dark', 'Blue Dark'], ['blue-light', 'Blue Light'],
  ['green-dark', 'Green Dark'], ['green-light', 'Green Light'], ['high-contrast-dark', 'High Contrast Dark'], ['high-contrast-light', 'High Contrast Light']];

export const themeKey = userID => `aimebu2.ui.theme.${userID}`;
export const previewKey = userID => `aimebu2.ui.sidebar_preview.${userID}`;

// A saved theme that is no longer offered falls back to the default.
export const knownTheme = value => (THEMES.some(([id]) => id === value) ? value : 'dark');

// The default theme is the absence of data-theme.
export const themeAttribute = theme => (theme === 'dark' ? null : theme);
