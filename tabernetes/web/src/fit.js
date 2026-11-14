// fitTerminal resizes an xterm.js terminal to the columns and rows that fit
// into the parent of its element, less the padding of the element and the
// scrollbar. The cell size comes from the renderer of xterm.js, which has no
// public API for it; without a rendered cell yet, nothing changes.
export function fitTerminal(term) {
  const parent = term.element?.parentElement;
  const cell = term._core?._renderService?.dimensions?.css?.cell;
  if (!parent || !cell?.width || !cell?.height) return;
  const px = (style, prop) => parseInt(style.getPropertyValue(prop), 10) || 0;
  const box = getComputedStyle(parent);
  const own = getComputedStyle(term.element);
  const scrollbar = term.options.scrollback === 0 ? 0 : term.options.overviewRuler?.width || 14;
  const width = Math.max(0, px(box, "width")) - px(own, "padding-left") - px(own, "padding-right") - scrollbar;
  const height = px(box, "height") - px(own, "padding-top") - px(own, "padding-bottom");
  const cols = Math.max(2, Math.floor(width / cell.width));
  const rows = Math.max(1, Math.floor(height / cell.height));
  if (cols !== term.cols || rows !== term.rows) term.resize(cols, rows);
}
