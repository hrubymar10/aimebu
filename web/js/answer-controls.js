import { iconButton, setGlyph } from './icon-button.js';
import { renderMarkdown } from './markdown.js';

export function answerControls(item, submit) {
  const wrap = document.createElement('div');
  wrap.className = 'answer-controls';
  if (item.open_questions?.length) {
    const open = document.createElement('button');
    open.type = 'button'; open.className = 'btn open-questions-open';
    const label = document.createElement('span'); label.textContent = 'Open Questions';
    const count = document.createElement('span'); count.className = 'open-questions-count';
    const refreshCount = () => { count.textContent = `${answeredCount(loadAnswers(item))}/${item.open_questions.length}`; };
    open.append(label, ' ', count); refreshCount();
    open.onclick = () => openQuestions(item, submit, refreshCount);
    wrap.append(open);
  }
  // A proposed answer is a button that sends it as it stands. Its pencil, or a Shift-click on the answer, opens it in an editor first, so the
  // person can change it before sending; the edited text goes out the same way (a reply that resolves the item).
  let editor = null, lastChoice = null;
  // The editor sits right under the answers, ahead of the free-text "Other" form when there is one.
  const edit = (value, origin) => {
    if (!editor) { editor = answerEditor(wrap, submit); lastChoice.after(editor.form); }
    editor.open(value, origin);
  };
  for (const value of item.proposed_answers || (item.open_questions?.length ? [] : item.options || [])) {
    const choice = document.createElement('span'); choice.className = 'answer-choice';
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'btn'; button.textContent = value; button.title = 'Send this answer (Shift-click to edit it first)';
    button.onclick = event => { if (event.shiftKey) edit(value, button); else send(wrap, submit, value); };
    const pencil = iconButton('edit', `Edit \u201c${value}\u201d before sending`, { className: 'answer-edit-open' });
    pencil.onclick = () => edit(value, button);
    choice.append(button, pencil);
    wrap.append(choice); lastChoice = choice;
  }
  if (item.free_text && (!item.open_questions?.length || item.proposed_answers?.length)) {
    const form = document.createElement('form');
    form.className = 'answer-other';
    const input = document.createElement('textarea');
    input.required = true; input.rows = 2; input.placeholder = 'Other answer';
    input.setAttribute('aria-label', 'Other answer');
    const button = document.createElement('button');
    button.type = 'submit'; button.className = 'btn btn-primary'; button.textContent = 'Send answer';
    form.append(input, button);
    form.onsubmit = event => { event.preventDefault(); send(wrap, submit, input.value.trim()); };
    wrap.append(form);
  }
  return wrap;
}

// The editor under the answers: the chosen answer in a text box to change, sent with its button or Ctrl/Cmd+Enter, closed with the X or Esc
// (focus goes back to the answer it came from). Nothing is stored: closing it drops the text.
function answerEditor(wrap, submit) {
  const form = document.createElement('form'); form.className = 'answer-edit'; form.hidden = true;
  const input = document.createElement('textarea'); input.rows = 3; input.required = true; input.setAttribute('aria-label', 'Edit the answer before sending');
  const actions = document.createElement('div'); actions.className = 'answer-edit-actions';
  const cancel = iconButton('close', 'Close the editor without sending', { className: 'answer-edit-close' });
  const sendButton = document.createElement('button'); sendButton.type = 'submit'; sendButton.className = 'btn btn-primary'; sendButton.textContent = 'Send answer';
  actions.append(cancel, sendButton); form.append(input, actions);
  let origin = null;
  const close = () => { form.hidden = true; input.value = ''; (origin?.isConnected ? origin : null)?.focus(); origin = null; };
  cancel.onclick = close;
  form.onsubmit = event => { event.preventDefault(); send(wrap, submit, input.value.trim()); };
  input.onkeydown = event => {
    if (event.isComposing) return;
    if (event.key === 'Escape') { event.preventDefault(); event.stopPropagation(); close(); }
    else if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) { event.preventDefault(); form.requestSubmit(); }
  };
  return { form, open(value, from) {
    origin = from;
    form.hidden = false; input.value = value; input.focus(); input.setSelectionRange(input.value.length, input.value.length);
  } };
}

// What a question or proposed answers on a message look like once nothing is open for them: the same buttons and question count, all
// disabled, kept under the message so the history reads as it did. An answered one says "Answered"; any other is marked as no
// longer pending, because nothing can answer it from here (imported history, or a question that was closed unanswered).
export function answerHistory(message, answered) {
  const wrap = document.createElement('div');
  wrap.className = 'answer-history'; wrap.dataset.answered = String(answered);
  const note = answered ? 'Answered' : 'No longer pending';
  const disabled = (button, text) => { button.type = 'button'; button.className = 'btn'; button.disabled = true; button.title = note; button.textContent = text; return button; };
  if (message.open_questions?.length) {
    const open = document.createElement('button'); disabled(open, 'Open Questions');
    const count = document.createElement('span'); count.className = 'open-questions-count'; count.textContent = `${message.open_questions.length}`;
    open.append(' ', count); wrap.append(open);
  }
  for (const value of message.proposed_answers || []) wrap.append(disabled(document.createElement('button'), value));
  const status = document.createElement('span'); status.className = 'answer-history-status'; status.textContent = note;
  wrap.append(status);
  return wrap;
}

async function send(wrap, submit, value) {
  if (!value || wrap.dataset.pending) return;
  wrap.dataset.pending = 'true';
  for (const control of wrap.querySelectorAll('button,textarea,input')) control.disabled = true;
  try { if (await submit(value) === false) throw new Error('Could not send answer'); }
  catch (error) {
    delete wrap.dataset.pending;
    for (const control of wrap.querySelectorAll('button,textarea,input')) control.disabled = false;
    let notice = wrap.querySelector('.answer-error');
    if (!notice) { notice = document.createElement('p'); notice.className = 'answer-error'; notice.role = 'alert'; wrap.append(notice); }
    notice.textContent = error.message || 'Could not send answer';
  }
}

// Open Questions drafts live per inbox item (or per question set when the item
// has no id) so closing the dialog, or the card being re-rendered, never loses
// answers. They are mirrored to sessionStorage so a reload keeps them too, and
// dropped once the answers are sent or the questions change.
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

function openQuestions(item, submit, onChange) {
  const questions = item.open_questions;
  const answers = loadAnswers(item);
  const total = questions.length;
  // Resume at the first unanswered question, or on the review step when done.
  let index = answers.findIndex(answer => !isAnswered(answer));
  if (index < 0) index = total;

  const dialog = document.createElement('dialog'); dialog.className = 'answer-dialog open-questions-dialog dialog-card dialog-card-primary'; dialog.id = 'open-questions-dialog';
  const form = document.createElement('form'); form.noValidate = true;
  const title = document.createElement('h2'); title.textContent = 'Open Questions';
  const subtitle = document.createElement('p'); subtitle.className = 'open-questions-subtitle'; subtitle.setAttribute('aria-live', 'polite');
  const steps = document.createElement('div'); steps.className = 'open-questions-steps';
  const stepButtons = questions.map((_, stepIndex) => {
    const step = document.createElement('button'); step.type = 'button'; step.className = 'row-btn open-questions-step';
    step.textContent = String(stepIndex + 1); step.setAttribute('aria-label', `Question ${stepIndex + 1}`);
    step.onclick = () => go(stepIndex);
    return step;
  });
  const reviewStep = document.createElement('button'); reviewStep.type = 'button'; reviewStep.className = 'row-btn open-questions-step open-questions-review-step';
  reviewStep.textContent = 'Send'; reviewStep.setAttribute('aria-label', 'Review and send answers');
  reviewStep.onclick = () => go(total);
  steps.append(...stepButtons, reviewStep);

  const sheets = questions.map((question, qIndex) => {
    const group = document.createElement('fieldset'); group.className = 'open-question-sheet';
    const heading = document.createElement('legend'); heading.textContent = `Q${qIndex + 1}) ${question.question}`;
    group.append(heading);
    if (question.description) { const detail = document.createElement('div'); detail.className = 'answer-description'; detail.append(renderMarkdown(question.description)); group.append(detail); }
    const other = document.createElement('input'); other.type = 'text'; other.placeholder = 'Your answer';
    other.setAttribute('aria-label', `Other answer for question ${qIndex + 1}`);
    other.value = answers[qIndex].text;
    const choose = (choice) => {
      answers[qIndex].choice = choice;
      if (choice === 'other') other.focus();
      changed();
    };
    question.options.forEach((option, optionIndex) => {
      const label = document.createElement('label');
      const radio = document.createElement('input'); radio.type = 'radio'; radio.name = `question-${qIndex}`;
      radio.value = String(optionIndex); radio.checked = answers[qIndex].choice === String(optionIndex);
      radio.onchange = () => choose(String(optionIndex));
      const text = document.createElement('span'); text.className = 'open-question-option'; text.textContent = `${String.fromCharCode(97 + optionIndex)}) ${option}`;
      label.append(radio, text); group.append(label);
    });
    const label = document.createElement('label');
    const radio = document.createElement('input'); radio.type = 'radio'; radio.name = `question-${qIndex}`;
    radio.value = 'other'; radio.checked = answers[qIndex].choice === 'other';
    radio.onchange = () => choose('other');
    // Tapping the text box is choosing Other; a disabled box would swallow the tap on a phone.
    other.onfocus = () => { if (answers[qIndex].choice !== 'other') { radio.checked = true; choose('other'); } };
    other.oninput = () => { answers[qIndex].text = other.value; if (answers[qIndex].choice !== 'other') { radio.checked = true; choose('other'); } else changed(); };
    // Enter in the free-text box moves on instead of submitting half the form.
    other.onkeydown = event => { if (event.key === 'Enter') { event.preventDefault(); if (isAnswered(answers[qIndex])) go(qIndex + 1); } };
    const otherText = document.createElement('span'); otherText.className = 'open-question-option'; otherText.textContent = 'Other';
    label.append(radio, otherText, other); group.append(label);
    return group;
  });

  const review = document.createElement('div'); review.className = 'open-questions-review';
  const reviewList = document.createElement('ol');
  review.append(reviewList);

  const error = document.createElement('p'); error.className = 'answer-error'; error.role = 'alert'; error.hidden = true;
  const back = document.createElement('button'); back.type = 'button'; back.className = 'btn open-questions-back'; back.textContent = 'Back';
  back.onclick = () => go(index - 1);
  const progress = document.createElement('span'); progress.className = 'open-questions-progress';
  const next = document.createElement('button'); next.type = 'button'; next.className = 'btn btn-primary open-questions-next'; next.textContent = 'Next';
  next.onclick = () => go(index + 1);
  const sendButton = document.createElement('button'); sendButton.type = 'submit'; sendButton.className = 'btn btn-primary open-questions-send'; sendButton.textContent = 'Send answers';
  const close = document.createElement('button'); close.type = 'button'; close.className = 'btn open-questions-close'; close.textContent = 'Close'; close.title = 'Your answers are kept';
  close.onclick = () => dialog.close();
  const actions = document.createElement('div'); actions.className = 'answer-dialog-actions open-questions-actions';
  actions.append(close, back, progress, next, sendButton);
  form.append(title, subtitle, steps, ...sheets, review, error, actions);
  dialog.append(form); document.body.append(dialog);

  function changed() { saveAnswers(item); onChange?.(); render(); }
  function go(target) { index = Math.max(0, Math.min(total, target)); error.hidden = true; render(); }
  function render() {
    const count = answeredCount(answers), all = count === total, onReview = index === total;
    subtitle.textContent = onReview ? `Review ${count}/${total} answers` : `Question ${index + 1} of ${total}`;
    stepButtons.forEach((step, stepIndex) => {
      step.classList.toggle('active', stepIndex === index);
      step.classList.toggle('answered', isAnswered(answers[stepIndex]));
      step.setAttribute('aria-current', stepIndex === index ? 'step' : 'false');
    });
    reviewStep.classList.toggle('active', onReview); reviewStep.classList.toggle('answered', all); reviewStep.disabled = !all;
    sheets.forEach((sheet, sheetIndex) => { sheet.hidden = sheetIndex !== index; });
    review.hidden = !onReview;
    reviewList.replaceChildren(...questions.map((question, qIndex) => {
      const line = document.createElement('li');
      const strong = document.createElement('strong'); strong.textContent = `Q${qIndex + 1}) ${question.question} `;
      const edit = document.createElement('button'); edit.type = 'button'; edit.className = 'icon-btn open-questions-edit'; setGlyph(edit, 'edit', `Edit question ${qIndex + 1}`); edit.onclick = () => go(qIndex);
      line.append(strong, isAnswered(answers[qIndex]) ? answerLine(question, qIndex, answers[qIndex]).replace(/^Q\d+\) /, '') : 'Not answered', ' ', edit);
      return line;
    }));
    progress.textContent = `${count}/${total} answered`;
    back.disabled = index === 0;
    next.hidden = onReview; next.disabled = index === total - 1 && !all;
    sendButton.hidden = !onReview; sendButton.disabled = !all || form.dataset.pending === 'true';
  }

  dialog.onclose = () => { dialog.remove(); onChange?.(); };
  form.onsubmit = async event => {
    event.preventDefault();
    if (form.dataset.pending === 'true') return;
    const missing = answers.findIndex(answer => !isAnswered(answer));
    if (missing >= 0) {
      go(missing);
      error.textContent = `Answer question ${missing + 1} before sending.`; error.hidden = false;
      return;
    }
    form.dataset.pending = 'true'; sendButton.disabled = true;
    try {
      if (await submit(questions.map((question, qIndex) => answerLine(question, qIndex, answers[qIndex])).join('\n')) === false) throw new Error('Could not send answers');
      clearAnswers(item);
      dialog.close();
    } catch (failure) {
      delete form.dataset.pending;
      error.textContent = failure.message || 'Could not send answers'; error.hidden = false;
      render();
    }
  };
  render();
  dialog.showModal();
}
