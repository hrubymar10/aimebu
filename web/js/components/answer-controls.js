import { html, render, useState, useRef, useLayoutEffect } from '../ui.js';
import { IconButton } from './icon-button.js';
import { renderMarkdown } from '../markdown.js';

// The answers under a question: the proposed answers as buttons, Open Questions, and (in the inbox) a field for an answer of one's own; and what they look like once nothing is
// open for them. `mention` ("@author") is put in front of a proposed answer, as v1 does, so the reply reads as an answer to that person; `compose(text)` puts the text in the
// composer, with the reply link, instead of sending it (Shift-click). Without it a Shift-click sends like any click. With `freeAnswer` (the inbox, which has no composer) a compact
// "Answer…" button under the chips opens a small text field. `className` is added to the root (a message list row's slot), `disabled` switches every control off from outside, and
// the rest of the props (data attributes) go on the root.
export function AnswerControls({ item, submit, mention = '', compose = null, freeAnswer = false, disabled = false, className = '', ...rest }) {
  const [pending, setPending] = useState(false), [error, setError] = useState(''), [free, setFree] = useState(false), [, refresh] = useState(0);
  const field = useRef(null), form = useRef(null);
  // The newest item and submit of the controls on the page are what an open dialog uses when it sends, and the count is drawn again when the dialog changes an answer.
  useLayoutEffect(() => {
    if (!item.open_questions?.length) return undefined;
    const key = draftKey(item), entry = { item, submit, compose: compose && (text => compose(mention ? `${mention} ${text}` : text)), refresh: () => refresh(count => count + 1) };
    live.set(key, [...(live.get(key) || []), entry]);
    return () => { live.set(key, (live.get(key) || []).filter(other => other !== entry)); settleLive(key); };
  });
  useLayoutEffect(() => { if (free) field.current?.focus(); }, [free]);
  const off = pending || disabled;
  const send = async value => {
    if (!value || pending) return;
    setPending(true);
    try { if (await submit(value) === false) throw new Error('Could not send answer'); }
    catch (failure) { setPending(false); setError(failure.message || 'Could not send answer'); }
  };
  const values = item.proposed_answers || (item.open_questions?.length ? [] : item.options || []);
  const count = item.open_questions?.length ? `${answeredCount(loadAnswers(item))}/${item.open_questions.length}` : '';
  return html`<div class=${`answer-controls${className}`} ...${rest}>
    ${item.open_questions?.length ? html`<button type="button" class="btn open-questions-open" disabled=${off} onClick=${() => openQuestions(item, !!compose)}><span>Open Questions</span>${' '}<span class="open-questions-count">${count}</span></button>` : null}
    ${values.map(value => {
      const text = mention ? `${mention} ${value}` : value;
      return html`<button type="button" class="btn proposed-answer-btn" key=${value} disabled=${off} title=${compose ? 'Send this answer (Shift-click to edit it in the composer first)' : 'Send this answer'}
        onClick=${event => { if (event.shiftKey && compose) compose(text); else send(text); }}>${value}</button>`;
    })}
    ${freeAnswer && item.free_text ? html`<button type="button" class="btn proposed-answer-btn answer-free-open" aria-expanded=${String(free)} title="Write an answer of your own" disabled=${off} onClick=${() => setFree(!free)}>Answer…</button>
      <form class="answer-other" ref=${form} hidden=${!free} onSubmit=${event => { event.preventDefault(); send(field.current.value.trim()); }}>
        <textarea required rows="2" placeholder="Your answer" aria-label="Your answer" ref=${field} disabled=${off} onKeyDown=${event => { if (event.key === 'Enter' && (event.ctrlKey || event.metaKey) && !event.isComposing) { event.preventDefault(); form.current.requestSubmit(); } }}></textarea>
        <button type="submit" class="btn btn-primary" disabled=${off}>Send answer</button></form>` : null}
    ${error ? html`<p class="answer-error" role="alert">${error}</p>` : null}
  </div>`;
}

// A resolved question points to the person's reply. A closed one has a single quiet status; a superseded ask has no history line.
export function AnswerHistory({ message, answered, className = '' }) {
  if (!answered && message.answer_state === 'superseded') return null;
  if (!answered) return html`<div class=${`answer-history${className}`} data-answered="false"><span class="answer-history-status">No longer pending</span></div>`;
  const chosen = message.answer_body || '';
  const plain = chosen.replace(/^@[a-z0-9][a-z0-9_-]*(?:@[a-z0-9_.-]+)?\s+/i, '').replace(/\s+/g, ' ').trim();
  const choice = (message.proposed_answers || []).filter(value => plain.startsWith(value) && (plain.length === value.length || /[\s\p{P}]/u.test(plain[value.length]))).sort((a, b) => b.length - a.length)[0];
  const summary = message.open_questions?.length
    ? `Answered by ${message.answer_author_name || 'someone'} (${message.open_questions.length}/${message.open_questions.length})`
    : `Answered by ${message.answer_author_name || 'someone'}: ${choice || [...plain].slice(0, 60).join('')}${[...plain].length > 60 && !choice ? '…' : ''}`;
  const content = html`<span class="icon-mask icon-mask-check" aria-hidden="true"></span><span>${summary}</span>`;
  const no = message.answer_message_no;
  return html`<div class=${`answer-history${className}`} data-answered="true">
    ${no ? html`<a class="msg-ref answer-history-link" href=${`#${no}`} data-no=${String(no)} title=${`Go to reply #${no}`}>${content}</a>`
      : html`<span class="answer-history-link">${content}</span>`}
  </div>`;
}

// Open Questions drafts live per inbox item (or per question set when the item has no id) so closing the dialog, or the card being drawn again, never loses answers. They are
// mirrored to sessionStorage so a reload keeps them too, and dropped once the answers are sent or the questions change.
const drafts = new Map();

function draftKey(item) { return String(item.id ?? 'questions'); }
function signature(item) { return item.open_questions.map(question => `${question.question}\u0000${(question.options || []).join('\u0001')}`).join('\u0002'); }

function loadAnswers(item) {
  const key = draftKey(item), sig = signature(item);
  let draft = drafts.get(key);
  if (!draft) {
    try { draft = JSON.parse(sessionStorage.getItem(`aimebu2.oq.${key}`) || 'null') || undefined; } catch { draft = undefined; }
  }
  if (!draft || draft.sig !== sig || !Array.isArray(draft.answers) || draft.answers.length !== item.open_questions.length) {
    draft = { sig, answers: item.open_questions.map(() => ({ choice: null, text: '' })) };
  }
  drafts.set(key, draft);
  return draft.answers;
}

function saveAnswers(item) {
  const draft = drafts.get(draftKey(item));
  if (!draft) return;
  try { sessionStorage.setItem(`aimebu2.oq.${draftKey(item)}`, JSON.stringify(draft)); } catch { /* storage unavailable: keep the in-memory draft */ }
}

function clearAnswers(item) {
  drafts.delete(draftKey(item));
  try { sessionStorage.removeItem(`aimebu2.oq.${draftKey(item)}`); } catch { /* ignore */ }
}

function isAnswered(answer) {
  return answer.choice !== null && (answer.choice !== 'other' || answer.text.trim() !== '');
}

function answeredCount(answers) { return answers.filter(isAnswered).length; }

function answerLine(question, index, answer) {
  if (answer.choice === 'other') return `Q${index + 1}) other) ${answer.text.trim()}`;
  const optionIndex = Number(answer.choice);
  return `Q${index + 1}) ${String.fromCharCode(97 + optionIndex)}) ${question.options[optionIndex]}`;
}

// A question's description is Markdown, drawn by the one renderer the chat uses.
function Description({ source }) {
  const host = useRef(null);
  useLayoutEffect(() => { host.current.replaceChildren(renderMarkdown(source)); }, [source]);
  return html`<div class="answer-description" ref=${host}></div>`;
}

// The dialog is a root of its own on the page body, keyed by the item, not a child of the controls: the controls are drawn again by live things (a reaction, a receipt, a
// new revision of the item) and a person filling in the questions must not lose the dialog under their hands. What it needs from the controls is looked up when it needs it.
const live = new Map(), dialogs = new Map();
const latest = key => (live.get(key) || []).at(-1);
const changedFor = key => { for (const entry of live.get(key) || []) entry.refresh(); };
// When the last controls of an item are gone for good (answered, removed, the row left the page) the dialog goes with them, and the key is forgotten: it could only send through
// controls that no longer exist, and an old revision's. A row drawn again unmounts and mounts its controls in one go, so this looks again once the page has drawn.
function settleLive(key) {
  requestAnimationFrame(() => {
    if ((live.get(key) || []).length) return;
    live.delete(key);
    dialogs.get(key)?.close();
  });
}

function openQuestions(item, canCompose) {
  const key = draftKey(item);
  if (dialogs.has(key)) return;
  const host = document.createElement('div'); document.body.append(host);
  const close = () => { if (dialogs.get(key)?.host !== host) return; render(null, host); host.remove(); dialogs.delete(key); changedFor(key); };
  dialogs.set(key, { host, close });
  render(html`<${OpenQuestions} item=${item} submit=${text => latest(key)?.submit(text) ?? false}
    composer=${() => (live.get(key) || []).findLast(control => control.compose)?.compose}
    canCompose=${canCompose} onChange=${() => changedFor(key)} onClose=${close} />`, host);
}

function OpenQuestions({ item, submit, composer, canCompose, onChange, onClose }) {
  const questions = item.open_questions, total = questions.length;
  const answersRef = useRef(null);
  if (!answersRef.current) answersRef.current = loadAnswers(item);
  const answers = answersRef.current;
  // Resume at the first unanswered question, or on the review step when done.
  const [index, setIndex] = useState(() => { const first = answers.findIndex(answer => !isAnswered(answer)); return first < 0 ? total : first; });
  const [error, setError] = useState(''), [pending, setPending] = useState(false), [, bump] = useState(0);
  const dialog = useRef(null), arrowing = useRef(false), others = useRef([]);
  useLayoutEffect(() => { dialog.current.showModal(); }, []);
  const go = target => { setIndex(Math.max(0, Math.min(total, target))); setError(''); };
  const changed = () => { saveAnswers(item); onChange?.(); bump(count => count + 1); };
  // Moving between the options with the arrow keys changes the choice too, but that is looking around, not answering: only a click, Enter or Space picks and moves on.
  const choose = (qIndex, choice) => {
    answers[qIndex].choice = choice;
    if (choice === 'other') { others.current[qIndex]?.focus(); changed(); return; }
    saveAnswers(item); onChange?.();
    if (arrowing.current) { bump(count => count + 1); return; }
    // Picking an option moves on: to the next question, and from the last one to the review once every question has an answer.
    if (qIndex < total - 1) go(qIndex + 1);
    else if (answeredCount(answers) === total) go(total);
    else bump(count => count + 1);
  };
  const count = answeredCount(answers), all = count === total, onReview = index === total;
  const submitAll = async (event, edit = false) => {
    event.preventDefault();
    if (pending) return;
    const missing = answers.findIndex(answer => !isAnswered(answer));
    if (missing >= 0) { go(missing); setError(`Answer question ${missing + 1} before sending.`); return; }
    const text = questions.map((question, qIndex) => answerLine(question, qIndex, answers[qIndex])).join('\n');
    if (edit) {
      const compose = composer();
      if (!compose) setError('The room composer is no longer available.');
      else {
        dialog.current.addEventListener('close', () => compose(text), { once: true });
        dialog.current.close();
      }
      return;
    }
    setPending(true);
    try {
      if (await submit(text) === false) throw new Error('Could not send answers');
      clearAnswers(item);
      dialog.current.close();
    } catch (failure) { setPending(false); setError(failure.message || 'Could not send answers'); }
  };
  return html`<dialog class="answer-dialog open-questions-dialog dialog-card dialog-card-primary" id="open-questions-dialog" ref=${dialog} onClose=${onClose}>
    <form noValidate onSubmit=${submitAll}>
      <h2>Open Questions</h2>
      <p class="open-questions-subtitle" aria-live="polite">${onReview ? `Review ${count}/${total} answers` : `Question ${index + 1} of ${total}`}</p>
      <div class="open-questions-steps">
        ${questions.map((_, stepIndex) => html`<button type="button" key=${stepIndex} class=${['row-btn', 'open-questions-step', stepIndex === index ? 'active' : '', isAnswered(answers[stepIndex]) ? 'answered' : ''].filter(Boolean).join(' ')} aria-label=${`Question ${stepIndex + 1}`} aria-current=${stepIndex === index ? 'step' : 'false'} onClick=${() => go(stepIndex)}>${String(stepIndex + 1)}</button>`)}
        <button type="button" class=${['row-btn', 'open-questions-step', 'open-questions-review-step', onReview ? 'active' : '', all ? 'answered' : ''].filter(Boolean).join(' ')} aria-label="Review and send answers" disabled=${!all} onClick=${() => go(total)}>Send</button>
      </div>
      ${questions.map((question, qIndex) => html`<fieldset class="open-question-sheet" key=${qIndex} hidden=${qIndex !== index}
        onKeyDown=${event => { arrowing.current = event.key.startsWith('Arrow'); }} onKeyUp=${() => { arrowing.current = false; }}>
        <legend>${`Q${qIndex + 1}) ${question.question}`}</legend>
        ${question.description ? html`<${Description} source=${question.description} />` : null}
        ${question.options.map((option, optionIndex) => html`<label key=${optionIndex}><input type="radio" name=${`question-${qIndex}`} value=${String(optionIndex)} checked=${answers[qIndex].choice === String(optionIndex)} onChange=${() => choose(qIndex, String(optionIndex))} />
          <span class="open-question-option"><strong>${`${String.fromCharCode(97 + optionIndex)})`}</strong>${` ${option}`}</span></label>`)}
        <label><input type="radio" name=${`question-${qIndex}`} value="other" checked=${answers[qIndex].choice === 'other'} onChange=${() => choose(qIndex, 'other')} />
          <span class="open-question-option">Other</span>
          <input type="text" placeholder="Your answer" aria-label=${`Other answer for question ${qIndex + 1}`} value=${answers[qIndex].text} ref=${node => { others.current[qIndex] = node; }}
            onFocus=${() => { if (answers[qIndex].choice !== 'other') choose(qIndex, 'other'); }}
            onInput=${event => { answers[qIndex].text = event.currentTarget.value; if (answers[qIndex].choice !== 'other') choose(qIndex, 'other'); else changed(); }}
            onKeyDown=${event => { if (event.key === 'Enter') { event.preventDefault(); if (isAnswered(answers[qIndex])) go(qIndex + 1); } }} /></label>
      </fieldset>`)}
      <div class="open-questions-review" hidden=${!onReview}><ol>${questions.map((question, qIndex) => html`<li key=${qIndex}><strong>${`Q${qIndex + 1}) ${question.question} `}</strong>${isAnswered(answers[qIndex]) ? answerLine(question, qIndex, answers[qIndex]).replace(/^Q\d+\) /, '') : 'Not answered'}${' '}<${IconButton} name="edit" label=${`Edit question ${qIndex + 1}`} className="open-questions-edit" onClick=${() => go(qIndex)} /></li>`)}</ol></div>
      <p class="answer-error" role="alert" hidden=${!error}>${error}</p>
      <div class="answer-dialog-actions open-questions-actions">
        <button type="button" class="btn open-questions-close" title="Your answers are kept" onClick=${() => dialog.current.close()}>Close</button>
        <button type="button" class="btn open-questions-back" disabled=${index === 0} onClick=${() => go(index - 1)}>Back</button>
        <span class="open-questions-progress">${`${count}/${total} answered`}</span>
        <button type="button" class="btn btn-primary open-questions-next" hidden=${onReview} disabled=${index === total - 1 && !all} onClick=${() => go(index + 1)}>Next</button>
        <button type="submit" class="btn btn-primary open-questions-send" hidden=${!onReview} disabled=${!all || pending}
          title=${canCompose ? 'Send answers (Shift-click to edit them in the composer first)' : 'Send answers'}
          onClick=${event => { if (event.shiftKey && canCompose) void submitAll(event, true); }}>Send answers</button>
      </div>
    </form>
  </dialog>`;
}
