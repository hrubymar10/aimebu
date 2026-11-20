import { batch, signal } from '../../ui.js';
import { tabHidden } from './model.js';
import { GLOBAL_REGISTRY, categoryRights } from './registry.js';

const NO_TARGET = { section: '', category: '', sub: '' };

// One store for every settings dialog: the registry says what exists, `hidden(category, store)` what this viewer may not open (the global Settings: by rights; a
// room's: by the room), `settingHidden(setting, store)` what a category holds that this viewer may not see (search skips it). `title` is the dialog's name,
// `prefix` the start of the ids its parts have (settings-search, room-info-search, ...), `first` the category a wide screen shows when none is named.
export class ShellStore {
  constructor({ users = signal('shown'), registry = GLOBAL_REGISTRY, hidden, settingHidden, first = 'appearance', title = 'Settings', prefix = 'settings' } = {}) {
    this.registry = registry; this.first = first; this.title = signal(title); this.prefix = prefix;
    this.hiddenFn = hidden || (category => tabHidden(categoryRights(category), this.rights()));
    this.settingHiddenFn = settingHidden || (() => false);
    // Where the person is: route is what the address says ('' for a level not chosen), category the one shown (a wide screen shows one even when the address names none),
    // section the page (one of the ten the content lives in) that category draws in.
    this.route = signal(NO_TARGET);
    this.category = signal(first);
    this.section = signal(registry.categoryById(first)?.page || '');
    this.owner = signal(false);
    this.admin = signal(false);
    this.usagesHidden = signal(false);
    // On a phone the dialog is two screens: the list of categories, then one category with a way back. Wider screens show both and ignore this.
    this.view = signal('section');
    this.users = users;
    this.query = signal('');
  }

  rights() { return { owner: this.owner.value, admin: this.admin.value, usagesHidden: this.usagesHidden.value, users: this.users.value }; }
  hidden(category) { return this.hiddenFn(category, this); }
  settingHidden(setting) { return this.settingHiddenFn(setting, this); }
  // The categories the account may open, and the sections that still hold one.
  categories(section) { return this.registry.categoriesOf(section).filter(category => !this.hidden(category)); }
  sections() { return this.registry.sections.filter(section => this.categories(section.id).length); }
  // A category whose entry is not shown is never the selection: it would open blank, with nothing in the nav to say where the person is.
  visible(id) { const category = this.registry.categoryById(id); return category && !this.hidden(category) ? category : null; }
  // Rights that take the selected category away put the selection back on the first one.
  settle() {
    if (this.visible(this.category.value)) return;
    this.apply(this.target(this.first));
  }
  target(id) { const category = this.registry.categoryById(id); return { section: category.section, category: id, sub: '' }; }
  // The level a path (or an old page name) names, cut down to what this account may open: a hidden category or an unknown one is the list.
  resolve(value) {
    const target = typeof value === 'string' ? this.registry.resolveTarget(value) : { ...NO_TARGET, ...value };
    const category = target.category && this.visible(target.category);
    if (category) return target;
    return { section: this.sections().some(section => section.id === target.section) ? target.section : '', category: '', sub: '' };
  }
  apply(target) {
    target = this.resolve(target);
    // A wide screen always shows a category: the one named, else the section's first, else the first of all.
    const shown = target.category || this.categories(target.section)[0]?.id || this.categories(this.sections()[0]?.id)[0]?.id || this.first;
    batch(() => {
      this.route.value = target;
      this.category.value = shown;
      this.section.value = this.registry.categoryById(shown)?.page || '';
      this.view.value = target.category ? 'section' : 'list';
    });
    return target;
  }
  setRights({ owner, admin }) { batch(() => { this.owner.value = owner; this.admin.value = admin; this.usagesHidden.value = !(owner || admin); this.settle(); }); }
  setUser() { this.usagesHidden.value = false; }
  hideUsages() { batch(() => { this.usagesHidden.value = true; this.settle(); }); }
  // The old call: a page name (or a path).
  select(value) { return this.apply(value); }
  showList() { this.apply({ ...NO_TARGET, section: this.route.value.section }); }
}
