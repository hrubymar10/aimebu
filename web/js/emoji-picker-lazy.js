import { reportChunkFailure } from './lazy.js';

// What the chat and the message menus hold in place of emoji-picker.js (a chunk the page fetches the first time a picker is opened). Opening is the person's
// click, so the one fetch it waits for is the only change; closing what was never opened is nothing to do.
let loaded = null, loading = null;

export function openEmojiPicker(options) {
  loading ||= import('./emoji-picker.js').then(module => { loaded = module; return module; }, error => { loading = null; reportChunkFailure(error, 'emoji-picker'); return null; });
  return loading.then(module => module?.openEmojiPicker(options));
}

export function closeEmojiPicker(restoreFocus = true) { loaded?.closeEmojiPicker(restoreFocus); }
