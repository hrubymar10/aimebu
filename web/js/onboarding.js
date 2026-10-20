// First-run onboarding and the empty state. With no project yet, the owner is taken through "Create your first project" (a project
// with its home room and a "general" room) and a short "How aimebu works"; the same flow opens from the empty state that shows when
// no room is selected.
import { request } from './api.js';

export const PRINCIPLES = [
  ['A project is one repo or folder.', 'Everything you do with agents happens inside a project.'],
  ['Every project has a home room ⌂ and a general room.', 'Almost all work happens in general.'],
  ['Extra sub-rooms are rare.', 'Your leader proposes one and you agree to it.'],
  ['Agents are spawned into a room.', 'Use Spawn; they keep working there until you stop them.'],
  ['Agents take roles.', 'The leader plans and coordinates, workers build, reviewers check.'],
  ['Anything that needs you lands in the Inbox.', 'You decide there: approve, answer or redirect.'],
  ['@mentions and replies wake agents.', 'A reaction acknowledges without waking anyone.'],
];

export class Onboarding {
  // create(project) makes the project (and returns its general room, already selected); spawn() opens the Spawn dialog in it.
  constructor({ create, spawn, send = request, root = document.querySelector('#onboarding-view') }) {
    this.create = create; this.spawn = spawn; this.send = send; this.root = root;
    this.form = root.querySelector('#onboarding-form'); this.error = root.querySelector('#onboarding-error');
    this.steps = { project: root.querySelector('[data-step="project"]'), how: root.querySelector('[data-step="how"]') };
    root.querySelector('.onboarding-principles').replaceChildren(...PRINCIPLES.map(([head, rest]) => {
      const item = document.createElement('li'); const strong = document.createElement('strong'); strong.textContent = head; item.append(strong, ` ${rest}`); return item;
    }));
    this.form.onsubmit = event => { event.preventDefault(); void this.submit(); };
    root.querySelector('.onboarding-skip').onclick = () => this.close();
    root.querySelector('#onboarding-spawn').onclick = () => { this.close(); this.spawn(); };
    root.querySelector('#onboarding-done').onclick = () => this.close();
    root.addEventListener('keydown', event => { if (event.key === 'Escape') this.close(); });
  }
  get open() { return !this.root.hidden; }
  show() {
    this.error.textContent = ''; this.form.reset(); this.showStep('project'); this.root.hidden = false;
    this.form.elements.name.focus();
  }
  close() { this.root.hidden = true; }
  showStep(name) { for (const [key, node] of Object.entries(this.steps)) node.hidden = key !== name; }
  async submit() {
    const name = this.form.elements.name.value.trim(), folder = this.form.elements.folder.value.trim();
    this.error.textContent = '';
    if (!name || !folder) { this.error.textContent = 'Enter a project name and a workspace folder.'; (name ? this.form.elements.folder : this.form.elements.name).focus(); return; }
    const button = this.form.querySelector('button[type=submit]'); button.disabled = true;
    try {
      await this.create({ name, folder });
      this.showStep('how'); this.root.querySelector('#onboarding-spawn').focus();
    } catch (error) { this.error.textContent = error.message; }
    finally { button.disabled = false; }
  }
}
