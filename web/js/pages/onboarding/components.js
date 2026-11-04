import { html, useLayoutEffect, useRef } from '../../ui.js';
import { rights } from '../../api.js';
import { prefillFolderOnFocus } from '../../workspace-folder.js';
import { PRINCIPLES } from './model.js';

function ProjectStep({ store }) {
  const form = useRef(null), reset = store.resetTick.value;
  useLayoutEffect(() => { prefillFolderOnFocus(form.current.elements.folder, () => rights.hostHome); }, []);
  useLayoutEffect(() => { if (reset) form.current.reset(); }, [reset]);
  const send = event => { event.preventDefault(); const fields = event.currentTarget.elements; void store.submit(fields.name.value, fields.folder.value); };
  return html`<section data-step="project" hidden=${store.step.value !== 'project'}><form id="onboarding-form" noValidate ref=${form} onSubmit=${send}>
    <h1 id="onboarding-title">Create your first project</h1>
    <p>A project is the repo or folder your agents work in. It gets a home room, where its leader works, and a <em>general</em> task room beside it.</p>
    <label>Project name<input name="name" autocomplete="off" required /></label>
    <label>Workspace folder<input name="folder" autocomplete="off" placeholder="/path/to/the/repo" required /></label>
    <button type="submit" class="btn btn-primary" disabled=${store.creating.value}>Create project</button>
    <button type="button" class="btn onboarding-skip" onClick=${() => store.close()}>Skip for now</button>
    <p id="onboarding-error" class="error" role="alert">${store.error.value}</p>
  </form></section>`;
}

function HarnessesStep({ store }) {
  const step = store.step.value, result = step === 'result';
  return html`<section data-step="harnesses" id="onboarding-harnesses" data-phase=${result ? 'result' : 'setup'} hidden=${step !== 'harnesses' && !result}>
    <h1>${result ? 'Your harnesses are ready' : 'Set up a harness'}</h1>
    ${!result && html`<p>A harness is the coding tool (Claude Code, Codex, Pi, OpenCode, Mistral Vibe) your agents run on. Auto-detect what is installed on this machine, or add one by hand, and give it at least one model.</p>`}
    ${result && html`<p>Here is what was added and the models found for it. Set the role defaults below if you like, then continue.</p>`}
    <div id="onboarding-harnesses-host"></div>
    <p id="onboarding-harness-status" role="status" hidden=${result}>${store.harnessStatus.value}</p>
    ${!result && html`<div class="onboarding-actions"><button type="button" id="onboarding-harnesses-skip" class="btn" onClick=${() => void store.skipHarnesses()}>Skip for now</button><button type="button" id="onboarding-harnesses-continue" class="btn btn-primary" disabled=${store.continueDisabled.value} onClick=${() => void store.continueHarnesses()}>Continue</button></div>`}
    ${result && html`<div class="onboarding-actions"><button type="button" id="onboarding-result-continue" class="btn btn-primary" onClick=${() => void store.finishResult()}>Continue</button></div>`}
  </section>`;
}

function HowStep({ store }) {
  const how = store.how.value;
  return html`<section data-step="how" hidden=${store.step.value !== 'how'}>
    <h1>How aimebu works</h1>
    <ol class="onboarding-principles">${PRINCIPLES.map(([head, rest]) => html`<li><strong>${head}</strong>${` ${rest}`}</li>`)}</ol>
    <p id="onboarding-harness-note" role="status" hidden=${how.noteHidden}>${how.noteText}</p>
    <button type="button" id="onboarding-spawn" class="btn btn-primary" hidden=${how.spawnHidden} onClick=${() => store.spawnNow()}>Spawn your first agent</button>
    <button type="button" id="onboarding-setup-harness" class="btn btn-primary" hidden=${how.setupHidden} onClick=${() => void store.showHarnesses('first')}>Set up a harness first</button>
    <button type="button" id="onboarding-done" class="btn" onClick=${() => store.close()}>Not now</button>
  </section>`;
}

const FOCUS = { name: '[name=name]', folder: '[name=folder]', spawn: '#onboarding-spawn', setup: '#onboarding-setup-harness', done: '#onboarding-done' };

export function OnboardingPage({ store, root }) {
  const focus = store.focus.value;
  useLayoutEffect(() => { if (focus.target) root.querySelector(FOCUS[focus.target])?.focus(); }, [focus.n]);
  return html`<${ProjectStep} store=${store} /><${HarnessesStep} store=${store} /><${HowStep} store=${store} />`;
}
