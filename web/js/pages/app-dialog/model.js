// The in-app dialog's field rules, import-free: scripts/test-app-dialog-model.cjs runs this module in a vm.
// A field spec is { name, label, type ('checkbox' or text), value, placeholder, hint, inputMode, required, requiredMessage, validate(value, values),
// showIf(values), disabledIf(values), reset: { label, value } }. Values are by field name: a checkbox's is a boolean, a text field's is trimmed.

// The values the fields hold now; `inputAt(index)` is the field's input.
export function readValues(fields, inputAt) {
  return Object.fromEntries(fields.map((spec, index) => {
    const input = inputAt(index);
    return [spec.name, spec.type === 'checkbox' ? !!input.checked : String(input.value ?? '').trim()];
  }));
}

// The values a call starts with, the same readValues gives for untouched inputs.
export const startValues = fields => Object.fromEntries(fields.map(spec => [spec.name, spec.type === 'checkbox' ? !!spec.value : String(spec.value || '').trim()]));

// How each field is drawn for these values: showIf hides it, disabledIf greys it out instead; a reset button is hidden with no value to put back,
// and off while its field is off or already holds that value.
export function fieldStates(fields, values) {
  return fields.map(spec => {
    const hidden = spec.showIf ? !spec.showIf(values) : false;
    const disabled = spec.disabledIf ? !!spec.disabledIf(values) : false;
    const reset = spec.reset ? { hidden: !spec.reset.value, disabled: disabled || values[spec.name] === spec.reset.value } : null;
    return { hidden, disabled, reset };
  });
}

// The first problem that keeps the dialog open, { index, message }, or null: an empty required field first, then a validator's answer. A hidden
// field is never checked.
export function firstProblem(fields, values, states = fieldStates(fields, values)) {
  for (const [index, spec] of fields.entries()) {
    if (spec.required && !states[index].hidden && !values[spec.name]) return { index, message: spec.requiredMessage || `${spec.label} is required.` };
  }
  for (const [index, spec] of fields.entries()) {
    const message = !states[index].hidden && spec.validate?.(values[spec.name], values);
    if (message) return { index, message };
  }
  return null;
}

// What a call answers: a form its values, or null when it is declined; a question true or false.
export const accepted = (fields, values) => (fields.length ? values : true);
export const declined = fields => (fields?.length ? null : false);

// Where the focus starts: a destructive question on Cancel, a form on its first field, anything else on the confirm button.
export const focusTarget = ({ danger = false, fields = [] }) => (danger ? 'cancel' : fields.length ? 'field' : 'ok');
