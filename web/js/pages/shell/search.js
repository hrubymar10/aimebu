// scripts/test-settings-search.cjs runs this module in a vm after registry.js, with its import line cut.
import { CATEGORIES, SETTINGS, categoryById, sectionOf, settingPath } from './registry.js';
//
// Search over every setting, by name, description and keywords, in categories the person has not opened. A result carries its path ("Agents › Prompts › Mentions").
// The caller says which categories the account may see, so a setting it may not open is never found (and its name never shown).

const fold = text => String(text || '').toLowerCase().normalize('NFD').replace(/[̀-ͯ]/g, '');
export const tokens = query => fold(query).split(/[^a-z0-9$%]+/).filter(Boolean);

// How well one token fits a field: 0 for no match, more for a start of the whole field or of a word, less for the middle of a word.
function fit(field, token) {
  const at = field.indexOf(token);
  if (at < 0) return 0;
  if (at === 0) return 4;
  return /[^a-z0-9]/.test(field[at - 1]) ? 3 : 1;
}

// results: [{kind: 'setting'|'category', category, setting?, path, title, score}], best first. `visible(categoryId)` answers for the account.
export function searchSettings(query, { visible = () => true } = {}) {
  const words = tokens(query);
  if (!words.length) return [];
  const results = [];
  const consider = (item, kind, category, fields) => {
    let score = 0;
    for (const word of words) {
      let best = 0;
      for (const [weight, field] of fields) best = Math.max(best, fit(field, word) * weight);
      if (!best) return; // every word must match something
      score += best;
    }
    results.push({ kind, category, ...item, score });
  };
  for (const category of CATEGORIES) {
    if (!visible(category.id)) continue;
    const section = sectionOf(category.section);
    consider({ path: `${section.title} › ${category.title}`, title: category.title }, 'category', category, [[8, fold(category.title)], [3, fold(section.title)], [2, fold(category.description)], [1, fold((category.subs || []).map(sub => sub.title).join(' '))]]);
  }
  for (const setting of SETTINGS) {
    if (!visible(setting.category)) continue;
    const category = categoryById(setting.category), section = sectionOf(category.section);
    consider({ setting, path: settingPath(setting), title: setting.name }, 'setting', category,
      [[8, fold(setting.name)], [5, fold(setting.keywords.join(' '))], [3, fold(`${category.title} ${section.title} ${setting.group}`)], [2, fold(setting.description)]]);
  }
  return results.sort((a, b) => b.score - a.score || (a.kind === b.kind ? 0 : a.kind === 'category' ? -1 : 1) || a.path.localeCompare(b.path));
}
