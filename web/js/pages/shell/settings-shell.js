import { html } from '../../ui.js';
import { IconButton } from '../../components/icon-button.js';
import { Icon } from './icons.js';
import { searchSettings, tokens } from './search.js';

// The top bar: the back arrow is for a phone, where a category is a screen of its own (CSS hides it elsewhere), and the title is centred there. On a wide screen
// the category's own header below carries the name, so the title stays for screen readers.
export function SettingsHeader({ store, onClose, onBack }) {
  const { categoryById } = store.registry, category = store.view.value === 'list' ? null : categoryById(store.category.value), id = store.prefix;
  return html`<${IconButton} id=${`${id}-back`} className="settings-back-btn" name="left" label="Back to the categories" onClick=${onBack} /><h2 id=${`${id}-title`} class="settings-section-title">${category ? category.title : store.title.value}</h2><${IconButton} id=${`${id}-close`} className="settings-close-btn" name="close" label=${`Close ${store.title.value.toLowerCase()}`} onClick=${onClose} />`;
}

// The left column: the sections with their categories. A category's id keeps the old tab ids where a page had one (settings-usages-tab, ...).
export function SettingsNav({ store, onNavigate, onClose, onResult }) {
  const id = store.prefix, { categoryPath } = store.registry;
  const search = text => searchSettings(text, { registry: store.registry, visible: category => !!store.visible(category), visibleSetting: setting => !store.settingHidden(setting) });
  const selected = store.category.value, wide = store.view.value, query = store.query.value;
  const pending = store.users.value === 'pending';
  const searching = tokens(query).length > 0;
  const head = html`<div class="settings-nav-head"><div class="settings-nav-title" aria-hidden="true">${store.title.value}</div><${IconButton} id=${`${id}-nav-close`} className="settings-nav-close" name="close" label=${`Close ${store.title.value.toLowerCase()}`} onClick=${onClose} /></div>
    <div class="settings-search"><${Icon} name="search" size=${15} /><input id=${`${id}-search`} type="search" class="settings-search-input" placeholder="Search" aria-label=${`Search ${store.title.value.toLowerCase()}`} autocomplete="off" spellcheck="false" value=${query}
      onInput=${event => { store.query.value = event.currentTarget.value; }}
      onKeyDown=${event => { if (event.key === 'Escape' && query) { event.preventDefault(); event.stopPropagation(); store.query.value = ''; } else if (event.key === 'Enter') { const first = search(query)[0]; if (first) onResult(first); } }} /></div>`;
  if (searching) {
    const results = search(query);
    return html`${head}<div class="settings-results" id=${`${id}-results`} role="list" aria-label="Search results">${results.length === 0
      ? html`<p class="settings-results-empty" id=${`${id}-results-empty`} role="status">No settings match “${query.trim()}”.</p>`
      : results.map(result => html`<button type="button" role="listitem" class="settings-result" key=${`${result.category.id}/${result.setting?.anchor || result.setting?.template || ''}`} title=${result.path} onClick=${() => onResult(result)}>
        <${Icon} name=${result.category.icon} size=${16} /><span class="settings-result-text"><span class="settings-result-name">${result.title}</span><span class="settings-result-path">${result.path}</span></span></button>`)}</div>`;
  }
  return html`${head}${store.sections().map(section => html`<div class="settings-nav-section" key=${section.id} role="group" aria-labelledby=${`${id}-${section.id}-title`}>
    <div id=${`${id}-${section.id}-title`} class="settings-nav-title settings-nav-group-title">${section.title}</div>
    ${store.categories(section.id).map(category => {
      const waiting = category.id === 'users' && pending, current = selected === category.id && wide !== 'list';
      return html`<button id=${`${id}-${category.id}-tab`} type="button" key=${category.id} class=${`settings-nav-item${selected === category.id ? ' active' : ''}${waiting ? ' settings-tab-pending' : ''}`} title=${category.title}
        aria-current=${current ? 'page' : undefined} aria-hidden=${waiting ? 'true' : undefined} tabindex=${waiting ? -1 : undefined} onClick=${() => onNavigate(categoryPath(category))}><${Icon} name=${category.icon} /><span class="settings-nav-label">${category.title}</span><${Icon} name="chevron" size=${14} className="settings-nav-chevron" /></button>`;
    })}</div>`)}`;
}

// The head of the right column: the category's icon, name and one line about it, then its subcategories as rows (each a jump inside the category).
export function CategoryHead({ store, onNavigate }) {
  const { categoryById, categoryPath, sectionOf, settings } = store.registry, category = categoryById(store.category.value);
  const subs = (category.subs || []).filter(sub => {
    const setting = settings.find(item => item.category === category.id && (sub.anchor ? item.anchor === sub.anchor : item.selector === sub.selector));
    return !setting || !store.settingHidden(setting);
  });
  return html`<div class="settings-cat-head"><div class="settings-cat-icon"><${Icon} name=${category.icon} size=${26} /></div><div class="settings-cat-text"><div class="settings-cat-name">${category.title}</div><p class="settings-cat-desc">${category.description}</p><p class="settings-cat-path" aria-hidden="true">${sectionOf(category.section).title}</p></div></div>${subs.length > 0 && html`<div class="settings-subs" role="list">${subs.map(sub => html`<button type="button" role="listitem" class="settings-sub-row" key=${sub.id} title=${sub.title} onClick=${() => onNavigate(categoryPath(category, sub.id))}><span>${sub.title}</span><${Icon} name="chevron" size=${14} /></button>`)}</div>`}`;
}
