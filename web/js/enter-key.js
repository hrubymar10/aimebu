// Whether Enter sends in the composers. On a touch screen with no fine pointer (a phone, an iPad without a trackpad or keyboard) Enter makes a new line and the
// send button sends; everywhere else Enter sends and Shift+Enter makes a new line. It is read each time a key is pressed, not once, because an iPad that gets a trackpad
// or a keyboard switches to a fine pointer and then behaves like a desktop.
const TOUCH = '(hover: none) and (pointer: coarse)';
export const enterSends = () => !(typeof matchMedia === 'function' && matchMedia(TOUCH).matches);
