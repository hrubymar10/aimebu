import { setGlyph } from './icon-button.js';
import { renderMarkdown } from './markdown.js';

// `mention` ("@author") is put in front of a proposed answer, as v1 does, so the reply reads as an answer to that person; `compose(text)` puts
// the text in the composer, with the reply link, instead of sending it (Shift-click). Without it a Shift-click sends like any click.
export function answerControls(item, submit, { mention = '', compose = null } = {}) {
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
  // A proposed answer is a button: a click sends it (as "@author answer", a reply to the message); Shift-click puts it in the composer with
  // the reply link, to change before sending. Anything else is typed in the composer as a reply.
  for (const value of item.proposed_answers || (item.open_questions?.length ? [] : item.options || [])) {
    const text = mention ? `${mention} ${value}` : value;
    const button = document.createElement('button');
    button.type = 'button'; button.className = 'proposed-answer-btn'; button.textContent = value;
    button.title = compose ? 'Send this answer (Shift-click to edit it in the composer first)' : 'Send this answer';
    button.onclick = event => { if (event.shiftKey && compose) compose(text); else send(wrap, submit, text); };
    wrap.append(button);
  }
  return wrap;
}

// What a question or proposed answers on a message look like once nothing is open for them: the same buttons and question count, all
// disabled, kept under the message so the history reads as it did. An answered one says "Answered"; any other is marked as no
// longer pending, because nothing can answer it from here (imported history, or a question that was closed unanswered).
export function answerHistory(message, answered) {
  const wrap = document.createElement('div');
  wrap.className = 'answer-history'; wrap.dataset.answered = String(answered);
  const note = answered ? 'Answered' : 'No longer pending';
  const disabled = (button, text, className = 'proposed-answer-btn') => { button.type = 'button'; button.className = className; button.disabled = true; button.title = note; button.textContent = text; return button; };
  if (message.open_questions?.length) {
    const open = document.createElement('button'); disabled(open, 'Open Questions', 'btn');
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
      if (choice === 'other') { other.focus(); changed(); return; }
      // Picking an option moves on: to the next question, and from the last one to the review once every question has an answer.
      saveAnswers(item); onChange?.();
      if (qIndex < total - 1) go(qIndex + 1);
      else if (answeredCount(answers) === total) go(total);
      else render();
    };
    question.options.forEach((option, optionIndex) => {
      const label = document.createElement('label');
      const radio = document.createElement('input'); radio.type = 'radio'; radio.name = `question-${qIndex}`;
      radio.value = String(optionIndex); radio.checked = answers[qIndex].choice === String(optionIndex);
      radio.onchange = () => choose(String(optionIndex));
      const text = document.createElement('span'); text.className = 'open-question-option'; const letter = document.createElement('strong'); letter.textContent = `${String.fromCharCode(97 + optionIndex)})`; text.append(letter, ` ${option}`);
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
