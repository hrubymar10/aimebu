import { html, useEffect, useLayoutEffect, useMemo, useRef, useState } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { HIGHLIGHT_MS, LINES_MAX, LINES_MIN, atBottom, highlightParts, matchLabel, stepMatch } from './log-model.js';

// The text with its matches as <mark> elements. Preact writes every part as a text node or a <mark> with a text node in it, never as HTML,
// so whatever the agent printed stays text.
function LogText({ parts, current, follow, onPinned, textRef, label }) {
  let index = -1;
  return html`<pre class="agent-log-text" ref=${textRef} tabindex="0" role="region" aria-label=${label} onScroll=${event => onPinned(atBottom(event.currentTarget))} data-follow=${follow ? 'on' : 'off'}>${parts.map((part, key) => {
    if (!part.mark) return part.text;
    index++;
    return html`<mark key=${key} class=${index === current ? 'agent-log-mark current' : 'agent-log-mark'}>${part.text}</mark>`;
  })}</pre>`;
}

// The popup over one agent's logs: how many lines, Follow (the view sticks to the end; scrolling up leaves it where it is), a highlight
// field (Enter and Shift+Enter step through the matches), Download and Close. Escape closes it, as every dialog does.
export function LogDialog({ store, host }) {
  const state = store.state.value;
  const [linesDraft, setLinesDraft] = useState(null); // what is typed in the lines field, until the store takes it
  const [query, setQuery] = useState(''), [applied, setApplied] = useState('');
  const [current, setCurrent] = useState(-1), [pinned, setPinned] = useState(true);
  const textRef = useRef(null), throttle = useRef({ timer: 0, last: 0 });
  const agentID = state?.agentID || '';
  // A new agent starts with an empty highlight and the default lines.
  useEffect(() => { setLinesDraft(null); setQuery(''); setApplied(''); setCurrent(-1); setPinned(true); }, [agentID]);
  useEffect(() => { setLinesDraft(null); }, [state?.lines]);
  useEffect(() => () => clearTimeout(throttle.current.timer), []);
  const found = useMemo(() => highlightParts(state?.text || '', applied), [state?.text, applied]);
  useEffect(() => { setCurrent(-1); }, [applied]);
  // Follow keeps the end in view, unless the person scrolled up.
  useLayoutEffect(() => { const text = textRef.current; if (text && state?.follow && pinned) text.scrollTop = text.scrollHeight; }, [state?.text, state?.follow, pinned]);
  useLayoutEffect(() => { if (current >= 0) textRef.current?.querySelectorAll('mark')[current]?.scrollIntoView({ block: 'nearest' }); }, [current]);
  if (!state) return null;
  const name = host.agentName(state.agentID);
  // Typing runs the highlight again at most every HIGHLIGHT_MS: the first key at once, the rest when the wait is over.
  const typed = value => {
    setQuery(value);
    const held = throttle.current;
    clearTimeout(held.timer);
    const wait = Math.max(0, held.last + HIGHLIGHT_MS - Date.now());
    if (!wait) { held.last = Date.now(); setApplied(value); return; }
    held.timer = setTimeout(() => { held.last = Date.now(); setApplied(value); }, wait);
  };
  const findKey = event => {
    if (event.key !== 'Enter') return;
    event.preventDefault();
    // A query still waiting for its run is applied first, so Enter never steps through the matches of an older one.
    if (applied !== query) { clearTimeout(throttle.current.timer); throttle.current.last = Date.now(); setApplied(query); return; }
    setCurrent(stepMatch(current, found.count, event.shiftKey ? -1 : 1));
  };
  const commitLines = event => { store.setLines(event.currentTarget.value); setLinesDraft(null); };
  const download = () => {
    const url = URL.createObjectURL(new Blob([state.text], { type: 'text/plain;charset=utf-8' }));
    const link = document.createElement('a'); link.href = url; link.download = `${host.agentSlug(state.agentID) || state.agentID}-logs.txt`; document.body.append(link); link.click(); link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  const follow = event => { setPinned(true); store.setFollow(event.currentTarget.checked); };
  return html`<header class="agent-log-head">
      <h2 id="agent-log-title">${`Logs · ${name}`}</h2>
      <div class="agent-log-controls">
        <label class="agent-log-lines">Lines <input type="number" inputmode="numeric" min=${LINES_MIN} max=${LINES_MAX} step="50" value=${linesDraft ?? String(state.lines)} aria-label="Number of lines"
          onInput=${event => setLinesDraft(event.currentTarget.value)} onChange=${commitLines} onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); commitLines(event); } }} /></label>
        <label class="agent-log-follow"><input type="checkbox" checked=${state.follow} onChange=${follow} /> Follow</label>
        <span class="agent-log-find"><input type="text" class="agent-log-query" enterkeyhint="search" autocomplete="off" spellcheck="false" placeholder="Highlight" aria-label="Highlight text" value=${query} onInput=${event => typed(event.currentTarget.value)} onKeyDown=${findKey} />
          <span class="agent-log-count" aria-live="polite">${matchLabel(found, applied, current)}</span></span>
        <span class="agent-log-stream" data-slot="stream" hidden></span>
        <span class="agent-log-tools"><${IconButton} name="refresh" label="Read the logs again" className="agent-log-refresh" disabled=${state.loading} onClick=${() => store.read()} />
          <${IconButton} name="arrow-down" label="Download logs" className="agent-log-download" disabled=${!state.text} onClick=${download} />
          <${IconButton} name="close" label="Close logs" className="agent-log-close" onClick=${() => host.closeLogs()} /></span>
      </div>
    </header>
    <div class="agent-log-summary" data-slot="summary"></div>
    <p class="agent-log-note">The end of what this agent's harness printed to stderr, kept on its machine for 14 days. It can contain secrets from tool output.</p>
    <${LogText} parts=${found.parts} current=${current} follow=${state.follow} onPinned=${setPinned} textRef=${textRef} label=${`Logs of ${name}`} />
    <p class="agent-log-status" role="status">${[state.loading && !state.text ? 'Loading…' : state.status, state.follow && !pinned ? 'Scrolled up: Follow keeps reading but leaves the view where it is.' : ''].filter(Boolean).join(' ')}</p>`;
}
