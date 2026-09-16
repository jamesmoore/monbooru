'use strict';

// Chord state for leader-key navigation. The next key within
// chordTimeoutMs resolves the chord; anything else cancels.
var _chordNode = null;
var _chordLabel = '';
var _chordTimer = null;
var _chordHint = null;
var chordTimeoutMs = 500;

// Chord tree. A leaf is a string URL (navigated to) or a function run with
// no args; a nested map is a sub-chord whose own keys resolve the next
// step. `g` leads navigation (`g c a` categories, `g c o` collections).
// `e` leads detail-page field edits: source, collection, note, annotation,
// relation, the bulk tag removal and the gallery transfer, plus a digit for
// that Nth collection. `t` leads the tag actions on the tags listing
// and a tag's own page. A leaf given as a selector list clicks the first
// one present, and the leader hint drops the leaves this page cannot serve.
// On pages without the matching control the chord no-ops.
var chordMap = {
  g: {
    g: '/',
    i: '/?q=inbox:true',
    c: {
      a: '/categories',
      o: '/collections',
    },
    t: '/tags',
    r: '/relations',
    s: '/settings',
  },
  t: {
    r: ['#btn-detail-rename'],
    c: ['#tag-batch-bar:not([hidden]) #btn-batch-category', '#btn-detail-category'],
    a: ['#tag-batch-bar:not([hidden]) #btn-batch-alias', '#btn-detail-alias'],
    m: ['#tag-batch-bar:not([hidden]) #btn-batch-merge'],
    i: ['#tag-batch-bar:not([hidden]) #btn-batch-imply'],
    f: ['#tag-batch-bar:not([hidden]) #btn-batch-merge-folded'],
    p: ['#tag-batch-bar:not([hidden]) #btn-batch-ptr'],
  },
  // The x chord has no named leaves: its leaves are the digits, one per
  // plugin button on whichever bar is up.
  x: {},
  e: {
    s: ['.btn-add-source'],
    c: ['.btn-add-collection'],
    n: ['.btn-edit-note'],
    a: ['.btn-add-annotation'],
    r: ['[data-relations-add]'],
    t: ['#remove-tags-btn'],
    g: ['#transfer-image-btn'],
  },
};

// chordDigitTargets names the controls a chord's digit leaves click, in
// render order: the collections listed on an image, and the plugin buttons
// on whichever bar is up.
function chordDigitTargets(node) {
  if (node === chordMap.e) return document.querySelectorAll('.btn-edit-collection');
  if (node === chordMap.x) return document.querySelectorAll('.plugin-card .plugin-btn, #plugin-batch-bar.visible .plugin-btn');
  return null;
}

function clearChord() {
  _chordNode = null;
  _chordLabel = '';
  if (_chordTimer) { clearTimeout(_chordTimer); _chordTimer = null; }
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = null;
}

// enterChord arms a (sub-)chord: the next key is resolved against `node`,
// expiring after chordTimeoutMs. `label` is the keys pressed so far.
function enterChord(node, label) {
  _chordNode = node;
  _chordLabel = label;
  showChordHint(node, label);
  if (_chordTimer) clearTimeout(_chordTimer);
  _chordTimer = setTimeout(clearChord, chordTimeoutMs);
}

function showChordHint(node, label) {
  if (_chordHint && _chordHint.parentNode) _chordHint.parentNode.removeChild(_chordHint);
  _chordHint = document.createElement('div');
  _chordHint.className = 'chord-leader-hint';
  var keys = Object.keys(node || {}).filter(function(k) {
    var leaf = node[k];
    return !Array.isArray(leaf) || leaf.some(function(sel) { return document.querySelector(sel); });
  });
  var digits = chordDigitTargets(node);
  if (digits) {
    for (var i = 1; i <= digits.length && i <= 9; i++) keys.push(String(i));
  }
  keys.sort();
  _chordHint.textContent = label + ' → ' + keys.join(' / ');
  document.body.appendChild(_chordHint);
}

// pageScroller returns the nearest ancestor that actually scrolls. The
// layout pins body to overflow:hidden and lets <main> own the scrolling
// so the topbar / footer stay sticky. Falls back to document scrolling.
function pageScroller(el) {
  var node = el && el.parentElement;
  while (node) {
    var ov = getComputedStyle(node).overflowY;
    if ((ov === 'auto' || ov === 'scroll') && node.scrollHeight > node.clientHeight) {
      return node;
    }
    node = node.parentElement;
  }
  return document.scrollingElement || document.documentElement;
}

function scrollPageTo(el, top) {
  var s = pageScroller(el);
  if (s === document.documentElement || s === document.body || s === document.scrollingElement) {
    window.scrollTo({ top: top, behavior: 'smooth' });
  } else {
    s.scrollTo({ top: top, behavior: 'smooth' });
  }
}

function scrollToTop(el) { scrollPageTo(el, 0); }
function scrollToBottom(el) {
  var s = pageScroller(el);
  scrollPageTo(el, s.scrollHeight);
}

// setFocused moves a keyboard cursor: strip .focused off every element in
// `items`, mark items[idx], and keep it in view. `block` overrides the
// scrollIntoView alignment (default 'nearest').
function setFocused(items, idx, block) {
  items.forEach(function(el) { el.classList.remove('focused'); });
  items[idx].classList.add('focused');
  items[idx].scrollIntoView({ block: block || 'nearest' });
}

// Move-cursor / open-card helpers reused by both arrow keys and h/j/k/l.
// When dy != 0 and the would-be index walks off the top/bottom row, the
// scroll container scrolls to the top / bottom (search bar above,
// pagination below) instead of clamping silently. Horizontal walk-off
// keeps the existing clamp - side-scrolling has no useful target here.
function moveGridCursor(dx, dy) {
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  var focused = document.querySelector('.thumb-card.focused');
  var idx = focused ? cards.indexOf(focused) : -1;
  if (idx < 0) {
    idx = 0;
  } else {
    var cols = 1;
    var grid = document.querySelector('.thumb-grid');
    if (grid && cards[0]) {
      var cardW = cards[0].offsetWidth;
      if (cardW > 0) cols = Math.max(1, Math.round(grid.offsetWidth / cardW));
    }
    var newIdx = idx + dx + dy * cols;
    if (dy < 0 && newIdx < 0) {
      scrollToTop(cards[idx]);
      return true;
    }
    if (dy > 0 && newIdx > cards.length - 1) {
      scrollToBottom(cards[idx]);
      return true;
    }
    idx = Math.max(0, Math.min(cards.length - 1, newIdx));
  }
  setFocused(cards, idx);
  return true;
}

// Row cursor on the tags listing, the table's counterpart to the grid
// cursor: it is what Space and Enter act on.
function moveTagRowCursor(step) {
  var rows = Array.from(document.querySelectorAll('.tag-row'));
  if (rows.length === 0) return false;
  var focused = document.querySelector('.tag-row.focused');
  var at = focused ? rows.indexOf(focused) + step : 0;
  setFocused(rows, Math.max(0, Math.min(rows.length - 1, at)));
  return true;
}

function jumpGridCursor(target) {
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  if (cards.length === 0) return false;
  setFocused(cards, target === 'first' ? 0 : cards.length - 1);
  return true;
}

// stepThumbSize walks the size ramp; the pressed button is the current size.
function stepThumbSize(delta) {
  var btns = Array.from(document.querySelectorAll('#thumb-size-ramp button'));
  if (btns.length === 0) return false;
  var at = btns.findIndex(function(b) { return b.getAttribute('aria-pressed') === 'true'; });
  var next = Math.max(0, Math.min(btns.length - 1, (at < 0 ? 0 : at) + delta));
  if (next !== at) btns[next].click();
  return true;
}

function clickPagination(needle) {
  var links = document.querySelectorAll('.pagination a');
  for (var i = 0; i < links.length; i++) {
    if (links[i].textContent.indexOf(needle) >= 0) { links[i].click(); return true; }
  }
  return false;
}

// handlePaginationKey maps [ ] G p onto the shared pagination controls, so
// the keys reach every page that renders the partial. Returns true when the
// key was consumed - the matching control exists and was clicked.
function handlePaginationKey(e) {
  if (e.key === '[') return clickPagination('Prev');
  if (e.key === ']') return clickPagination('Next');
  if (e.key === 'G') return clickPagination('Last');
  if (e.key === 'p') {
    var jp = document.querySelector('.page-jump');
    if (jp) { jp.click(); return true; }
  }
  return false;
}

// detailRefBack is the history-back step of the back chain alone: true when
// the detail page carries data-ref (similar-click chain) and history has a
// predecessor. Shared with the .back-link click handler, which must leave
// the link's default navigation alive on cold loads (direct URL, bookmark).
function detailRefBack() {
  var detailPage = document.getElementById('detail-page');
  if (detailPage && detailPage.dataset.ref && history.length > 1) { history.back(); return true; }
  return false;
}

// detailBack walks the full back chain shared by Escape, Backspace, and the
// pages-grid Escape: unwind history one step on a data-ref page, else click
// the .back-link (its href carries the return query), else '/'. The pages
// grid has no #detail-page, so only the link walk runs there.
function detailBack() {
  if (detailRefBack()) return;
  var backLink = document.querySelector('.back-link');
  if (backLink) backLink.click();
  else window.location.href = '/';
}

// Detail-page prev/next, image or tag depending on noun. Falls through
// when no nav arrow is present.
function navDetailArrow(direction, noun) {
  var word = direction === 'prev' ? 'Previous' : 'Next';
  var a = document.querySelector('.nav-arrow[title^="' + word + ' ' + noun + '"]');
  if (a && a.href) { window.location.href = a.href; return true; }
  return false;
}

// Remove a relation row and, when it empties its origin subgroup, drop the
// now-empty list and its "added by" subheading too.
function pruneRelationRow(btn) {
  var li = btn.closest('li');
  if (!li) return;
  var ul = li.parentElement;
  li.remove();
  if (ul && ul.classList.contains('tag-list') && !ul.querySelector('li')) {
    var sub = ul.previousElementSibling;
    if (sub && sub.classList.contains('relation-origin-sub')) sub.remove();
    ul.remove();
  }
}

// Group delete: drop the origin subheading the button sits in along with
// the list under it, the group-scale counterpart of pruneRelationRow.
function pruneRelationGroup(btn) {
  var sub = btn.closest('.relation-origin-sub');
  if (!sub) return;
  var ul = sub.nextElementSibling;
  if (ul && ul.classList.contains('tag-list')) ul.remove();
  sub.remove();
}

// Cycle the rating ceiling level. delta=+1 picks a stricter level (less
// permissive); delta=-1 picks a more permissive one. Fires the matching
// inactive footer-rating-link form.
function cycleRatingCeiling(delta) {
  var labels = document.querySelectorAll('.footer-rating-active, .footer-rating-link');
  // Active sits among the cluster as the [bracketed] span; siblings are
  // <button class="footer-rating-link">. Find the active index from the
  // active span's text and submit the neighbour's parent form.
  var active = document.querySelector('.footer-rating-active');
  if (!active) return false;
  var siblings = active.parentNode.parentNode.querySelectorAll('.footer-rating-active, .footer-rating-link');
  var ordered = Array.prototype.slice.call(siblings);
  var idx = ordered.indexOf(active);
  if (idx < 0) return false;
  var next = idx + delta;
  if (next < 0 || next >= ordered.length) return false;
  var target = ordered[next];
  if (target.tagName === 'BUTTON') {
    var f = target.closest('form');
    if (f) { f.requestSubmit ? f.requestSubmit() : f.submit(); return true; }
  }
  return false;
}

// applySortChange re-runs the search on the picked sort. Only collection
// order forces a direction: carrying a descending pick over renders the
// collection backwards, which is never what picking it means. Every
// other sort keeps whatever the operator set, so switching newest to
// file size while reading oldest-first stays smallest-first.
function applySortChange(sortEl) {
  if (sortEl.value === 'order') {
    var orderEl = document.querySelector('#search-form select[name="order"]');
    if (orderEl) orderEl.value = 'asc';
  }
  // The seed belongs to the random sort. Left on the form it rides into
  // the pushed URL, where the pagination links have already dropped it and
  // Save search would store it against the schema's own rule.
  if (sortEl.value !== 'random') {
    var seedEl = document.querySelector('#search-form input[name="seed"]');
    if (seedEl) seedEl.remove();
  }
  return submitSearch();
}

// Cycle the gallery sort select between newest -> filesize -> order -> random.
function cycleSort() {
  var sortEl = document.getElementById('search-sort');
  if (!sortEl) return false;
  var order = ['newest', 'filesize', 'order', 'random'];
  var i = order.indexOf(sortEl.value);
  var next = order[(i + 1) % order.length];
  if (next === 'random') {
    applyRandomSort();
    return true;
  }
  var opt = sortEl.querySelector('option[value="' + next + '"]');
  if (!opt) {
    opt = document.createElement('option');
    opt.value = next;
    opt.textContent = next;
    sortEl.appendChild(opt);
  }
  sortEl.value = next;
  return applySortChange(sortEl);
}

function flipSortDirection() {
  var orderEl = document.querySelector('#search-form select[name="order"]');
  if (!orderEl) return false;
  orderEl.value = orderEl.value === 'asc' ? 'desc' : 'asc';
  return submitSearch();
}

// Re-runs the gallery search with whatever the form currently holds.
// htmx binds the swap to the form's submit event, so dispatching it is
// how any control outside the Search button applies its change.
function submitSearch() {
  var form = document.getElementById('search-form');
  if (!form) return false;
  form.dispatchEvent(new Event('submit', { bubbles: true }));
  return true;
}

function focusFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { revealSidebarFor(el); el.focus(); if (el.select) el.select(); return true; }
  }
  return false;
}

// clickFirstSelector is focusFirstSelector for the shortcuts whose target is
// a button: a key means nothing on a page that doesn't render one, so the
// caller only swallows the event when something was clicked.
function clickFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { el.click(); return true; }
  }
  return false;
}

// Surface helpers; keep the keydown router readable.
function isGalleryPage()    { return !!document.querySelector('.thumb-grid, #gallery-grid'); }
function isDetailPage()     { return !!document.getElementById('detail-page'); }
function isTagDetailPage()  { return !!document.getElementById('tag-detail-page'); }
function isTagsPage()       { return !!document.getElementById('tags-page'); }
function isCategoriesPage() { return !!document.getElementById('categories-page'); }
function isSettingsPage()   { return !!document.getElementById('settings-page'); }
function batchBarVisible() {
  var bar = document.getElementById('batch-bar');
  return !!(bar && bar.classList.contains('visible'));
}

// The surfaces whose bindings are live right now, read off the same
// predicates the keydown router branches on so the sheet and the router
// cannot disagree about what a key does here.
function activeSurfaces() {
  var s = ['anywhere'];
  if (isGalleryPage())    s.push('gallery');
  if (batchBarVisible())  s.push('selection');
  if (isDetailPage())     s.push('detail');
  if (isTagsPage())       s.push('tags');
  if (isTagDetailPage())  s.push('tag-detail');
  if (isCategoriesPage()) s.push('categories');
  if (isSettingsPage())   s.push('settings');
  if (document.getElementById('pages-grid-page')) s.push('pages');
  if (document.getElementById('relations-session-page')) s.push('relations');
  if (document.querySelector('.detail-img-link')) s.push('lightbox');
  return s;
}

// Hides the blocks for surfaces the operator is not on, and the rows whose
// control this page does not render - each row's data-when is the selector
// its handler looks for, so a listed key always has something behind it.
function syncShortcutSheet(dlg) {
  var all = dlg.classList.contains('show-all');
  var active = activeSurfaces();
  dlg.querySelectorAll('section[data-surface]').forEach(function(sec) {
    sec.hidden = !all && active.indexOf(sec.dataset.surface) === -1;
  });
  dlg.querySelectorAll('[data-when]').forEach(function(row) {
    row.hidden = !all && !document.querySelector(row.dataset.when);
  });
}

function openShortcutSheet() {
  var dlg = document.getElementById('shortcuts-help');
  if (!dlg) return false;
  dlg.classList.remove('show-all');
  var toggle = document.getElementById('shortcuts-all');
  if (toggle) toggle.textContent = '[all surfaces]';
  syncShortcutSheet(dlg);
  if (!dlg.open) dlg.showModal();
  // showModal scrolls its first focusable element into view, which on a
  // viewport too short for the sheet means opening below the title.
  dlg.scrollTop = 0;
  return true;
}

document.addEventListener('click', function(e) {
  if (!e.target.closest || !e.target.closest('#shortcuts-all')) return;
  var dlg = document.getElementById('shortcuts-help');
  var all = dlg.classList.toggle('show-all');
  e.target.textContent = all ? '[this page]' : '[all surfaces]';
  syncShortcutSheet(dlg);
});

// Pages-grid keymap. Arrow keys walk the cell focus across rows / columns
// (cols computed from the rendered card width like the gallery grid),
// Enter opens the focused page in the reader, Home / End jump.
function handlePagesGridKey(e) {
  var page = document.getElementById('pages-grid-page');
  if (!page) return false;
  if (e.target.tagName.toLowerCase() === 'input' || e.target.isContentEditable) return false;
  if (document.querySelector('dialog[open]')) return false;
  var cells = Array.from(page.querySelectorAll('.manga-page-cell'));
  if (cells.length === 0) return false;
  var focused = page.querySelector('.manga-page-cell.focused');
  var idx = focused ? cells.indexOf(focused) : -1;
  function moveTo(target) {
    target = Math.max(0, Math.min(cells.length - 1, target));
    setFocused(cells, target);
  }
  function cols() {
    if (cells.length === 0) return 1;
    // Count cells whose offsetTop matches the first cell's top - that's
    // the rendered column count regardless of grid gap. The simple
    // gridWidth/cellWidth heuristic overshoots when the gap is set
    // because cellWidth excludes the gap, leaving ArrowDown short by
    // one column on every row boundary.
    var firstTop = cells[0].offsetTop;
    var n = 0;
    for (var i = 0; i < cells.length; i++) {
      if (cells[i].offsetTop !== firstTop) break;
      n++;
    }
    return Math.max(1, n);
  }
  function tryVerticalScroll(target) {
    if (idx < 0) return false;
    if (target < 0) {
      scrollToTop(cells[idx]);
      return true;
    }
    if (target > cells.length - 1) {
      scrollToBottom(cells[idx]);
      return true;
    }
    return false;
  }
  switch (e.key) {
    case 'ArrowRight':
      e.preventDefault(); moveTo(idx < 0 ? 0 : idx + 1); return true;
    case 'ArrowLeft':
      e.preventDefault(); moveTo(idx < 0 ? 0 : idx - 1); return true;
    case 'ArrowDown':
      e.preventDefault();
      if (tryVerticalScroll(idx + cols())) return true;
      moveTo(idx < 0 ? 0 : idx + cols()); return true;
    case 'ArrowUp':
      e.preventDefault();
      if (tryVerticalScroll(idx - cols())) return true;
      moveTo(idx < 0 ? 0 : idx - cols()); return true;
    case 'Home':
      e.preventDefault(); moveTo(0); return true;
    case 'End':
      e.preventDefault(); moveTo(cells.length - 1); return true;
    case 'Enter':
      if (idx < 0) return false;
      var link = cells[idx].querySelector('a');
      if (link && link.href) { window.location.href = link.href; return true; }
      return false;
  }
  return false;
}

// Uses a <dialog> so the browser handles Esc / scroll lock natively; the
// keymap below runs first in the global router so wheel-driven zoom and
// pan keys don't leak into detail-page bindings.
var lightbox = (function () {
  var dlg = null, img = null, stage = null, zoomLbl = null, openLink = null;
  var scale = 1, tx = 0, ty = 0;
  var panning = false, px0 = 0, py0 = 0, tx0 = 0, ty0 = 0;
  // pressMoved guards click-to-close so a drag-to-pan never dismisses the
  // viewer. Squared 4px threshold matches the browser's own click-vs-drag
  // tolerance, which is what users feel.
  var pressMoved = false;
  var bound = false;

  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
  // The scale that renders one image pixel per device pixel. `scale` is a
  // multiplier on top of the CSS fit, so the ratio is read back off the
  // painted box rather than assumed to be 1.
  function oneToOneScale() {
    if (!img || !img.naturalWidth) return 0;
    var r = contentRect();
    if (!r || !r.width) return 0;
    return img.naturalWidth * scale / r.width;
  }
  // The floor keeps a large image from shrinking to nothing, but 1:1 on an
  // image smaller than the stage sits below it, so it never rules that out.
  function minScale() {
    var one = oneToOneScale();
    return one > 0 ? Math.min(0.5, one) : 0.5;
  }
  function apply() {
    if (!img) return;
    img.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    if (!zoomLbl) return;
    // Reported against the image's own pixels, not the fit: on a small
    // image the fit is already an upscale, and "100%" there would lie.
    var one = oneToOneScale();
    zoomLbl.textContent = Math.round((one > 0 ? scale / one : scale) * 100) + '%';
  }
  function reset() { scale = 1; tx = 0; ty = 0; apply(); }
  function zoomAt(cx, cy, factor) {
    var ns = clamp(scale * factor, minScale(), 4);
    var k = ns / scale;
    tx = cx - k * (cx - tx);
    ty = cy - k * (cy - ty);
    scale = ns;
    apply();
  }
  // The IMG element fills the stage (CSS width/height: 100%) so a small
  // image scales up; object-fit: contain letterboxes the natural-aspect
  // content inside that box. Click-to-close and the 1:1 zoom math both
  // need the visible content rect, not the element rect, otherwise the
  // letterbox counts as "on the image".
  function contentRect() {
    if (!img) return null;
    var r = img.getBoundingClientRect();
    if (!r.width || !r.height) return null;
    var iw = img.naturalWidth, ih = img.naturalHeight;
    if (!iw || !ih) return r;
    var ia = iw / ih, ba = r.width / r.height;
    var w, h;
    if (ia > ba) { w = r.width; h = r.width / ia; }
    else { h = r.height; w = r.height * ia; }
    var left = r.left + (r.width - w) / 2;
    var top = r.top + (r.height - h) / 2;
    return { left: left, top: top, right: left + w, bottom: top + h, width: w, height: h };
  }
  // 1:1 means one image pixel renders to one device pixel. The currently
  // displayed width is naturalWidth at scale=1 multiplied by the CSS fit,
  // so we invert through whatever scale is active right now to land on
  // naturalWidth without re-reading the un-transformed bounding box.
  function oneToOne() {
    var one = oneToOneScale();
    if (one <= 0) return;
    var ns = clamp(one, minScale(), 4);
    var k = ns / scale;
    tx = k * tx; ty = k * ty;
    scale = ns;
    apply();
  }
  function onWheel(e) {
    e.preventDefault();
    var sr = stage.getBoundingClientRect();
    var cx = e.clientX - (sr.left + sr.width / 2);
    var cy = e.clientY - (sr.top + sr.height / 2);
    zoomAt(cx, cy, e.deltaY < 0 ? 1.2 : 1 / 1.2);
  }
  function onPointerDown(e) {
    if (e.button !== 0) return;
    stage.setPointerCapture(e.pointerId);
    panning = true;
    pressMoved = false;
    px0 = e.clientX; py0 = e.clientY;
    tx0 = tx; ty0 = ty;
    stage.classList.add('grabbing');
  }
  function onPointerMove(e) {
    if (!panning) return;
    if (!pressMoved) {
      var dx = e.clientX - px0, dy = e.clientY - py0;
      if (dx * dx + dy * dy > 16) pressMoved = true;
    }
    tx = tx0 + (e.clientX - px0);
    ty = ty0 + (e.clientY - py0);
    apply();
  }
  function onPointerEnd(e) {
    if (!panning) return;
    panning = false;
    stage.releasePointerCapture(e.pointerId);
    stage.classList.remove('grabbing');
    // Click-to-close: release without panning, off the image itself.
    // Releases *on* the image fall through so dblclick still toggles
    // fit / 1:1 (the first click of a dblclick must not dismiss).
    if (pressMoved || !img) return;
    var r = contentRect();
    if (!r || !r.width || e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) {
      dlg.close();
    }
  }
  function onDblClick() {
    if (Math.abs(scale - 1) < 0.01) oneToOne();
    else reset();
  }
  function ensure() {
    if (dlg) return true;
    dlg = document.getElementById('image-lightbox');
    if (!dlg) return false;
    img = document.getElementById('lightbox-img');
    stage = document.getElementById('lightbox-stage');
    zoomLbl = document.getElementById('lightbox-zoom');
    openLink = document.getElementById('lightbox-open');
    if (!bound && stage) {
      stage.addEventListener('wheel', onWheel, { passive: false });
      stage.addEventListener('pointerdown', onPointerDown);
      stage.addEventListener('pointermove', onPointerMove);
      stage.addEventListener('pointerup', onPointerEnd);
      stage.addEventListener('pointercancel', onPointerEnd);
      stage.addEventListener('dblclick', onDblClick);
      // The zoom readout needs the natural size, which open() sets the src
      // too early to have.
      img.addEventListener('load', apply);
      dlg.addEventListener('close', reset);
      var closeBtn = document.getElementById('lightbox-close');
      if (closeBtn) closeBtn.addEventListener('click', function () { dlg.close(); });
      bound = true;
    }
    return true;
  }
  return {
    open: function (e, src) {
      // Pass-through for modifier / middle clicks so target=_blank still
      // opens the raw file in a new tab. Returning truthy keeps the
      // anchor's default navigation alive.
      if (e && (e.button === 1 || e.ctrlKey || e.metaKey || e.shiftKey)) return true;
      if (!ensure()) return true;
      if (e && e.preventDefault) e.preventDefault();
      img.src = src;
      if (openLink) openLink.href = src;
      reset();
      if (!dlg.open) dlg.showModal();
      return false;
    },
    handleKey: function (e) {
      if (!dlg || !dlg.open) return false;
      var t = e.target.tagName.toLowerCase();
      if (t === 'input' || t === 'textarea' || e.target.isContentEditable) return false;
      switch (e.key) {
        case '+': case '=':
          e.preventDefault(); zoomAt(0, 0, 1.2); return true;
        case '-':
          e.preventDefault(); zoomAt(0, 0, 1 / 1.2); return true;
        case '0':
          e.preventDefault(); reset(); return true;
        case '1':
          e.preventDefault(); oneToOne(); return true;
      }
      return false;
    },
  };
})();

// Exposed for inline onclick on the still-image link and the
// .btn-view button in .detail-actions.
function openLightbox(e, src) { return lightbox.open(e, src); }

// openReaderJumpDialog opens the reader's page-jump dialog with the input
// focused and pre-selected. Shared by the p key and the counter click.
// Returns false when the dialog isn't on the page.
function openReaderJumpDialog() {
  var dlg = document.getElementById('reader-jump-dialog');
  if (!dlg) return false;
  dlg.showModal();
  var inp = document.getElementById('reader-jump-input');
  if (inp) { inp.focus(); inp.select(); }
  return true;
}

// Reader keymap. The reader is a separate <body class="reader-body"> that
// hides every gallery / detail control via CSS, so its keys must run
// before the global keymap and short-circuit it; otherwise the gallery's
// h/l/space bindings would steal the reader's prev/next.
function handleReaderKey(e) {
  var reader = document.getElementById('reader');
  if (!reader) return false;
  if (e.target.tagName.toLowerCase() === 'input') return false;
  if (document.querySelector('dialog[open]')) {
    if (e.key === 'Escape') {
      // Browser handles dialog Esc; let it through.
    }
    return false;
  }
  var page = parseInt(reader.dataset.page, 10) || 1;
  var total = parseInt(reader.dataset.total, 10) || 1;
  var imgID = reader.dataset.imageId;
  var detail = reader.dataset.detailUrl || ('/images/' + imgID);
  function go(p) {
    if (p < 1) p = 1; if (p > total) p = total;
    if (p === page) return;
    // Preserve the existing query (back_q / from=pages / etc) so a
    // keyboard page-flip stays in the same context as a click on the
    // template-emitted prev/next anchors. Without from=pages the
    // next Esc would land on the detail page instead of the pages grid.
    var url = new URL(window.location.href);
    url.searchParams.set('page', String(p));
    url.hash = '';
    window.location.href = url.pathname + url.search;
  }
  switch (e.key) {
    case 'ArrowRight': case 'l': case ' ':
      e.preventDefault(); go(page + 1); return true;
    case 'ArrowLeft': case 'h':
      e.preventDefault(); go(page - 1); return true;
    case 'Home':
      e.preventDefault(); go(1); return true;
    case 'End':
      e.preventDefault(); go(total); return true;
    case 'p':
      if (openReaderJumpDialog()) e.preventDefault();
      return true;
    case 'P':
      var pagesLink = document.querySelector('.reader-pages');
      if (pagesLink && pagesLink.href) {
        e.preventDefault();
        window.location.href = pagesLink.href;
        return true;
      }
      return false;
    case 'o':
      e.preventDefault();
      var openLink = document.querySelector('.reader-open');
      if (openLink) openLink.click();
      return true;
    case 'e':
      var extractForm = document.querySelector('.reader-extract');
      if (extractForm) {
        e.preventDefault();
        extractForm.submit();
        return true;
      }
      return false;
    case 'v':
      var pageLink = document.querySelector('.reader-page-link');
      if (pageLink) {
        e.preventDefault();
        openLightbox(null, pageLink.href);
        return true;
      }
      return false;
    case 'Escape': case 'Backspace':
      e.preventDefault();
      window.location.href = detail;
      return true;
  }
  return false;
}

document.addEventListener('keydown', function(e) {
  var tag = e.target.tagName.toLowerCase();
  var isInput = tag === 'input' || tag === 'textarea' || tag === 'select' || e.target.isContentEditable;

  if (lightbox.handleKey(e)) return;
  if (handleReaderKey(e)) return;
  if (handlePagesGridKey(e)) return;

  // 'Escape' → blur input first; once nothing is focused, walk the chain.
  if (e.key === 'Escape') {
    if (isInput) { e.target.blur(); return; }
    if (document.querySelector('dialog[open]')) return;
    if (selectionArmed()) {
      e.preventDefault();
      selectMode = false;
      clearSelection();
      return;
    }
    if (document.body.classList.contains('tag-focus') || focusedTagRow()) {
      e.preventDefault();
      exitTagFocusMode();
      return;
    }
    if (isDetailPage() || document.getElementById('pages-grid-page')) {
      e.preventDefault();
      detailBack();
      return;
    }
    // Back-navigation fallback: respect any page handler that already
    // claimed this Esc (e.g. the relations-session compare-slider close).
    // Without this guard the data-esc-back match below would navigate on
    // top of a slider dismissal.
    if (e.defaultPrevented) return;
    var escBackEl = document.querySelector('[data-esc-back]');
    if (escBackEl) {
      var dest = escBackEl.dataset.escBack;
      if (dest) {
        e.preventDefault();
        window.location.href = dest;
        return;
      }
    }
    return;
  }

  // While a dialog is open, swallow gallery / detail keyboard shortcuts so
  // arrow-key grid nav, page navigation, single-key actions, and the chord
  // state machine don't fire under the user's input. Inputs, selects, and
  // textareas inside the dialog still receive their own keystrokes because
  // isInput is true for those targets.
  if (!isInput && document.querySelector('dialog[open]')) {
    // ? closes the shortcuts overlay so the same key is a toggle.
    if (e.key === '?' && document.getElementById('shortcuts-help') && document.getElementById('shortcuts-help').open) {
      e.preventDefault();
      document.getElementById('shortcuts-help').close();
    }
    return;
  }

  if (isInput) return;

  // Chord in progress: this key is the next step. A nested map descends one
  // level (sub-chord); a string navigates; a function runs.
  if (_chordNode) {
    var node = _chordNode;
    var label = _chordLabel;
    var next = node[e.key];
    if (next && typeof next === 'object' && !Array.isArray(next)) {
      e.preventDefault();
      enterChord(next, label + ' ' + e.key);
      return;
    }
    clearChord();
    if (next !== undefined) {
      e.preventDefault();
      if (typeof next === 'string') window.location.href = next;
      else if (Array.isArray(next)) clickFirstSelector(next);
      else next();
      return;
    }
    var digitTargets = chordDigitTargets(node);
    if (digitTargets && /^[1-9]$/.test(e.key)) {
      var picked = digitTargets[parseInt(e.key, 10) - 1];
      if (picked) { e.preventDefault(); picked.click(); return; }
    }
    // Unknown key: fall through so it still does its single-key job.
  }

  if (e.key === '?') {
    var helpDlg = document.getElementById('shortcuts-help');
    if (helpDlg) {
      e.preventDefault();
      if (helpDlg.open) helpDlg.close();
      else openShortcutSheet();
    }
    return;
  }

  if (e.key === 's' || e.key === '/') {
    if (focusFirstSelector(['#search-input', '#sidebar-inner input[name="q"]'])) {
      e.preventDefault();
      return;
    }
  }

  // b → the topbar sidebar toggle, so the key means whichever of the two
  // jobs that button has at the current width.
  if (e.key === 'b') {
    if (clickFirstSelector(['#sidebar-toggle'])) { e.preventDefault(); return; }
  }

  if (e.key === 'Y') {
    var syncForm = document.querySelector('form[hx-post="/internal/sync"]');
    if (syncForm) { e.preventDefault(); syncForm.requestSubmit ? syncForm.requestSubmit() : syncForm.submit(); return; }
  }

  if (e.key === ',') { if (cycleRatingCeiling(-1)) { e.preventDefault(); return; } }
  if (e.key === '.') { if (cycleRatingCeiling(+1)) { e.preventDefault(); return; } }

  if (e.key === '\\') {
    var swDlg = document.getElementById('gallery-switch-dialog');
    if (swDlg) { e.preventDefault(); swDlg.showModal(); return; }
  }

  // Leader: g opens a navigation chord window.
  if (e.key === 'g' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault();
    enterChord(chordMap.g, 'g');
    return;
  }

  // Leader: e opens the detail-page edit chord. Gated on the detail page so
  // non-detail surfaces don't swallow a stray `e`.
  if (e.key === 'e' && !e.ctrlKey && !e.metaKey && !e.altKey && isDetailPage()) {
    e.preventDefault();
    enterChord(chordMap.e, 'e');
    return;
  }

  // Leader: x picks a plugin button by position. Armed only when there are
  // buttons, so the key stays free on every other surface.
  if (e.key === 'x' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    if (chordDigitTargets(chordMap.x).length) {
      e.preventDefault();
      enterChord(chordMap.x, 'x');
      return;
    }
  }

  // Leader: t opens the tag-action chord on the two tag surfaces, where the
  // key is otherwise unbound.
  if (e.key === 't' && !e.ctrlKey && !e.metaKey && !e.altKey && (isTagsPage() || isTagDetailPage())) {
    e.preventDefault();
    enterChord(chordMap.t, 't');
    return;
  }

  // Ctrl/Cmd+A → select every visible thumbnail, or the tags listing's page.
  // Gated on one of the two grids existing so the browser's native
  // select-all-text still works elsewhere.
  if ((e.ctrlKey || e.metaKey) && (e.key === 'a' || e.key === 'A')) {
    if (document.querySelector('.thumb-checkbox')) {
      e.preventDefault();
      selectAll();
      return;
    }
    var tagPageBox = document.getElementById('tag-select-page');
    if (tagPageBox) {
      e.preventDefault();
      if (!tagPageBox.checked) tagPageBox.click();
    }
    return;
  }

  if (isTagsPage()) {
    if (e.key === 'n') {
      if (clickFirstSelector(['#btn-create-tag'])) { e.preventDefault(); return; }
    }
    if (e.key === 'N') {
      if (clickFirstSelector(['#btn-create-alias'])) { e.preventDefault(); return; }
    }
  }

  if (handlePaginationKey(e)) { e.preventDefault(); return; }

  if (isCategoriesPage() && e.key === 'n') {
    if (focusFirstSelector(['.add-cat-form input[name="name"]'])) { e.preventDefault(); return; }
  }

  // Settings page: the digits jump to section anchors, read off the nav so a
  // profile-gated section cannot shift them away from what is listed. 0 is
  // the tenth, which is what the desktop profile's extra entry pushes off
  // the end of 1-9.
  if (isSettingsPage() && /^[0-9]$/.test(e.key)) {
    var nth = e.key === '0' ? 9 : parseInt(e.key, 10) - 1;
    var link = document.querySelectorAll('.settings-nav a')[nth];
    var sec = link && document.querySelector(link.getAttribute('href'));
    if (sec) { e.preventDefault(); sec.scrollIntoView({ behavior: 'smooth', block: 'start' }); return; }
  }

  // f / i toggles on the detail page (favorite / inbox archive).
  // Selection branch opens the batch-favorite dialog before falling through
  // so a selection-mode `f` doesn't also trip the detail-page handler.
  if (e.key === 'f') {
    if (batchBarVisible()) {
      if (typeof openBatchFavoriteDialog === 'function') {
        e.preventDefault(); openBatchFavoriteDialog('selection'); return;
      }
    }
    if (clickFirstSelector(['.btn-fav'])) { e.preventDefault(); return; }
  }

  // 'a' → context-dependent add-tag entry point: the selection's dialog
  // first, then the detail page's tag input.
  if (e.key === 'a' && !e.ctrlKey && !e.metaKey) {
    if (batchBarVisible()) { e.preventDefault(); openTagSelectedDialog(); return; }
    var tagInput = document.getElementById('tag-input');
    if (tagInput) { e.preventDefault(); tagInput.focus(); return; }
  }

  if (e.key === 'r') {
    if (batchBarVisible()) { e.preventDefault(); openStripSelectedDialog(); return; }
    if (isDetailPage()) { e.preventDefault(); enterTagFocusMode(); return; }
  }

  // Manga reader shortcuts. R opens the reader from the detail page or
  // the pages grid (where there's no tag-focus context to clobber).
  // P opens the all-pages grid from the detail page. Both gate on the
  // matching .btn-manga-action anchor being present so non-manga
  // detail pages don't fire on these uppercase keys.
  if (e.key === 'R') {
    var readBtn = document.querySelector('.btn-manga-action.btn-read');
    if (readBtn) { e.preventDefault(); window.location.href = readBtn.href; return; }
    // On /pages the visible Open-in-reader button was dropped as
    // redundant; the keyboard shortcut still has a job, so fall back
    // to the page's image-id data attribute.
    var pagesGrid = document.getElementById('pages-grid-page');
    if (pagesGrid && pagesGrid.dataset.imageId) {
      e.preventDefault();
      window.location.href = '/images/' + pagesGrid.dataset.imageId + '/read?page=1';
      return;
    }
  }
  if (e.key === 'P' && isDetailPage()) {
    var pagesBtn = document.querySelector('.btn-manga-action.btn-pages');
    if (pagesBtn) { e.preventDefault(); window.location.href = pagesBtn.href; return; }
  }
  if (e.key === 'C' && isDetailPage()) {
    if (clickFirstSelector(['.btn-manga-action.btn-generate-collection'])) { e.preventDefault(); return; }
  }

  if (e.key === 'd' && isDetailPage()) {
    if (clickFirstSelector(['.detail-actions a[download]'])) { e.preventDefault(); return; }
  }

  // 'B' → flip the sidebar's tag listing between the category and source
  // groupings, opening the column first like the other keys that reach in.
  if (e.key === 'B' && isDetailPage()) {
    var modeBtn = document.querySelector('.tag-mode-toggle');
    if (modeBtn) { e.preventDefault(); revealSidebarFor(modeBtn); modeBtn.click(); return; }
  }

  // 'v' → open the image viewer. Gated on the still-image branch
  // (the .detail-img-link anchor only renders for non-video / non-manga
  // / not-missing rows), so the key no-ops on every other surface.
  if (e.key === 'v' && isDetailPage()) {
    var lbTrigger = document.querySelector('.detail-img-link');
    if (lbTrigger) { e.preventDefault(); openLightbox(null, lbTrigger.href); return; }
  }

  if (e.key === 't') {
    if (batchBarVisible()) {
      if (!document.querySelector('.btn-autotag')) return;
      e.preventDefault(); openBatchAutotagDialog('selection'); return;
    }
    if (isDetailPage()) {
      if (clickFirstSelector(['.btn-autotag'])) { e.preventDefault(); return; }
    }
  }

  if (e.key === 'm') {
    if (batchBarVisible()) { e.preventDefault(); openBatchPlaceDialog('selection'); return; }
    if (isDetailPage()) {
      var placeDlg = document.getElementById('place-image-dialog');
      if (placeDlg && typeof openPlaceImageDialog === 'function') {
        e.preventDefault(); openPlaceImageDialog(); return;
      }
    }
  }

  if (e.key === 'i') {
    if (batchBarVisible()) {
      if (typeof openBatchInboxDialog === 'function') {
        e.preventDefault(); openBatchInboxDialog('selection'); return;
      }
    }
    if (isDetailPage()) {
      if (clickFirstSelector(['.btn-inbox'])) { e.preventDefault(); return; }
    }
  }

  // 'L' → monloader lookup: batch dialog on a selection, or the detail
  // page's Find-tags-online button (which confirms, then runs the online
  // lookup). Both gate on the paired-only control being rendered.
  if (e.key === 'c' && batchBarVisible()) {
    if (typeof openBatchCollectionDialog === 'function') {
      e.preventDefault(); openBatchCollectionDialog('selection'); return;
    }
  }

  if (e.key === 'T' && batchBarVisible()) {
    if (clickFirstSelector(['#batch-transfer'])) { e.preventDefault(); return; }
  }

  // Invert and the whole-search escalation are the two the operator reaches
  // for with nothing picked yet, so they follow the controls' own state
  // rather than there being a selection.
  if (e.key === 'I' && selectionArmed()) {
    if (clickFirstSelector(['#batch-invert'])) { e.preventDefault(); return; }
  }

  if (e.key === 'A' && !e.ctrlKey && !e.metaKey) {
    if (clickFirstSelector(['#batch-select-all-matching:not([hidden])'])) { e.preventDefault(); return; }
  }

  if (e.key === 'L') {
    if (batchBarVisible()) {
      if (document.querySelector('#batch-bar .monloader-accent')) {
        e.preventDefault(); openBatchLookupDialog('selection'); return;
      }
    }
    if (clickFirstSelector(['.add-tag-form .btn-hash-lookup'])) { e.preventDefault(); return; }
  }

  // Gallery view-level toggles (Shift modifiers).
  if (isGalleryPage()) {
    if (e.key === 'F') {
      if (clickFirstSelector(['#fav-filter-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === 'R') {
      if (clickFirstSelector(['#random-sort-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === 'V') {
      if (clickFirstSelector(['#select-mode-btn'])) { e.preventDefault(); return; }
    }
    if (e.key === '-' || e.key === '=') {
      if (stepThumbSize(e.key === '=' ? 1 : -1)) { e.preventDefault(); return; }
    }
    if (e.key === 'O') { if (cycleSort()) { e.preventDefault(); return; } }
    if (e.key === 'D') { if (flipSortDirection()) { e.preventDefault(); return; } }
    if (e.key === 'S' && !batchBarVisible()) {
      if (openSaveSearchDialog()) { e.preventDefault(); return; }
    }
    if (e.key === 'Home') { if (jumpGridCursor('first')) { e.preventDefault(); return; } }
    if (e.key === 'End')  { if (jumpGridCursor('last'))  { e.preventDefault(); return; } }
  }

  if (e.key === 'Delete' || e.key === 'Del') {
    if (batchBarVisible()) {
      e.preventDefault();
      if (typeof batchDeleteSelected === 'function') batchDeleteSelected();
      return;
    }
    if (clickFirstSelector([
      '#delete-image-btn',
      '#tag-batch-bar:not([hidden]) #btn-batch-delete',
      '#btn-detail-delete',
    ])) { e.preventDefault(); return; }
  }

  if (e.key === ' ') {
    var vid = document.querySelector('.detail-video');
    if (vid) {
      e.preventDefault();
      if (vid.paused) vid.play(); else vid.pause();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused');
    if (focusedCard) {
      var cb = focusedCard.querySelector('.thumb-checkbox');
      if (cb) {
        e.preventDefault();
        cb.checked = !cb.checked;
        pickCheckbox(cb);
        writeSelection();
        updateBatchBar();
      }
      return;
    }
    // The tags listing keeps its selection wiring in its own page script, so
    // the click is what carries the toggle there.
    var focusedRow = document.querySelector('.tag-row.focused');
    if (focusedRow) {
      var rowBox = focusedRow.querySelector('.tag-select');
      if (rowBox) { e.preventDefault(); rowBox.click(); }
    }
    return;
  }

  if (e.key === 'Backspace' && isDetailPage()) {
    e.preventDefault();
    detailBack();
    return;
  }
  if (e.key === 'Backspace' && isTagDetailPage()) {
    var tdBack = document.getElementById('tag-detail-page');
    e.preventDefault();
    window.location.href = (tdBack && tdBack.dataset.escBack) || '/tags';
    return;
  }

  if (e.key === 'o') {
    if (isDetailPage()) {
      var dlA = document.querySelector('.detail-actions a[download]');
      if (dlA) { e.preventDefault(); window.open(dlA.href, '_blank'); return; }
    } else {
      var foc = document.querySelector('.thumb-card.focused a');
      if (foc) { e.preventDefault(); window.location.href = foc.href; return; }
    }
  }

  // h j k l and the arrow keys route the same way: tag-focus cycle, then
  // detail prev/next, then a gallery grid move (h ← l → k ↑ j ↓).
  var navStep = {
    h: [-1, 0], l: [1, 0], k: [0, -1], j: [0, 1],
    ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1],
  }[e.key];
  if (navStep) {
    var dx = navStep[0], dy = navStep[1];
    var forward = dx + dy > 0;
    var arrow = e.key.startsWith('Arrow');
    if (focusedTagRow()) {
      e.preventDefault();
      cycleTagFocus(forward ? 1 : -1);
      return;
    }
    var kind = isDetailPage() ? 'image' : isTagDetailPage() ? 'tag' : '';
    if (kind) {
      // An arrow stops on a detail page whatever it hits, since up and
      // down have nowhere to go there; j and k fall through so the rest
      // of the map still sees them.
      if ((!arrow || dy === 0) && navDetailArrow(forward ? 'next' : 'prev', kind)) {
        e.preventDefault();
        return;
      }
      if (arrow) return;
    } else if (isGalleryPage() && moveGridCursor(dx, dy)) {
      e.preventDefault();
      return;
    } else if (isTagsPage() && dy !== 0 && moveTagRowCursor(dy)) {
      e.preventDefault();
      return;
    }
  }

  if (e.key === 'Enter') {
    var focusedTag = focusedTagRow();
    if (focusedTag) {
      e.preventDefault();
      // Stash the focused tag's flat-list index so the htmx:afterSettle
      // swap below can land the cursor on the previous sibling instead
      // of dropping back to index 0 (the natural restart point with no
      // .focused element to anchor to).
      var idx = tagFocusRows().indexOf(focusedTag);
      if (idx >= 0) document.body.dataset.tagFocusIdx = String(idx);
      var tagBtn = focusedTag.querySelector('.tag-entry-remove');
      if (tagBtn) tagBtn.click();
      return;
    }
    var focusedCard = document.querySelector('.thumb-card.focused a');
    if (focusedCard) { window.location.href = focusedCard.href; return; }
    var tagLink = document.querySelector('.tag-row.focused .btn-see-detail');
    if (tagLink) { window.location.href = tagLink.href; return; }
  }
});

// On a similar-click detail page (marked by data-ref), the "← Previous image"
// link walks browser history instead of navigating to its href so chains of
// any depth unwind one page at a time - matching the Escape keybinding. The
// href stays wired for cold loads (direct URL, bookmarked tab) where history
// has no predecessor.
document.addEventListener('click', function(e) {
  var link = e.target.closest('.back-link');
  if (!link) return;
  if (detailRefBack()) e.preventDefault();
});

// Delete-from-ref walks history instead of server-redirecting so the chain
// (A -> B -> C -> delete) lands back on B's original URL (with its own
// ref=A intact), leaving Escape free to unwind the rest of the chain.
// HX-Redirect would push a fresh history entry and silently drop the ref
// chain. The event's detail.fallback carries the redirect URL the handler
// would otherwise have set; we fall back to it when history has no
// predecessor (direct link, fresh tab).
document.body.addEventListener('delete-go-back', function(e) {
  if (history.length > 1) { history.back(); return; }
  var fallback = e.detail && e.detail.fallback;
  if (fallback) window.location.href = fallback;
});

// Per-tagger config dialogs (thresholds, galleries) and the per-token
// privileges dialog close themselves on a successful save. The server fires
// `tagger-saved` / `token-saved` via HX-Trigger and names the dialog id to
// close; the parent flash and the row updates arrive as OOB swaps so the
// page state is already updated by the time this listener runs.
function closeDialogFromSaveEvent(e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
}
document.body.addEventListener('tagger-saved', closeDialogFromSaveEvent);
document.body.addEventListener('token-saved', closeDialogFromSaveEvent);

// The dialog's rows are a diff against the repository, which is a network
// call away: the pop-in opens on the click so the wait is inside it rather
// than in front of a button that looks untouched, and the modal is also
// what stops a second click landing on the same fetch.
function ptrContribOpen() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) load.showModal();
}

// The answer takes the wait's place. Escape during the wait is a cancel:
// the answer is dropped rather than popping up after the fact.
function ptrContribOpened() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) return;
  if (load) load.close();
  var dlg = document.getElementById('ptr-contrib-dialog');
  if (dlg) dlg.showModal();
  else htmx.trigger(document.body, 'ptr-contrib-closed');
}

// PTR contribute dialogs (image tags and tag relations). The dialog is
// injected fresh on every open, so all wiring is delegated. Both dialogs
// share the same skeleton; the form's data-verb/data-noun carry the only
// wording difference, and each hunk's optional [data-reason] field
// reveals while that hunk has ticked rows.
function ptrContribRefresh(form) {
  var counts = [];
  form.querySelectorAll('.ptr-hunk-wrap').forEach(function(w) {
    var boxes = w.querySelectorAll('.ptr-row input[type="checkbox"]');
    var n = w.querySelectorAll('.ptr-row input[type="checkbox"]:checked').length;
    counts.push(n);
    var live = w.querySelector('.ptr-live');
    if (live) live.textContent = n + '/' + boxes.length;
    var box = w.querySelector('[data-reason]');
    if (box) {
      var was = box.hidden;
      box.hidden = n === 0;
      var input = box.querySelector('input');
      if (input) input.required = n > 0;
      if (n > 0 && was && input) input.focus();
    }
  });
  var adds = counts[0] || 0, pets = counts[1] || 0;
  var noun = form.dataset.noun, parts = [];
  if (adds) parts.push(form.dataset.verb + ' ' + adds + ' ' + noun + (adds > 1 ? 's' : ''));
  if (pets) parts.push('petition the removal of ' + pets + ' ' + noun + (pets > 1 ? 's' : ''));
  var summary = form.querySelector('#ptr-contrib-summary');
  if (summary) summary.textContent = parts.length ? 'asks to ' + parts.join(' and ') + '.' : 'nothing selected yet.';
  var send = form.querySelector('#ptr-contrib-send');
  if (send) send.disabled = (adds + pets) === 0;
}

document.body.addEventListener('change', function(e) {
  var form = e.target.closest && e.target.closest('#ptr-contrib-form');
  if (form) ptrContribRefresh(form);
});

document.addEventListener('click', function(e) {
  if (!e.target.closest) return;
  var form = e.target.closest('#ptr-contrib-form');
  if (!form) return;
  var bulk = e.target.closest('[data-bulk]');
  if (bulk) {
    bulk.closest('.ptr-hunk-wrap').querySelectorAll('label.ptr-row input[type="checkbox"]').forEach(function(c) {
      c.checked = bulk.dataset.bulk === 'all';
    });
    ptrContribRefresh(form);
    return;
  }
  // Shift-click paints the contiguous range with the anchor row's state.
  // The handler keys on the label because the activation click a label
  // forwards to its control drops the modifier flags.
  var row = e.target.closest('label.ptr-row');
  if (!row) return;
  var wrap = row.closest('.ptr-hunk-wrap');
  var rows = Array.prototype.slice.call(wrap.querySelectorAll('label.ptr-row'));
  var i = rows.indexOf(row);
  var last = wrap.ptrContribAnchor;
  if (e.shiftKey && last !== undefined && last !== i) {
    e.preventDefault();
    var state = rows[last].querySelector('input[type="checkbox"]').checked;
    var from = Math.min(last, i), to = Math.max(last, i);
    for (var k = from; k <= to; k++) {
      rows[k].querySelector('input[type="checkbox"]').checked = state;
    }
    ptrContribRefresh(form);
  }
  wrap.ptrContribAnchor = i;
});

// Closing the contribute dialog re-fetches the panel behind it, so a
// summary the send just outdated never lingers. Captured because the
// dialog close event does not bubble.
document.addEventListener('close', function(e) {
  if (e.target.id === 'ptr-contrib-dialog') htmx.trigger(document.body, 'ptr-contrib-closed');
}, true);

// Tab strips inside dialogs (the per-tagger Configure pop-in). Tabs
// only toggle panel visibility within their own form; hidden panels'
// fields still submit, so one Save covers every tab. Delegated so
// htmx-injected dialog bodies need no wiring.
document.body.addEventListener('click', function(e) {
  var tab = e.target.closest && e.target.closest('.dialog-tab');
  if (!tab || !tab.dataset.panel) return;
  var strip = tab.closest('.dialog-tabs');
  var scope = strip && strip.parentElement;
  if (!scope) return;
  strip.querySelectorAll('.dialog-tab').forEach(function(t) {
    t.classList.toggle('active', t === tab);
  });
  scope.querySelectorAll('.dialog-panel').forEach(function(p) {
    p.hidden = p.id !== tab.dataset.panel;
  });
});

// The mappings search box is a text input inside the dialog's config
// form, so Enter would implicitly submit it - saving the tagger and
// refreshing the page out from under the open dialog. htmx already
// re-runs the search on the `search` event, so swallowing the submit
// leaves Enter meaning "search now".
document.body.addEventListener('keydown', function(e) {
  if (e.key !== 'Enter') return;
  var el = e.target;
  if (el && el.name === 'q' && el.closest && el.closest('.tc-mappings')) {
    e.preventDefault();
  }
});

// Mappings panel of the tagger Configure dialog. Apply and Reset are
// htmx buttons posting one rule each and swapping the result list back;
// this handler only owns the editor row's lifecycle (open, seed, close)
// and the search-row buttons.
document.body.addEventListener('click', function(e) {
  var btn = e.target.closest && e.target.closest('.tc-edit, .tc-reset, .tc-edit-cancel, .tc-filter, .tc-more-btn');
  if (!btn) return;
  var panel = btn.closest('.tc-mappings');
  if (!panel) return;
  // The search row carries q, filter and limit, so re-triggering the
  // search input re-renders the list with whatever the buttons set.
  var search = panel.querySelector('input[name=q]');
  var limit = panel.querySelector('input[name=limit]');

  if (btn.classList.contains('tc-filter')) {
    panel.querySelectorAll('.tc-filter').forEach(function(f) { f.classList.toggle('active', f === btn); });
    var hidden = panel.querySelector('input[name=filter]');
    if (hidden) hidden.value = btn.dataset.filter;
    if (search) htmx.trigger(search, 'search');
    return;
  }

  if (btn.classList.contains('tc-more-btn')) {
    if (limit) limit.value = btn.dataset.limit;
    if (search) htmx.trigger(search, 'search');
    return;
  }

  // One editor at a time; any action first closes the open one.
  var open = panel.querySelector('.tc-editor-row');
  if (open) open.remove();

  if (!btn.classList.contains('tc-edit')) return;

  // Edit: build the editor row from the panel's template, seeded from
  // the row's current state. The Apply button carries the label as its
  // own name/value so the post identifies the row it came from.
  var row = btn.closest('tr');
  var tpl = panel.querySelector('.tc-editor-template');
  if (!row || !tpl) return;
  var editor = tpl.content.firstElementChild.cloneNode(true);
  var apply = editor.querySelector('.tc-edit-apply');
  apply.value = row.dataset.source;
  if (row.dataset.category) editor.querySelector('.tc-edit-category').value = row.dataset.category;
  editor.querySelector('.tc-edit-name').value =
    row.dataset.name !== row.dataset.source ? row.dataset.name : '';
  editor.querySelector('.tc-edit-mute').checked = row.dataset.muted === '1';
  // Enter inside the editor applies the edit. Left alone it would
  // submit the dialog's form instead, closing the whole pop-in.
  editor.addEventListener('keydown', function(ev) {
    if (ev.key !== 'Enter') return;
    ev.preventDefault();
    apply.click();
  });
  row.after(editor);
  htmx.process(editor);
});

// Per-tagger Galleries dialog helpers. taggerGalAllToggle disables and
// force-checks the per-gallery boxes when "All galleries" is on so the
// submitted state matches the rendered state. taggerGalSelect mass-sets
// the per-gallery boxes (used by "Select all" / "Select none").
function taggerGalAllToggle(cb) {
  var form = cb.closest('form');
  if (!form) return;
  form.querySelectorAll('input[name=gallery_names]').forEach(function(c) {
    c.disabled = cb.checked;
    if (cb.checked) c.checked = true;
  });
}

function taggerGalSelect(btn, on) {
  var form = btn.closest('form');
  if (!form) return;
  var allCb = form.querySelector('input[name=all]');
  if (allCb && allCb.checked) {
    allCb.checked = false;
    taggerGalAllToggle(allCb);
  }
  form.querySelectorAll('input[name=gallery_names]').forEach(function(c) {
    c.checked = !!on;
  });
}

// "At default" means every number input equals its data-default (empty
// when the catalog seeds none) and the checkbox matches its
// data-default-checked state.
function threshRowAtDefault(row) {
  var atDefault = true;
  row.querySelectorAll('input[type=number]').forEach(function(inp) {
    if (inp.value !== (inp.dataset.default || '')) atDefault = false;
  });
  var cb = row.querySelector('input[type=checkbox]');
  if (cb && cb.checked !== (cb.dataset.defaultChecked === '1')) atDefault = false;
  return atDefault;
}

function syncThreshRow(row) {
  if (!row) return;
  var reset = row.querySelector('.reset-thresh');
  // Show the Reset only when the row differs from its catalog default
  // (any number off its default, or the category disabled). Toggle a
  // visibility class rather than the hidden attribute so the cell keeps
  // reserving the link's width and the column doesn't reflow.
  if (reset) reset.classList.toggle('reset-thresh-shown', !threshRowAtDefault(row));
}

// Reset one row to its catalog defaults, then re-sync so the link hides
// itself now that the row sits back at its default.
function resetThreshRow(link) {
  var row = link.closest('tr');
  if (!row) return;
  row.querySelectorAll('input[type=number]').forEach(function(inp) {
    inp.value = inp.dataset.default || '';
  });
  var cb = row.querySelector('input[type=checkbox]');
  if (cb) cb.checked = cb.dataset.defaultChecked === '1';
  syncThreshRow(row);
}

// Re-sync a row as the operator edits it (one body bound to both input and
// change so number typing and checkbox toggles are covered), and run an
// initial pass when the dialog body lazy-loads (or re-renders after "Reset
// to defaults"): htmx swaps the table into #tagger-thresh-<name>-body.
function syncThreshRowFromEvent(e) {
  var row = e.target.closest && e.target.closest('.tagger-thresh-table tbody tr');
  if (row) syncThreshRow(row);
}
document.body.addEventListener('input', syncThreshRowFromEvent);
document.body.addEventListener('change', syncThreshRowFromEvent);
document.body.addEventListener('htmx:afterSwap', function(e) {
  var t = e.detail && e.detail.target;
  if (!t) return;
  var table = t.querySelector && t.querySelector('.tagger-thresh-table');
  if (table) table.querySelectorAll('tbody tr').forEach(syncThreshRow);
});

// Returning from a detail page via a back-link with #img-N restores the
// arrow-key cursor on the matching thumbnail.
function restoreGalleryFocusFromHash() {
  var m = window.location.hash.match(/^#img-(\d+)$/);
  if (!m) return;
  var card = document.querySelector('.thumb-card[data-id="' + m[1] + '"]');
  if (!card) return;
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  setFocused(cards, cards.indexOf(card));
}
document.addEventListener('DOMContentLoaded', restoreGalleryFocusFromHash);

// Video hover preview swap (with error fallback to avoid "?" on hover fail)
document.addEventListener('mouseover', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  const hoverSrc = card.dataset.hover;
  if (!img || !hoverSrc || img.dataset.hovering) return;
  img.dataset.orig = img.src;
  img.dataset.hovering = '1';
  img.onerror = function() {
    img.src = img.dataset.orig || '';
    delete img.dataset.orig;
    delete img.dataset.hovering;
    img.onerror = null;
  };
  img.src = hoverSrc;
});

document.addEventListener('mouseout', function(e) {
  const card = e.target.closest('.thumb-card');
  if (!card) return;
  const img = card.querySelector('.thumb-img');
  if (!img || !img.dataset.orig) return;
  img.src = img.dataset.orig;
  delete img.dataset.orig;
  delete img.dataset.hovering;
  img.onerror = null;
});

// Tag-input (detail page) clears the invalid-tag flash as soon as the user
// starts fixing the input, so the error isn't stuck on-screen through the
// next submit.
document.addEventListener('input', function(e) {
  if (e.target.id !== 'tag-input') return;
  var tagsDiv = document.getElementById('image-tags');
  if (!tagsDiv) return;
  var err = tagsDiv.querySelector('.flash-err');
  if (err) err.remove();
});

// The tags/collections live search swaps only the page region and leaves
// the sidebar alone, so the filter links keep the q from the last sidebar
// render. Boosted links capture their path when the sidebar is processed,
// so the request path is re-synced at request time; the hrefs are kept
// honest too for open-in-new-tab.
function sidebarFilterQ(url) {
  const input = document.querySelector('#sidebar-inner form input[name=q]');
  if (!input) return null;
  url.searchParams.set('q', input.value);
  return url.pathname + '?' + url.searchParams.toString();
}

document.body.addEventListener('htmx:configRequest', function(e) {
  const elt = e.detail.elt;
  if (!elt || !elt.matches || !elt.matches('#sidebar-inner a.filter-btn')) return;
  const path = sidebarFilterQ(new URL(e.detail.path, window.location.origin));
  if (path) e.detail.path = path;
});

// htmx skips swapping error responses; the job-conflict 409s carry the
// inline "already running" flash as their body, so let those render.
document.body.addEventListener('htmx:beforeSwap', function(e) {
  if (e.detail.xhr && e.detail.xhr.status === 409) e.detail.shouldSwap = true;
});

document.addEventListener('input', function(e) {
  if (!e.target.matches || !e.target.matches('#sidebar-inner form input[name=q]')) return;
  document.querySelectorAll('#sidebar-inner a.filter-btn').forEach(function(a) {
    const href = sidebarFilterQ(new URL(a.getAttribute('href'), window.location.origin));
    if (href) a.setAttribute('href', href);
  });
});

// Sidebar tag-add-btn: toggle tag in/out of the current search query.
// The sidebar emits the term as "category:name"; the query may carry
// either that form or the bare name, so we drop both on remove.
document.addEventListener('click', function(e) {
  const btn = e.target.closest('.tag-add-btn');
  if (!btn) return;
  e.preventDefault();
  const tagName = btn.dataset.tag;
  if (!tagName) return;
  const si = document.getElementById('search-input');
  if (!si) return;
  const colon = tagName.indexOf(':');
  const bare = colon >= 0 ? tagName.slice(colon + 1) : tagName;
  const terms = si.value.trim().split(/\s+/).filter(Boolean);
  const filtered = terms.filter(t => t !== tagName && t !== bare);
  if (filtered.length === terms.length) filtered.push(tagName);
  si.value = filtered.join(' ');
  const form = document.getElementById('search-form');
  if (form && window.htmx) window.htmx.trigger(form, 'submit');
  else if (form) form.submit();
});

// Category collapse in sidebar. Listening on the whole header (not just
// the ▾) keeps the click target away from the sidebar scrollbar at the
// right edge.
document.addEventListener('click', function(e) {
  const header = e.target.closest('.tag-group-header');
  if (!header) return;
  const group = header.closest('.tag-group');
  if (!group) return;
  const list = group.querySelector('.tag-list-sidebar');
  if (!list) return;
  const indicator = group.querySelector('.cat-collapse');
  const collapsed = list.style.display === 'none';
  list.style.display = collapsed ? '' : 'none';
  if (indicator) indicator.textContent = collapsed ? '▾' : '▸';
});

// Batch selection: show/hide batch bar and keep checkboxes visible. The
// blur drops focus off the checkbox after the toggle so the keydown
// router's input-focused guard (`if (isInput) return`) doesn't swallow
// the selection shortcuts (a, f, r, t, m, i, Delete) on the next press.
document.addEventListener('change', function(e) {
  if (!e.target.classList.contains('thumb-checkbox')) return;
  pickCheckbox(e.target);
  writeSelection();
  updateBatchBar();
  e.target.blur();
});

// forEachClusterCheckbox walks forward through the grid's children after a
// cluster header, visiting every thumbnail checkbox until the next cluster
// header (or the end). Shared by the [Select]/[Unselect] click below and
// updateClusterButtons' visibility sync.
function forEachClusterCheckbox(header, fn) {
  var node = header.nextElementSibling;
  while (node && !node.classList.contains('thumb-cluster-header')) {
    var cb = node.querySelector ? node.querySelector('.thumb-checkbox') : null;
    if (cb) fn(cb);
    node = node.nextElementSibling;
  }
}

// Inbox cluster [Select] / [Unselect] buttons: tick or untick every
// thumbnail checkbox in the cluster; the only difference between the two
// buttons is the target checked state.
document.addEventListener('click', function(e) {
  var sel = e.target.closest('[data-cluster-select]');
  var uns = e.target.closest('[data-cluster-unselect]');
  if (!sel && !uns) return;
  e.preventDefault();
  var header = (sel || uns).closest('.thumb-cluster-header');
  if (!header) return;
  var target = !!sel;
  forEachClusterCheckbox(header, function(cb) { cb.checked = target; pickCheckbox(cb); });
  writeSelection();
  updateBatchBar();
});

// [Select all] takes the batch whole, tail included. A cluster's own time
// range is a search of exactly its rows, so the ids come from the server and
// join the pick where it stands - the listing on screen does not move.
document.addEventListener('click', function(e) {
  var btn = e.target.closest ? e.target.closest('[data-cluster-all]') : null;
  if (!btn) return;
  e.preventDefault();
  btn.disabled = true;
  fetch('/internal/search/ids?q=' + encodeURIComponent(btn.dataset.clusterAll), {
    headers: {'Accept': 'application/json'},
  }).then(function(res) {
    if (!res.ok) throw new Error('ids');
    return res.json();
  }).then(function(data) {
    var ids = data.ids.map(String);
    ids.forEach(function(v) {
      if (pickedIDs.indexOf(v) === -1) pickedIDs.push(v);
    });
    var header = btn.closest('.thumb-cluster-header');
    if (header) header.clusterAllIDs = ids;
    selectAllMatching = false;
    writeSelection();
    syncSelectionBoxes();
    updateBatchBar();
    if (data.truncated) {
      setFlashText(document.getElementById('gallery-flash'), 'err',
        'Batch too large: the first ' + ids.length + ' are selected.');
    }
  }).catch(function() {
    setFlashText(document.getElementById('gallery-flash'), 'err', 'Could not read the batch.');
  }).finally(function() {
    btn.disabled = false;
  });
});

// The card a range is measured from: the last one clicked, so shift-click
// paints the run between it and the next, the way the /tags rows do.
var lastPickedCard = null;

// While the grid is armed, a left-click on a thumbnail toggles its checkbox
// instead of opening the detail page, shift-click takes the run since the
// last one, and Ctrl/cmd does what a plain click does - a modifier that
// opens a tab out of a selection is not what it means anywhere else. Alt
// stays out of it (the band reads it as "subtract") and so does the middle
// click. Esc / Cancel disarms and the link goes back to navigating.
document.addEventListener('click', function(e) {
  if (e.button !== 0 || e.altKey) return;
  var grid = document.getElementById('gallery-grid');
  if (!grid || !grid.classList.contains('batch-active')) return;
  var link = e.target.closest('.thumb-link');
  if (!link) return;
  var card = link.closest('.thumb-card');
  if (!card) return;
  var cb = card.querySelector('.thumb-checkbox');
  if (!cb) return;
  e.preventDefault();
  var cards = Array.prototype.slice.call(grid.querySelectorAll('.thumb-card'));
  var to = cards.indexOf(card);
  var from = lastPickedCard ? cards.indexOf(lastPickedCard) : -1;
  if (e.shiftKey && from !== -1 && from !== to) {
    // The anchor's own state is what the run is painted with, so a
    // shift-click can clear a run as readily as it can take one.
    var want = !cb.checked;
    cards.slice(Math.min(from, to), Math.max(from, to) + 1).forEach(function(c) {
      var box = c.querySelector('.thumb-checkbox');
      if (!box) return;
      box.checked = want;
      pickCheckbox(box);
    });
  } else {
    cb.checked = !cb.checked;
    pickCheckbox(cb);
  }
  lastPickedCard = card;
  writeSelection();
  updateBatchBar();
});

// Rubber-band selection. A run of images is the unit an import is judged
// in, and ticking each one is the slow way to say so. A drag adds to the
// page's picks and Alt takes them away, so several sweeps build one
// selection and a drag nobody meant to start cannot wipe one. Picks on
// other pages are untouched - the band only reaches what is on screen.
var marquee = null;

function marqueeCells(grid, origin) {
  return Array.prototype.map.call(grid.querySelectorAll('.thumb-card'), function(card) {
    var r = card.getBoundingClientRect();
    return {
      cb: card.querySelector('.thumb-checkbox'),
      x: r.left - origin.left, y: r.top - origin.top, w: r.width, h: r.height,
    };
  });
}

function marqueePaint() {
  var m = marquee;
  var x = Math.min(m.x0, m.x1), y = Math.min(m.y0, m.y1);
  var w = Math.abs(m.x1 - m.x0), h = Math.abs(m.y1 - m.y0);
  if (!m.box) {
    m.box = document.createElement('div');
    m.box.className = 'marquee';
    m.grid.appendChild(m.box);
  }
  m.box.style.left = x + 'px';
  m.box.style.top = y + 'px';
  m.box.style.width = w + 'px';
  m.box.style.height = h + 'px';
  m.cells.forEach(function(c, i) {
    if (!c.cb) return;
    var hit = !(c.x > x + w || c.x + c.w < x || c.y > y + h || c.y + c.h < y);
    var on = m.mode === 'add' ? (m.was[i] || hit) : (m.was[i] && !hit);
    if (c.cb.checked !== on) {
      c.cb.checked = on;
      pickCheckbox(c.cb);
    }
  });
  updateBatchBar();
}

document.addEventListener('mousedown', function(e) {
  if (e.button !== 0 || window.matchMedia('(pointer: coarse)').matches) return;
  if (!e.target.closest) return;
  var grid = e.target.closest('.thumb-grid');
  // The checkbox and the cluster buttons are controls of their own; the
  // card's link is not, or the band could only start in the gaps.
  if (!grid || e.target.closest('button, input')) return;
  var origin = grid.getBoundingClientRect();
  marquee = {
    grid: grid, box: null, moved: false,
    x0: e.clientX - origin.left, y0: e.clientY - origin.top,
    x1: e.clientX - origin.left, y1: e.clientY - origin.top,
    mode: e.altKey ? 'sub' : 'add',
    cells: marqueeCells(grid, origin),
  };
  marquee.was = marquee.cells.map(function(c) { return !!(c.cb && c.cb.checked); });
  // Without this the drag turns into a native link drag or a text selection.
  e.preventDefault();
  document.body.classList.add('marquee-active');
});

document.addEventListener('mousemove', function(e) {
  if (!marquee) return;
  // Re-read the origin every move so a grid that scrolled under the pointer
  // keeps the band anchored to the cards rather than to the viewport.
  var origin = marquee.grid.getBoundingClientRect();
  marquee.x1 = e.clientX - origin.left;
  marquee.y1 = e.clientY - origin.top;
  if (Math.abs(marquee.x1 - marquee.x0) > 4 || Math.abs(marquee.y1 - marquee.y0) > 4) {
    marquee.moved = true;
  }
  // Nothing is painted until the pointer clears the threshold: a click carries
  // a pixel or two of hand movement, and a band that thin in 'set' mode drops
  // every pick outside it.
  if (!marquee.moved) return;
  var content = document.getElementById('content');
  if (content) {
    var cr = content.getBoundingClientRect();
    if (e.clientY < cr.top + 40) content.scrollTop -= 12;
    else if (e.clientY > cr.bottom - 40) content.scrollTop += 12;
  }
  marqueePaint();
});

document.addEventListener('mouseup', function() {
  if (!marquee) return;
  var moved = marquee.moved;
  if (marquee.box) marquee.box.remove();
  marquee = null;
  document.body.classList.remove('marquee-active');
  writeSelection();
  updateBatchBar();
  // The release fires a click on whatever card is under it, which would
  // undo the last cell the band took.
  if (moved) marqueeSwallowUntil = Date.now() + 100;
});

// Set by a drag that actually moved; the click handlers above read it.
var marqueeSwallowUntil = 0;

document.addEventListener('click', function(e) {
  if (Date.now() >= marqueeSwallowUntil) return;
  if (!e.target.closest || !e.target.closest('.thumb-grid')) return;
  e.preventDefault();
  e.stopPropagation();
}, true);

// The ids [Select all] would add, when they are known: the cluster's own
// rows when the batch is wholly on this page, else whatever the endpoint
// answered on an earlier click. Null while a batch with a tail elsewhere
// has not been taken yet - the page cannot tell what it is missing.
function clusterAllIDs(header) {
  if (header.clusterAllIDs) return header.clusterAllIDs;
  if (!header.hasAttribute('data-cluster-whole')) return null;
  var ids = [];
  forEachClusterCheckbox(header, function(cb) { ids.push(cb.value); });
  return ids;
}

// In the inbox, hide a cluster's [Unselect] when nothing in it is selected,
// [Select] when all of it is, and [Select all] once the pick already holds
// the whole batch - the same sibling-walk as the click above.
function updateClusterButtons() {
  document.querySelectorAll('.thumb-cluster-header[data-cluster-start]').forEach(function(header) {
    var total = 0, checked = 0;
    forEachClusterCheckbox(header, function(cb) {
      total++;
      if (cb.checked) checked++;
    });
    var sel = header.querySelector('[data-cluster-select]');
    var uns = header.querySelector('[data-cluster-unselect]');
    var all = header.querySelector('[data-cluster-all]');
    if (sel) sel.hidden = total > 0 && checked === total;
    if (uns) uns.hidden = checked === 0;
    if (all) {
      var ids = clusterAllIDs(header);
      all.hidden = !!ids && ids.length > 0 && ids.every(function(id) {
        return pickedIDs.indexOf(id) !== -1;
      });
    }
  });
}

// The selection is a list of ids rather than whatever is ticked right now:
// paging the grid swaps the checkboxes away, and a group worth acting on -
// a collection, an import batch - routinely spans pages. Pick order is the
// order a job that numbers its scope walks it in, so an id already on the
// list keeps its place.
var pickedIDs = [];
// Set by the "Select all N matching" escalation: the ids stop being the
// scope and the server re-resolves the query instead.
var selectAllMatching = false;
// Arms the grid before there is anything picked, so the first image is taken
// by a click rather than by finding its checkbox. A selection arms it too.
var selectMode = false;

// selectionArmed is the one state the grid, the bar and the toggle read: the
// mode the operator asked for, or a selection that put them in it anyway.
function selectionArmed() {
  return selectMode || selectAllMatching || pickedIDs.length > 0;
}

// Leaving the mode takes the selection with it - the toggle reads as armed
// once anything is picked, so switching it off has to mean what it shows.
function toggleSelectMode() {
  if (selectionArmed()) {
    selectMode = false;
    clearSelection();
    return;
  }
  selectMode = true;
  updateBatchBar();
}

// Records one box's state. Callers persist once they are done, so ticking a
// whole page is a single write rather than one per thumbnail.
function pickCheckbox(cb) {
  var at = pickedIDs.indexOf(cb.value);
  if (cb.checked) {
    if (at === -1) pickedIDs.push(cb.value);
  } else if (at !== -1) {
    pickedIDs.splice(at, 1);
  }
  // Any single toggle is the operator narrowing again, which the whole-search
  // scope can no longer describe.
  selectAllMatching = false;
}

// syncSelectionBoxes ticks the boxes the fresh grid arrived with from the
// list. Every box is set, not just the ones the list names: a browser that
// keeps form state across a reload restores it by position, which after an
// action that dropped rows points at whichever rows moved up into their
// places. A whole-search scope reaches every row on screen, so it takes
// their ids too - which is what a toggle narrows back to.
function syncSelectionBoxes() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    var picked = pickedIDs.indexOf(cb.value) !== -1;
    if (selectAllMatching && !picked) {
      pickedIDs.push(cb.value);
      picked = true;
    }
    cb.checked = picked;
  });
}

function updateBatchBar() {
  const onPage = document.querySelectorAll('.thumb-checkbox:checked').length;
  const total = document.querySelectorAll('.thumb-checkbox').length;
  const active = selectAllMatching || pickedIDs.length > 0;
  const armed = selectionArmed();
  const bar = document.getElementById('batch-bar');
  const pluginBar = document.getElementById('plugin-batch-bar');
  const grid = document.getElementById('gallery-grid');
  if (bar) {
    // Up with the mode, lit with the selection: the .visible class is what
    // the keyboard router reads to know there is a scope to act on.
    bar.hidden = !armed;
    bar.classList.toggle('visible', active);
  }
  // Up with the mode like the bar above it; lit only while the scope is a
  // list of ids it can post, which the whole-search escalation is not.
  if (pluginBar) {
    pluginBar.hidden = !armed;
    pluginBar.classList.toggle('visible', active && !selectAllMatching);
  }
  if (grid) grid.classList.toggle('batch-active', armed);
  const modeBtn = document.getElementById('select-mode-btn');
  if (modeBtn) modeBtn.setAttribute('aria-pressed', String(armed));
  const matching = matchingTotal();
  const countEl = document.getElementById('batch-count');
  if (countEl) {
    // Beside the match count now, so it says nothing until there is one.
    countEl.hidden = !active;
    countEl.textContent = selectAllMatching
      ? 'All ' + matching + ' matching selected'
      : pickedIDs.length + ' selected' +
        (pickedIDs.length > onPage ? ' · ' + onPage + ' on this page' : '');
  }
  // Offered once the page is exhausted, so it reads as the next step up from
  // Select page rather than a competing way to pick.
  const escalate = document.getElementById('batch-select-all-matching');
  if (escalate) {
    escalate.textContent = 'Select all ' + matching + ' matching';
    escalate.hidden = selectAllMatching || total === 0 || onPage < total || matching <= total;
  }
  updateClusterButtons();
}

// matchingTotal reads the status bar rather than a template value: the count
// is swapped out of band on every htmx search, and the bar is not.
function matchingTotal() {
  var el = document.querySelector('.result-count');
  var m = el && el.textContent.match(/(\d+)/);
  return m ? parseInt(m[1], 10) : 0;
}

function clearSelection() {
  pickedIDs = [];
  selectAllMatching = false;
  writeSelection();
  syncSelectionBoxes();
  updateBatchBar();
}

// selectAll takes the page, which is as far as the checkboxes reach; the
// escalation beside it is what takes the rest of the search.
function selectAll() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = true;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// Flips the page, not the search: the boxes are what the operator just
// judged, and ids picked on other pages are not on screen to reconsider.
// Triage runs pick the rejects, act, invert, act on what is left.
function invertSelection() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = !cb.checked;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// The stash keeps the selection across a page flip, a detail-page detour and
// the reload a finished job ends in. It names the listing it was made on -
// the query, not the page - so paging keeps it and searching drops it,
// rather than a set built on one listing re-selecting on an unrelated one.
// The window is minutes: a batch reloads when its background job finishes,
// which over a few thousand images is a while after the click.
var selectionStashMs = 300000;

// A gallery is an id space of its own, so every stashed pick is named by the
// one it was made in: after a switch the same URL is a different library.
function activeGallery() {
  return (document.body && document.body.dataset.gallery) || '';
}

// The query alone, from the URL rather than the search box: the box can hold
// something typed but not submitted, while the URL is what the grid on screen
// was built from. Sort and page are deliberately out - they reorder and slice
// the same images, and a pick worth keeping outlives both.
function selectionKey() {
  return activeGallery() + location.pathname + '?q=' + (new URLSearchParams(location.search).get('q') || '');
}

function writeSelection() {
  try {
    sessionStorage.setItem('monbooru_selection', JSON.stringify(
      {ids: pickedIDs, all: selectAllMatching, url: selectionKey(), t: Date.now()}));
  } catch (e) {}
}

// readSelection adopts the stash when it belongs to the listing on screen
// and empties the selection when it does not, which is what makes a page
// flip keep the pick and a new search drop it.
function readSelection() {
  var raw = null;
  try { raw = sessionStorage.getItem('monbooru_selection'); } catch (e) {}
  var stash = null;
  if (raw) {
    try { stash = JSON.parse(raw); } catch (e) {}
  }
  if (!stash || !stash.ids || Date.now() - stash.t > selectionStashMs ||
      stash.url !== selectionKey()) {
    pickedIDs = [];
    selectAllMatching = false;
    return;
  }
  pickedIDs = stash.ids;
  selectAllMatching = !!stash.all;
}

// stashSelection carries the /tags page's checked rows across the reload an
// action ends in. One shot, and pinned to the listing it was made on, so one
// left behind by an action that never reloaded cannot re-select elsewhere.
// The gallery has no use for it: its own selection is persisted on every
// change and survives the reload on its own.
function stashSelection(ids) {
  try {
    sessionStorage.setItem('monbooru_tag_selection', JSON.stringify(
      {ids: ids, url: tagSelectionKey(), t: Date.now()}));
  } catch (e) {}
}

function tagSelectionKey() {
  return activeGallery() + location.pathname + location.search;
}

function takeTagStash() {
  var raw = null;
  try { raw = sessionStorage.getItem('monbooru_tag_selection'); } catch (e) {}
  if (!raw) return [];
  try { sessionStorage.removeItem('monbooru_tag_selection'); } catch (e) {}
  var stash = null;
  try { stash = JSON.parse(raw); } catch (e) {}
  if (!stash || !stash.ids || Date.now() - stash.t > selectionStashMs ||
      stash.url !== tagSelectionKey()) {
    return [];
  }
  return stash.ids;
}

document.addEventListener('DOMContentLoaded', function() {
  readSelection();
  syncSelectionBoxes();
  // Every box is set, not just the ones the stash names: a browser that keeps
  // form state across a reload restores it by position, which after an action
  // that dropped rows points at whichever rows moved up into their places.
  var tagIDs = takeTagStash();
  document.querySelectorAll('.tag-select').forEach(function(cb) {
    cb.checked = tagIDs.indexOf(cb.value) !== -1;
  });
  // The bars are derived from the list and only ever recomputed on a change
  // event, so a load that arrives with one needs the derivation run once here
  // or the selection sits there with no bar.
  updateBatchBar();
  if (typeof updateTagBatchBar === 'function') updateTagBatchBar();
});

// The escalation hands the scope to the server: no ids travel, the query
// does, so what runs is the search as it stands rather than a snapshot.
document.addEventListener('click', function(e) {
  if (!e.target.closest || !e.target.closest('#batch-select-all-matching')) return;
  e.preventDefault();
  selectAllMatching = true;
  writeSelection();
  updateBatchBar();
});

function sidebarTagRows() {
  return Array.from(document.querySelectorAll('#tag-groups .tag-entry[data-tag-id]'));
}

// tagFocusRows are the rows the keyboard cursor visits: implied tags go
// with the parent that justifies them, so they can't be removed alone.
function tagFocusRows() {
  return sidebarTagRows().filter(function(li) { return !li.classList.contains('tag-entry-implied'); });
}

function focusedTagRow() {
  return document.querySelector('#tag-groups .tag-entry.focused');
}

// Detail-page tag-focus mode (entered with 'r'). The currently focused tag
// is marked by .tag-entry.focused; arrow keys cycle, Enter triggers the
// matching .tag-entry-remove, Escape exits without removing.
function enterTagFocusMode() {
  var items = tagFocusRows();
  if (!items.length) return;
  if (focusedTagRow()) return;
  revealSidebarFor(items[0]);
  items[0].classList.add('focused');
  items[0].scrollIntoView({block: 'nearest'});
  // Mode flag survives an htmx swap that strips the .focused class on the
  // <li> - the Escape handler keys off body.tag-focus to know we're still
  // in mode.
  document.body.classList.add('tag-focus');
}

function exitTagFocusMode() {
  document.querySelectorAll('#tag-groups .tag-entry.focused').forEach(function(li) {
    li.classList.remove('focused');
  });
  document.body.classList.remove('tag-focus');
}

function cycleTagFocus(step) {
  var items = tagFocusRows();
  if (!items.length) return;
  var current = focusedTagRow();
  var idx = current ? items.indexOf(current) : 0;
  idx = Math.max(0, Math.min(items.length - 1, idx + step));
  setFocused(items, idx);
}

// Batch delete: opens the unified delete dialog scoped to the checked
// thumbs. The actual fetch + background-job wiring lives in
// confirmBatchDelete (gallery.html).
function batchDeleteSelected() {
  if (typeof openBatchDeleteDialog === 'function') openBatchDeleteDialog('selection');
}

// openSaveSearchDialog prefills the save-search dialog from the current
// search input, then opens it. Shared by the S-key shortcut and the header
// button. Returns false when the dialog isn't on the page so the key
// handler can fall through.
function openSaveSearchDialog() {
  var dlg = document.getElementById('save-search-dialog');
  if (!dlg) return false;
  var si = document.getElementById('search-input');
  var sq = document.getElementById('save-search-query');
  var sp = document.getElementById('save-search-preview');
  if (si && sq) sq.value = si.value;
  if (si && sp) sp.textContent = si.value || '(empty)';
  // Snapshot the current URL's sort/order/seed so the saved entry
  // reopens at the same view. Defaults stay empty so the gallery
  // handler's defaults take over on reopen if nothing was set here.
  var url = new URL(window.location.href);
  var ss = document.getElementById('save-search-sort');
  var so = document.getElementById('save-search-order');
  var se = document.getElementById('save-search-seed');
  var savedSort = url.searchParams.get('sort') || '';
  if (ss) ss.value = savedSort;
  if (so) so.value = url.searchParams.get('order') || '';
  // Empty under any other sort: nothing reads a seed there, and the row
  // would carry a value the schema says it does not.
  if (se) se.value = savedSort === 'random' ? (url.searchParams.get('seed') || '') : '';
  dlg.showModal();
  return true;
}

// refreshJobStatus forces the top-right job-status widget to re-fetch its
// state so a newly started background job becomes visible without waiting for
// the next 2s poll tick.
function refreshJobStatus() {
  var el = document.getElementById('job-status');
  if (!el || !window.htmx) return;
  el.setAttribute('hx-trigger', 'every 2s');
  window.htmx.process(el);
  window.htmx.ajax('GET', '/internal/job/status', {target: '#job-status', swap: 'outerHTML'});
}

// Shared confirmation dialog: replaces native confirm() and intercepts
// hx-confirm via the htmx:confirm event listener below. The triggering
// element may set data-confirm-danger for a red-tinted second warning line.
// alt is an optional {label, run} third choice for prompts where the
// decision is not yes/no: the upgrade prompt offers keeping the local file
// beside taking the source's, since both answer the comparison it shows.
function showConfirm(message, onOk, danger, okLabel, alt) {
  var dlg = document.getElementById('confirm-dialog');
  if (!dlg) { if (window.confirm(message)) onOk(); return; }
  document.getElementById('confirm-dialog-msg').textContent = message || '';
  document.getElementById('confirm-dialog-danger').textContent = danger || '';
  var okBtn = document.getElementById('confirm-dialog-ok');
  var cancelBtn = document.getElementById('confirm-dialog-cancel');
  var altBtn = document.getElementById('confirm-dialog-alt');
  okBtn.textContent = okLabel || 'OK';
  altBtn.hidden = !alt;
  altBtn.textContent = alt ? alt.label : '';
  var close = function() { dlg.close(); okBtn.onclick = null; cancelBtn.onclick = null; altBtn.onclick = null; };
  okBtn.onclick = function() { close(); onOk(); };
  altBtn.onclick = alt ? function() { close(); alt.run(); } : null;
  cancelBtn.onclick = close;
  dlg.showModal();
  // Land focus on the safer button so a keyboard-only operator can
  // commit safe prompts with a single Enter while destructive prompts
  // (those carrying a danger line) require an explicit Tab to OK.
  if (danger) cancelBtn.focus(); else okBtn.focus();
}

document.body.addEventListener('htmx:confirm', function(e) {
  if (!e.detail || !e.detail.question) return;
  e.preventDefault();
  var elt = e.detail.elt;
  var ds = elt && elt.dataset ? elt.dataset : {};
  var alt = null;
  if (ds.confirmAlt && ds.confirmAltUrl) {
    // The alternative posts the triggering form's own hidden fields (which
    // carry the CSRF token and the origin's identity) to a different route.
    alt = {label: ds.confirmAlt, run: function() {
      var values = {};
      var form = elt.closest('form');
      if (form) {
        form.querySelectorAll('input[type="hidden"]').forEach(function(i) { values[i.name] = i.value; });
      }
      if (ds.confirmAltValue) {
        values[ds.confirmAltValue.split('=')[0]] = ds.confirmAltValue.split('=')[1];
      }
      // No source element: htmx would inherit the hx-confirm that opened
      // this dialog and ask the same question again instead of posting.
      htmx.ajax('POST', ds.confirmAltUrl, {values: values});
    }};
  }
  showConfirm(e.detail.question, function() { e.detail.issueRequest(true); }, ds.confirmDanger, ds.confirmOk, alt);
});

// htmx raises htmx:confirm only for requests it issues itself, so an
// hx-confirm on a plain form (Stop, which has to navigate) would be inert.
// form.submit() fires no submit event, so a confirmed form posts without
// looping back here.
var htmxVerbs = ['hx-get', 'hx-post', 'hx-put', 'hx-patch', 'hx-delete'];
document.body.addEventListener('submit', function(e) {
  var form = e.target;
  if (!form.hasAttribute || !form.hasAttribute('hx-confirm')) return;
  if (htmxVerbs.some(function(v) { return form.hasAttribute(v); })) return;
  e.preventDefault();
  var ds = form.dataset;
  showConfirm(form.getAttribute('hx-confirm'), function() { form.submit(); }, ds.confirmDanger, ds.confirmOk);
});

// Page jump: dialog that sets ?page= on the current URL. Works for both
// the HTMX gallery and the full-page tags pagination.
document.addEventListener('click', function(e) {
  var btn = e.target.closest('.page-jump');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('page-jump-dialog');
  var input = document.getElementById('page-jump-input');
  var totalSpan = document.getElementById('page-jump-total');
  if (!dlg || !input) return;
  var current = btn.dataset.current || '1';
  var total = btn.dataset.total || '1';
  input.value = current;
  // data-max, not the HTML max attribute: max would trigger HTML5
  // constraint validation and block submit, so the clamp never runs.
  input.dataset.max = total;
  if (totalSpan) totalSpan.textContent = total;
  dlg.showModal();
  setTimeout(function() { input.focus(); input.select(); }, 0);
});

// A derivative graph hangs one action per incoming edge off the node the
// edge lands on - a [review again] on the browse card, an [unlink source]
// on the image's own page - and the id in the label is otherwise all that
// tells apart the several a fan-in carries. Pointing at either end lights
// the other.
['mouseover', 'mouseout', 'focusin', 'focusout'].forEach(function(type) {
  document.addEventListener(type, function(e) {
    var el = e.target.closest ? e.target.closest('[data-edge]') : null;
    var graph = el && el.closest('.deriv-graph');
    if (!graph) return;
    var on = type === 'mouseover' || type === 'focusin';
    graph.querySelectorAll('[data-edge="' + el.dataset.edge + '"]').forEach(function(peer) {
      peer.classList.toggle('deriv-edge-lit', on);
    });
  });
});

// "Add a relation..." chip in the detail-page Relations panel opens
// the manual relation-add dialog with the current image pre-filled.
// Each open resets the form so a chain of adds doesn't keep the prior
// child id or relation-type radio in place after the dialog closes.
document.addEventListener('click', function(e) {
  var btn = e.target.closest('[data-relations-add]');
  if (!btn) return;
  e.preventDefault();
  var dlg = document.getElementById('relation-edit-dialog');
  if (!dlg) return;
  var parentInput = document.getElementById('relation-parent-id');
  var otherInput = document.getElementById('relation-other-id');
  var self = btn.getAttribute('data-relations-add');
  if (parentInput) { parentInput.value = self; parentInput.readOnly = true; }
  if (otherInput) { otherInput.value = ''; otherInput.readOnly = false; }
  var otherThumb = document.getElementById('relation-edit-thumb-other');
  if (otherThumb) { otherThumb.hidden = true; otherThumb.removeAttribute('src'); otherThumb.alt = ''; }
  var dupRadio = dlg.querySelector('input[name="type"][value="duplicate"]');
  if (dupRadio) dupRadio.checked = true;
  var err = document.getElementById('relation-edit-error');
  if (err) err.innerHTML = '';
  var overwrite = document.getElementById('relation-edit-overwrite-btn');
  if (overwrite) {
    overwrite.hidden = true;
    overwrite.textContent = 'Overwrite existing relation';
  }
  dlg.showModal();
});

// Handles the response of the detail-page external-field dialogs
// (Source / URL / Collection / Order). On 204 the server sends back
// HX-Refresh so the page reloads with the new value; the post-reload
// flash is delivered via the shared monbooru:flash HX-Trigger / picker.
// Validation errors land 200 OK with a flash-err body the dialog already
// targets, so the dialog stays open and the operator can correct the
// input.
//
// HTMX events bubble: the input inside the form fires hx-get against
// the collection / source suggest endpoints, and those return 204 when
// no matches exist. Without the FORM gate below, every empty suggest
// response would slip past the status check and close the dialog
// mid-typing.
function onExternalEditResponse(event, dialogID) {
  if (!event || !event.detail || !event.detail.xhr) return;
  if (!event.detail.elt || event.detail.elt.tagName !== 'FORM') return;
  if (event.detail.xhr.status !== 204) return;
  var dlg = document.getElementById(dialogID);
  if (dlg) dlg.close();
}

function toggleAnnotations(btn) {
  var media = btn.closest('.detail-media');
  if (!media) return;
  var hidden = media.classList.toggle('annotations-hidden');
  btn.textContent = hidden ? '[show annotations]' : '[hide annotations]';
}

// actionFlashSlots is the ordered list of per-page slot ids the shared
// flash helpers fall through. Each page that wants action feedback ships
// one of these slots in its template; the page only ever has one of them.
var actionFlashSlots = ['gallery-flash', 'detail-flash', 'tag-flash', 'cat-flash', 'collection-flash', 'flash-tagger'];

function findActionFlashSlot() {
  for (var i = 0; i < actionFlashSlots.length; i++) {
    var el = document.getElementById(actionFlashSlots[i]);
    if (el) return el;
  }
  return null;
}

// Every call restamps the token, so an earlier call's timer finds a stale
// one and leaves the newer message standing.
function showActionFlash(html, kind) {
  var slot = findActionFlashSlot();
  if (!slot || !html) return;
  var cls = kind === 'err' ? 'flash-err' : 'flash-ok';
  if (/class\s*=\s*"[^"]*flash\b/.test(html)) {
    slot.innerHTML = html;
  } else {
    slot.innerHTML = '<div class="flash ' + cls + '">' + html + '</div>';
  }
  var token = String(Date.now()) + Math.random();
  slot.dataset.token = token;
  setTimeout(function() {
    if (slot.dataset.token === token) slot.innerHTML = '';
  }, 5000);
}

function escapeHTML(s) {
  var d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

// setFlashText replaces slot's content with a flash carrying free text;
// textContent keeps interpolated names inert.
function setFlashText(slot, kind, text) {
  if (!slot) return;
  var d = document.createElement('div');
  d.className = 'flash flash-' + kind;
  d.textContent = text;
  slot.textContent = '';
  slot.appendChild(d);
}

// stashActionFlash queues a flash for the next page load - used when an
// action triggers a full reload / redirect and the in-place showActionFlash
// would be wiped before the user could read it. The timestamp lets the
// picker reject stashes older than the staleness window so a non-navigating
// action's stash doesn't poison a later, unrelated navigation.
function stashActionFlash(html, kind) {
  if (!html) return;
  try {
    sessionStorage.setItem('monbooru_action_flash',
      JSON.stringify({h: html, k: kind || 'ok', t: Date.now()}));
  } catch (e) {}
}

// stashStalenessMs caps how long a stashed flash survives before the picker
// drops it. Long enough to cover a normal action+navigation round-trip,
// short enough that a stash left behind by a non-navigating action doesn't
// pop on the next page the user visits five minutes later.
var stashStalenessMs = 10000;

document.addEventListener('DOMContentLoaded', function() {
  if (!findActionFlashSlot()) return;
  var raw;
  try { raw = sessionStorage.getItem('monbooru_action_flash'); } catch (e) { return; }
  if (!raw) return;
  try { sessionStorage.removeItem('monbooru_action_flash'); } catch (e) {}
  var stash;
  try { stash = JSON.parse(raw); } catch (e) { return; }
  if (!stash || !stash.h) return;
  if (stash.t && Date.now() - stash.t > stashStalenessMs) return;
  showActionFlash(stash.h, stash.k);
});

// HX-Trigger bridge: every handler that wants a success flash emits
// `{"monbooru:flash": {"text": "...", "kind": "ok"}}` on the response.
// Dual-mode: show on the current page (covers non-navigating actions like
// relation-add) and stash for the next page (covers HX-Redirect / HX-Refresh
// flows like delete / move / categories / tags / external-edit). The
// stash carries a timestamp so a non-consumed stash doesn't outlive the
// staleness window.
document.body.addEventListener('monbooru:flash', function(e) {
  if (!e || !e.detail) return;
  var text = e.detail.text || '';
  var kind = e.detail.kind || 'ok';
  showActionFlash(text, kind);
  stashActionFlash(text, kind);
});

// Handles the dialog response for /relations/add. The success-side flash
// rides the shared monbooru:flash HX-Trigger so it lands in the page
// slot via the common listener; this handler only owns the conflict
// affordance (Overwrite button label / visibility) and the dialog-close
// + related-entries refresh on success.
function onRelationEditResponse(event) {
  var src = document.getElementById('relation-edit-error');
  if (!src) return;
  var overwrite = document.getElementById('relation-edit-overwrite-btn');
  // Both answers the server writes - added, and refused - come back 200
  // with a body for the slot, so the body is what tells them apart. A
  // request that swaps nothing (dropped connection, 5xx) leaves the slot
  // as it was, and reading it there reports a conflict nobody hit while
  // the dialog sits open saying nothing at all.
  var answered = event && event.detail && event.detail.successful &&
    src.querySelector('.flash-ok, .flash-err');
  if (!answered) {
    if (overwrite) overwrite.hidden = true;
    src.innerHTML = '<div class="flash flash-err">The relation was not saved.</div>';
    return;
  }
  var success = src.querySelector('.flash-ok');
  if (!success) {
    // Conflict: surface the Overwrite affordance so the operator can
    // replace the existing relation in one click. The button label
    // tracks which conflict the server reported so the operator reads
    // exactly what they're about to drop:
    //   - "already has a different relation" -> Overwrite existing relation
    //     (pair-shaped conflict between THIS pair)
    //   - "already has a version edge" -> Replace existing version edge
    //     (one side is already on a version chain with a third image)
    var err = src.querySelector('.flash-err');
    if (overwrite) {
      var msg = err ? (err.textContent || '') : '';
      if (/already has a different relation/i.test(msg)) {
        overwrite.textContent = 'Overwrite existing relation';
        overwrite.hidden = false;
      } else if (/already has a version edge/i.test(msg)) {
        overwrite.textContent = 'Replace existing version edge';
        overwrite.hidden = false;
      } else {
        overwrite.hidden = true;
      }
    }
    return;
  }
  if (overwrite) {
    overwrite.hidden = true;
    overwrite.textContent = 'Overwrite existing relation';
  }
  src.innerHTML = '';
  var dlg = document.getElementById('relation-edit-dialog');
  if (dlg) dlg.close();
  if (window.htmx) {
    var panel = document.getElementById('related-entries-panel');
    if (panel) window.htmx.trigger(panel, 'relations-changed');
  }
}

function submitPageJump() {
  var input = document.getElementById('page-jump-input');
  if (!input) return;
  var p = parseInt(input.value, 10);
  if (!p || p < 1) p = 1;
  var max = parseInt(input.dataset.max, 10);
  if (max && p > max) p = max;
  var u = new URL(window.location.href);
  u.searchParams.set('page', String(p));
  window.location.href = u.toString();
}

// Sidebar toggle: a drawer over the grid at narrow viewports, a persisted
// collapse of the layout column above them. The topbar button carries the
// narrow job and the layout's left rail the wide one, so only one of the two
// is ever on screen; the b key clicks the button either way.
document.addEventListener('click', function(e) {
  if (!e.target.matches || !e.target.matches('#sidebar-toggle, #sidebar-rail')) return;
  if (window.matchMedia('(max-width: 768px)').matches) {
    const sidebar = document.getElementById('sidebar');
    if (sidebar) sidebar.classList.toggle('open');
    return;
  }
  const layout = document.getElementById('main-layout');
  if (layout) setSidebarCollapsed(!layout.classList.contains('sidebar-collapsed'));
});

// The size is a cookie the server reads, so it lands on the next render
// rather than this one. Page 1 because the number in the URL indexes a
// listing sliced at the old size.
function setPageSize(size) {
  postForm('/internal/view-prefs', {page_size: size}, {
    onOK: function() {
      var u = new URL(location.href);
      u.searchParams.set('page', '1');
      location.href = u.toString();
    }
  });
}

// Cell size is pure CSS, so it applies now; the cookie only decides how the
// next render arrives.
function setThumbSize(size) {
  var grid = document.querySelector('.thumb-grid');
  if (grid) {
    grid.classList.toggle('thumb-sm', size === 's');
    grid.classList.toggle('thumb-lg', size === 'l');
  }
  document.querySelectorAll('#thumb-size-ramp button').forEach(function(b) {
    b.setAttribute('aria-pressed', String(b.dataset.thumb === size));
  });
  postForm('/internal/view-prefs', {thumb: size});
}

// The cookie is what the server reads to render the next page already
// collapsed; the class is what the current page needs right now.
function setSidebarCollapsed(collapsed) {
  var layout = document.getElementById('main-layout');
  if (!layout) return;
  layout.classList.toggle('sidebar-collapsed', collapsed);
  document.cookie = 'monbooru_sidebar=' + (collapsed ? 'collapsed' : '') +
    '; path=/; max-age=' + (collapsed ? 31536000 : 0);
  // A collapsed layout renders its lazy panels without their load trigger, so
  // showing the column is what fetches them, against the query on screen now
  // rather than the one the page opened on.
  if (!collapsed) htmx.trigger(document.body, 'sidebar-shown');
}

// A collapsed sidebar still holds its fields and its tag rows; landing the
// keyboard on one the operator can't see reads as the key doing nothing.
function revealSidebarFor(el) {
  if (el && el.closest && el.closest('#sidebar')) setSidebarCollapsed(false);
}

function getFolderCookie() {
  var m = document.cookie.match(/monbooru_folders=([^;]*)/);
  if (!m) return new Set();
  try { return new Set(decodeURIComponent(m[1]).split(',').filter(Boolean)); }
  catch (err) { return new Set(); }
}

function setFolderCookie(set) {
  document.cookie = 'monbooru_folders=' + encodeURIComponent(Array.from(set).join(',')) + '; path=/; max-age=31536000';
}

function toggleFolderItem(btn, targetId, path) {
  var list = document.getElementById(targetId);
  if (!list) return;
  var state = getFolderCookie();
  var isCollapsed = list.style.display === 'none';
  list.style.display = isCollapsed ? '' : 'none';
  btn.textContent = isCollapsed ? '▼' : '▶';
  if (isCollapsed) state.add(path || targetId);
  else state.delete(path || targetId);
  setFolderCookie(state);
}

// The cookie records sections toggled off their default: an opened
// default-collapsed section, or a collapsed default-open one (Tags).
function initSectionToggle(toggleId, listId, cookieKey, forceOpen, defaultOpen) {
  var toggle = document.getElementById(toggleId);
  var list = document.getElementById(listId);
  if (!toggle || !list) return;
  var offDefault = getFolderCookie().has(cookieKey);
  if (defaultOpen) {
    if (offDefault) { list.style.display = 'none'; toggle.textContent = '▶'; }
  } else if (forceOpen || offDefault) {
    list.style.display = '';
    toggle.textContent = '▼';
  }
  var clickToggle = function() {
    var state = getFolderCookie();
    var isCollapsed = list.style.display === 'none';
    list.style.display = isCollapsed ? '' : 'none';
    toggle.textContent = isCollapsed ? '▼' : '▶';
    var nowOffDefault = defaultOpen ? !isCollapsed : isCollapsed;
    if (nowOffDefault) state.add(cookieKey);
    else state.delete(cookieKey);
    setFolderCookie(state);
  };
  toggle.onclick = clickToggle;
  var header = toggle.closest('.sidebar-section-header');
  var title = header && header.querySelector('.sidebar-section-title');
  if (title) title.onclick = clickToggle;
}

// _lastInitedQuery latches the URL the sidebar's auto-expand logic
// last responded to. HTMX OOB swaps replace #sidebar-inner verbatim on
// every gallery refresh - including the watcher-driven refreshes the
// job-status poll fires without a URL change - so any `firstInit` marker
// stored on a DOM element gets wiped between renders. Comparing the
// captured query against window.location.search is the only signal
// that distinguishes a real navigation from a same-view re-render, and
// without it the URL-driven force-open undoes the user's explicit
// section collapse a few seconds after they click it.
var _lastInitedQuery = null;

function initFolderTree() {
  // The sidebar is lazy-loaded on every page that owns one; DOMContentLoaded
  // fires before the placeholder's hx-get settles. Bail when no toggle
  // buttons are present so the empty pass doesn't latch the URL and
  // suppress the next real settle's navigation-driven force-open.
  if (!document.querySelector('.folder-toggle-btn')) return;

  var expanded = getFolderCookie();
  var currentQuery = window.location.search;
  var urlChanged = currentQuery !== _lastInitedQuery;
  _lastInitedQuery = currentQuery;

  // Determine current folder from URL query. Both folder:PATH (recursive)
  // and folderonly:PATH (exact) drive the same sidebar auto-expand path.
  var currentFolder = '';
  var urlParams = new URLSearchParams(currentQuery);
  var q = urlParams.get('q') || '';
  var folderMatch = q.match(/(?:^|\s)folder(?:only)?:(?:"([^"]+)"|([^\s]*))/);
  if (folderMatch) {
    currentFolder = folderMatch[1] || folderMatch[2];
  }

  // Tags section: open by default, collapse persists (inverse of the others).
  initSectionToggle('tags-toggle', 'tag-groups', '__tags__', false, true);
  // Folder tree main toggle (show/hide whole tree).
  // Use onclick assignment (not addEventListener) to prevent duplicate handlers
  // from multiple calls (e.g. HTMX partial swaps fire htmx:afterSettle repeatedly).
  initSectionToggle('folder-tree-toggle', 'folder-tree-list', '__tree__', false);
  // Sources panel: auto-open when the current query targets a specific
  // source label, so the matching row is visible on landing.
  var sourceLabelMatch = q.match(/(?:^|\s)source:(?:"([^"]+)"|([^\s]+))/);
  var sourceLabelOpen = urlChanged && !!sourceLabelMatch;
  initSectionToggle('source-labels-toggle', 'source-labels-list', '__sources__', sourceLabelOpen);
  var treeToggle = document.getElementById('folder-tree-toggle');
  var treeList = document.getElementById('folder-tree-list');

  // Subfolder toggles - use onclick to avoid duplicate listeners
  document.querySelectorAll('.folder-toggle-btn[data-path]').forEach(function(btn) {
    var path = btn.dataset.path;
    var targetId = btn.dataset.target || ('fc-' + path);
    var list = document.getElementById(targetId);
    if (!list) return;

    // urlDriven: this expansion comes from navigation context (the current
    // folder). Gated by urlChanged so a same-view sidebar rebuild (watcher
    // refresh, job progress) leaves the user's collapse alone - only a real
    // URL change re-asserts the navigation-driven open.
    var urlDriven = currentFolder && (currentFolder === path || currentFolder.startsWith(path + '/'));
    var shouldExpand = expanded.has(path) || (urlChanged && urlDriven);

    if (shouldExpand) {
      list.style.display = '';
      btn.textContent = '▼';
      // Force the parent tree open only when navigation drives this
      // expansion. The parent's open/close otherwise stays under the
      // user's control via initSectionToggle's own cookie key.
      if (urlChanged && urlDriven && treeList) {
        treeList.style.display = '';
        if (treeToggle) treeToggle.textContent = '▼';
      }
    }

    btn.onclick = function(e) {
      e.stopPropagation();
      toggleFolderItem(btn, targetId, path);
    };
  });
}

document.addEventListener('DOMContentLoaded', initFolderTree);
// Re-run on HTMX settle to restore auto-expand state after URL changes,
// but since we use btn.onclick (not addEventListener) no duplicate handlers accumulate.
document.addEventListener('htmx:afterSettle', initFolderTree);

// Tag suggest (detail page + merge dialog): apply selected suggestion
// lastWordIndex returns the index of the last non-whitespace token in a
// split(/(\s+)/) word list, or -1 when every token is whitespace.
function lastWordIndex(words) {
  for (var i = words.length - 1; i >= 0; i--) {
    if (words[i].trim() !== '') return i;
  }
  return -1;
}

function applyTagSuggest(btn) {
  var tagName = btn.dataset.tagName;
  if (!tagName) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  dd.innerHTML = '';
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  // Multi-tag inputs (e.g. upload form) keep previous tokens: replace the last
  // whitespace-separated word and append a trailing space for the next tag.
  if (input.dataset.multiTags) {
    var words = input.value.split(/(\s+)/);
    var lastIdx = lastWordIndex(words);
    if (lastIdx >= 0) words[lastIdx] = tagName;
    else words.push(tagName);
    input.value = words.join('') + ' ';
    input.focus();
    return;
  }
  input.value = tagName;
  input.focus();
  // Submit the tag-add form if present; merge dialog just fills the input
  var form = input.closest('form') || container.querySelector('form');
  if (form && form.id === 'add-tag-form') {
    form.requestSubmit();
  }
}

// Label suggest (place-image/batch-place folder dialogs; detail and batch
// collection/source dialogs): copies the picked value into the dropdown's
// nearest text input and keeps focus so the user can keep typing. key names
// the dataset field carrying the value ('folderPath' or 'series').
function applyLabelSuggest(btn, key) {
  var label = btn.dataset[key];
  if (label == null) return;
  var dd = btn.closest('.suggest-dropdown');
  if (!dd) return;
  var container = dd.parentElement;
  if (!container) return;
  var input = container.querySelector('input[type="text"]');
  if (!input) return;
  dd.innerHTML = '';
  input.value = label;
  input.focus();
}

// insertToken drops a chip's token into the field it names, at the caret,
// and fires the input event the preview slot listens for. A rail serving
// two fields inserts into whichever holds the caret; the chip suppresses
// its own focus grab so the click never takes it away first.
function insertToken(btn) {
  var ids = (btn.dataset.targets || '').split(/\s+/).filter(Boolean);
  var input = ids.indexOf((document.activeElement || {}).id) >= 0
    ? document.activeElement
    : document.getElementById(ids[0]);
  if (!input) return;
  var token = btn.dataset.token;
  var at = input.selectionStart == null ? input.value.length : input.selectionStart;
  var to = input.selectionEnd == null ? at : input.selectionEnd;
  input.value = input.value.slice(0, at) + token + input.value.slice(to);
  input.focus();
  input.setSelectionRange(at + token.length, at + token.length);
  input.dispatchEvent(new Event('input', {bubbles: true}));
}

// namePreviewVals is the hx-vals shape a batch dialog's preview slot posts:
// both halves of the destination, the surface they belong to, and the head
// of the scope they would apply to. A search scope has no checked ids, so
// the first rendered thumbnails stand in - they are the head of the same
// order the job walks.
function namePreviewVals(prefix, scope) {
  var scopeEl = document.getElementById(prefix + '-scope');
  var kind = scopeEl && scopeEl.value ? scopeEl.value : 'selection';
  var ids = kind === 'search'
    ? Array.prototype.map.call(document.querySelectorAll('.thumb-card'),
        function(c) { return c.dataset.id; })
    : selectedImageIds();
  var slot = document.getElementById(prefix + '-preview');
  var rows = parseInt(slot && slot.dataset.rows, 10) || 5;
  return {
    folder: document.getElementById(prefix + '-folder').value,
    name: document.getElementById(prefix + '-name').value,
    scope: scope,
    rows: rows,
    ids: ids.slice(0, rows).join(','),
    total: scopeCount(kind, null, null)
  };
}

// showMoreNamePreview widens the window the preview slot asks for and
// re-runs it. A click rather than a keystroke, because a {md5} template
// hashes files that carry no digest yet.
function showMoreNamePreview(btn) {
  var slot = btn.closest('[hx-get]');
  if (!slot) return;
  slot.dataset.rows = btn.dataset.rows;
  if (window.htmx) window.htmx.trigger(slot, 'preview');
}

function applySearchSuggest(tagName) {
  var si = document.getElementById('search-input');
  if (!si) return;
  var words = si.value.split(/(\s+)/);
  var lastWordIdx = lastWordIndex(words);
  if (lastWordIdx >= 0) {
    var last = words[lastWordIdx];
    var prefix = last.startsWith('-') ? '-' : '';
    words[lastWordIdx] = prefix + tagName;
  } else {
    words.push(tagName);
  }
  // system: cheat-sheet rows end on a colon (`date:`) or a comparison
  // operator (`date:>`, `width:>=`, `date:..`); the user's next keystroke
  // is the value, so don't push the cursor onto a new term with a space.
  var keepCursor = /[:<>=]$|\.\.$/.test(tagName);
  si.value = words.join('') + (keepCursor ? '' : ' ');
  var dd = document.getElementById('search-suggest');
  if (dd) dd.innerHTML = '';
  si.focus();
  // After picking a colon- or operator-terminated row the cursor stays
  // parked next to the prefix (`fav:`, `date:>`, `character:`); fire an
  // input event so htmx's `input changed` trigger refreshes the
  // dropdown with the level-2 hint - operators or value lists for
  // filter keywords, live tags for category prefixes - without forcing
  // the user to type a throwaway character to wake autocomplete back up.
  if (keepCursor) {
    si.dispatchEvent(new Event('input', { bubbles: true }));
  }
}

// Copy `text` to the clipboard with a clipboard-API path and a
// document.execCommand fallback so the helper still works under the
// LAN-HTTP deployments where the secure-context API is unavailable.
// `flashEl`, when provided, is briefly unhidden as a "copied" badge so
// the click has visible feedback instead of failing silently.
function copyToClipboard(text, flashEl) {
  var done = function () {
    if (!flashEl) return;
    flashEl.hidden = false;
    setTimeout(function () { flashEl.hidden = true; }, 1500);
  };
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(done, function () {
      copyToClipboardLegacy(text, done);
    });
    return;
  }
  copyToClipboardLegacy(text, done);
}

function copyToClipboardLegacy(text, done) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (e) { /* fall through */ }
  document.body.removeChild(ta);
  done();
}

document.addEventListener('click', function (e) {
  var btn = e.target.closest('[data-copy], [data-copy-from]');
  if (!btn) return;
  e.preventDefault();
  var text = btn.dataset.copy;
  if (!text && btn.dataset.copyFrom) {
    var src = document.querySelector(btn.dataset.copyFrom);
    text = src ? src.value : '';
  }
  var flash = btn.parentElement && btn.parentElement.querySelector('.copy-flash');
  copyToClipboard(text, flash);
});

document.addEventListener('click', function (e) {
  var btn = e.target.closest('.btn-tagger-cmd');
  if (!btn) return;
  var dlg = document.getElementById('tagger-cmd-dialog');
  if (!dlg) return;
  var host = btn.dataset.hostCmd || '';
  var docker = btn.dataset.dockerCmd || '';
  document.getElementById('tagger-cmd-name').textContent = btn.dataset.taggerName || '';
  document.getElementById('tagger-cmd-desc').textContent = btn.dataset.taggerDesc || '';
  document.getElementById('tagger-cmd-gated').hidden = !btn.dataset.gated;
  document.getElementById('tagger-cmd-host').textContent = host;
  document.getElementById('tagger-cmd-docker').textContent = docker;
  document.getElementById('tagger-cmd-host-copy').dataset.copy = host;
  document.getElementById('tagger-cmd-docker-copy').dataset.copy = docker;
  dlg.showModal();
});

// The directory picker: a browser cannot hand the server a filesystem path,
// so the choice is made against a server-rendered listing. The Browse button
// only opens the dialog - htmx has already been told to fetch the listing
// into it - and "Use this folder" writes the result back into the input the
// button named.
document.addEventListener('click', function (e) {
  if (e.target.closest('.dir-picker-open')) {
    var dlg = document.getElementById('dir-picker-dialog');
    if (dlg && !dlg.open) dlg.showModal();
    return;
  }
  var use = e.target.closest('.dir-picker-use');
  if (!use) return;
  var input = document.getElementById(use.dataset.into || '');
  if (input) {
    input.value = use.dataset.path || '';
    input.dispatchEvent(new Event('input', { bubbles: true }));
  }
  var open = use.closest('dialog');
  if (open) open.close();
});

var _jobAutoClearTimer = null;
// FinishedAt the current auto-clear timer was armed against; re-armed on newer
// surface events so rolling watcher activity doesn't trip the dismiss mid-batch
// (which would strip hx-trigger and silence the widget until page reload).
var _jobAutoClearFinishedAt = '';
// Track the FinishedAt timestamp of the last reloaded event so each new
// watcher/job completion triggers exactly one reload.
var _lastReloadedFinishedAt = '';
// refreshGalleryGrid re-fetches the current URL into the gallery grid.
// No-op off the gallery page or before htmx loads.
function refreshGalleryGrid() {
  if (!document.getElementById('gallery-grid') || !window.htmx) return;
  var u = new URL(window.location.href);
  window.htmx.ajax('GET', u.pathname + u.search, {target: '#gallery-grid', swap: 'innerHTML'});
}

// The first job-status settle after page load reports whatever job last
// finished on the server, even when that job completed before the page was
// rendered. Reloading the grid/tags on that first poll is wasted work (and
// races user keystrokes) because the server-rendered DOM already reflects
// the post-job state. Skip exactly one reload, then resume normal behaviour
// so subsequent watcher/job events still surface live.
var _firstJobStatusSettle = true;
// Processed cursor for the currently-running job. The gallery grid is
// refreshed whenever the worker bumps this so changes show up without a
// manual reload, mirroring the watcher's per-event surface path. Applies
// to progress-emitting job types listed in the handler below.
var _lastJobProcessed = -1;
// Watcher-event counter. Bumped by the server whenever the filesystem
// watcher surfaces an ingest/remove while a job is running; gives the grid
// a refresh signal for job types that don't themselves change the image
// list (autotag) or whose progress cursor is scoped to existing rows.
var _lastWatcherNotices = -1;
// One-shot latch set by user-initiated deletes. When the delete job finishes,
// the htmx.ajax reload path intermittently fails to settle the gallery-grid
// swap, leaving the view stale. Force a full reload instead - only for the
// delete case; other job completions still use the incremental swap.
var _pendingGalleryReload = false;

// armGalleryReload is the onOK every page-level job dialog passes: the job
// has started, so the next completion reloads rather than swapping, and the
// status widget picks the job up now instead of on its next poll. The
// typeof guard stays because refreshJobStatus is defined in a template that
// is not on every page.
function armGalleryReload() {
  _pendingGalleryReload = true;
  if (typeof refreshJobStatus === 'function') refreshJobStatus();
}

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;

  // When the gallery grid is swapped (pagination, search, or job reload), put
  // the selection back on the fresh checkboxes. htmx has already pushed the
  // new URL, so the listing key decides: a page flip keeps the pick, a search
  // drops it. The swap wipes any .focused class; reapply it from the URL hash
  // so the arrow-key cursor doesn't vanish when a post-job refresh races the
  // user.
  if (el && el.id === 'gallery-grid') {
    readSelection();
    syncSelectionBoxes();
    updateBatchBar();
    restoreGalleryFocusFromHash();
    initInboxUpload();
    return;
  }

  // After a tag-remove swap in tag-focus mode, restore .focused on the
  // row one position before the one just deleted. Without this the
  // mode flag survives but the .focused class is wiped, so cycleTagFocus's
  // `current ? indexOf : 0` fallback drops the cursor to the first tag.
  if (el && (el.id === 'image-tags' || el.id === 'sidebar-inner') &&
      document.body.classList.contains('tag-focus')) {
    var stash = document.body.dataset.tagFocusIdx;
    if (stash !== undefined) {
      delete document.body.dataset.tagFocusIdx;
      var items = tagFocusRows();
      if (items.length === 0) {
        document.body.classList.remove('tag-focus');
      } else {
        var prev = Math.max(0, parseInt(stash, 10) - 1);
        if (prev >= items.length) prev = items.length - 1;
        items[prev].classList.add('focused');
        items[prev].scrollIntoView({block: 'nearest'});
      }
    }
  }

  if (!el || el.id !== 'job-status') return;

  // Latch first-settle once per page load. Used below to suppress the
  // initial done-state reload when the server-rendered DOM already reflects
  // the just-finished job.
  var firstSettle = _firstJobStatusSettle;
  _firstJobStatusSettle = false;

  var isDone = !!el.querySelector('.job-done');
  var isErr  = !!el.querySelector('.job-error');
  // `job-running` lives on #job-status itself, not a descendant, so use
  // classList instead of querySelector (which would always miss it).
  var isRunning = el.classList.contains('job-running');
  var finishedAt = el.dataset.finishedAt || '';

  // Running progress-emitting jobs: refresh the gallery grid whenever the
  // worker's Processed cursor advances so new ingests, deletions, and
  // re-extractions show up without a manual reload. The listed types call
  // jobs.Update(processed,...) inside their worker loops; others either
  // finish quickly (watcher events surface via FinishedAt) or don't
  // visibly alter the gallery during the run. Sits above the isIdle bail
  // so the running state reaches this branch.
  //
  // WatcherNotices covers the other half: the filesystem watcher may ingest
  // or remove files while any job runs, and autotag's own cursor doesn't
  // reflect those. A bump triggers the same grid refresh regardless of job
  // type (sync drops watcher events upstream, so its counter stays at 0).
  var jobType = el.dataset.jobType || '';
  var processed = parseInt(el.dataset.processed || '0', 10);
  var watcherNotices = parseInt(el.dataset.watcherNotices || '0', 10);
  var refreshDuringRun = jobType === 'sync' || jobType === 'delete' || jobType === 're-extract';
  if (!isRunning) {
    _lastJobProcessed = -1;
    _lastWatcherNotices = -1;
  } else {
    var needRefresh = false;
    if (refreshDuringRun && processed > 0 && processed !== _lastJobProcessed) {
      _lastJobProcessed = processed;
      needRefresh = true;
    }
    if (watcherNotices > 0 && watcherNotices !== _lastWatcherNotices) {
      _lastWatcherNotices = watcherNotices;
      needRefresh = true;
    }
    if (needRefresh) {
      refreshGalleryGrid();
    }
  }

  var isIdle = !isDone && !isErr && !isRunning;

  // Reset auto-clear flag when job-status goes idle (dismissed)
  if (isIdle) {
    _jobAutoClearFinishedAt = '';
    if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
    return;
  }

  // Auto-clear 30s after the last surface event. Re-arm whenever FinishedAt
  // advances so rolling watcher events during a batch keep the widget alive.
  if ((isDone || isErr) && finishedAt && finishedAt !== _jobAutoClearFinishedAt) {
    _jobAutoClearFinishedAt = finishedAt;
    if (_jobAutoClearTimer) clearTimeout(_jobAutoClearTimer);
    _jobAutoClearTimer = setTimeout(function() {
      _jobAutoClearFinishedAt = '';
      dismissJobStatus();
    }, 30000);
  }

  // Post-delete: full reload guarantees the gallery / tags table
  // reflects the deletions once the background job finishes.
  if (isDone && _pendingGalleryReload) {
    if (finishedAt) _lastReloadedFinishedAt = finishedAt;
    if (document.getElementById('gallery-grid') || document.getElementById('tags-page') || document.getElementById('tag-detail-page') || document.getElementById('collections-page')) {
      var pendingDone = el.querySelector('.job-done');
      if (pendingDone) stashActionFlash(escapeHTML(pendingDone.textContent || ''), 'ok');
      // The flag stays armed: location.reload() does not stop this script,
      // so clearing it here would say "no navigation pending" while one is
      // in flight. The next document starts with it false anyway.
      window.location.reload();
      return;
    }
    _pendingGalleryReload = false;
  }

  // Reload gallery grid or detail tags once per completion event. The
  // data-finished-at attribute changes whenever the server records a new
  // event (e.g. sync complete, watcher add/remove), so reloads no longer
  // latch on a single flag.
  if (isDone && finishedAt && finishedAt !== _lastReloadedFinishedAt) {
    _lastReloadedFinishedAt = finishedAt;
    if (firstSettle) return;

    // Relations hub: its queue and group counters are computed at render
    // time, so a finished find-pairs or phash job leaves them stale until a
    // reload.
    if (document.getElementById('relations-page') && (jobType === 'relations' || jobType === 'phash')) {
      window.location.reload();
      return;
    }

    // Gallery page: reload grid + lift the job summary into the inline
    // flash slot so the user sees the result without having to scan the
    // top-right job-status widget. A check changed nothing, so refreshing
    // the grid would only cost the selection it was run against.
    var grid = jobType === 'check' ? null : document.getElementById('gallery-grid');
    if (grid) {
      var doneEl = el.querySelector('.job-done');
      if (doneEl) showActionFlash(escapeHTML(doneEl.textContent || ''), 'ok');
      refreshGalleryGrid();
    }

    // Detail page: reload the tag list so freshly added auto-tags show up.
    // The "Auto-tagger started..." / completion flash rides the shared
    // monbooru:flash slot which self-dismisses after 5 s.
    var imageTags = document.getElementById('image-tags');
    if (imageTags) {
      var imageId = imageTags.dataset.imageId;
      if (imageId && window.htmx) {
        window.htmx.ajax('GET', '/images/' + imageId + '/tags', {target: '#image-tags', swap: 'outerHTML'});
      }
    }
  }
});

function getCSRFToken() {
  var meta = document.querySelector('meta[name="csrf-token"]');
  if (meta) return meta.content;
  var input = document.querySelector('input[name="_csrf"]');
  return input ? input.value : '';
}

// scopeCount populates the dialog's count + noun span pair from the
// current search-result span (search scope) or the checked-thumb count
// (selection scope). Returns the resolved count so callers can early-
// return on an empty selection.
function scopeCount(scope, countEl, nounEl) {
  var n = scope === 'selection' ? pickedIDs.length : matchingTotal();
  if (countEl) countEl.textContent = n;
  if (nounEl) {
    var suffix = n === 1 ? 'image' : 'images';
    nounEl.textContent = scope === 'selection' ? 'selected ' + suffix : suffix + ' in current search';
  }
  return n;
}

// The listing on screen, not the form: the box can hold text typed but never
// submitted, while the URL is what the grid and its match count were built from.
function searchScopeParts() {
  var params = new URLSearchParams(location.search);
  var sortEl = document.getElementById('search-sort');
  var orderEl = document.querySelector('#search-form select[name="order"]');
  var parts = ['q=' + encodeURIComponent(params.get('q') || ''),
               'sort=' + encodeURIComponent(params.get('sort') || (sortEl ? sortEl.value : 'newest')),
               'order=' + encodeURIComponent(params.get('order') || (orderEl ? orderEl.value : 'desc'))];
  // A random sort's order is the seed's; without it a job that numbers its
  // scope by position walks a different shuffle from the one on screen.
  var seed = params.get('seed');
  if (seed) parts.push('seed=' + encodeURIComponent(seed));
  return parts;
}

// selectedImageIds returns the picked ids, for callers that build their own
// request body. The list, not the checkboxes: the pick reaches past the page.
function selectedImageIds() {
  return pickedIDs.slice();
}

// selectionScopeIds returns the checked-thumb id parts. Returns null
// when nothing is checked (caller writes the flash and aborts).
// One comma-joined parameter rather than one per id: a selection built up
// across pages can pass net/url's 10000-parameter cap, and the form parser
// refuses the whole request before a handler sees it.
function selectionScopeIds() {
  var ids = selectedImageIds();
  if (ids.length === 0) return null;
  return ['ids=' + encodeURIComponent(ids.join(','))];
}

// batchScopeParams resolves a batch dialog's scope into request params: the
// current search, or the checked thumbs. Returns null after writing the flash
// when the selection is empty, which is the caller's cue to stop.
function batchScopeParams(scope, flash) {
  if (scope === 'search') return searchScopeParts();
  var ids = selectionScopeIds();
  if (!ids) {
    if (flash) flash.innerHTML = '<div class="flash flash-err">No images selected.</div>';
    return null;
  }
  return ids;
}

// relayPlugin carries a plugin's relay click to the server with the scope
// its surface holds: the detail page's own image, or the gallery's current
// selection. The peer's answer comes back as a flash trigger.
function relayPlugin(btn) {
  var pinned = btn.closest('[data-image-id]');
  var ids = pinned ? [pinned.dataset.imageId] : selectedImageIds();
  if (!ids.length || !window.htmx) return;
  window.htmx.ajax('POST', '/internal/plugin/relay', {
    swap: 'none',
    values: {_csrf: getCSRFToken(), plugin: btn.dataset.plugin, button: btn.dataset.button, ids: ids}
  });
}

// openPluginPage shows a plugin's own page without leaving monbooru. The
// page is served through monbooru itself (/plugins/<name>/...), so it opens
// wherever monbooru does; a plugin sending the browser on to its {back_url}
// is what says it is done, and the pop-in closes onto a reloaded page.
function openPluginPage(btn) {
  var dlg = document.getElementById('plugin-open-dialog');
  var frame = document.getElementById('plugin-open-frame');
  if (!dlg || !frame) return;
  dlg.querySelector('.plugin-open-title').textContent = btn.dataset.peer + ': ' + btn.textContent.trim();
  dlg.querySelector('.plugin-open-tab').href = btn.dataset.href;
  if (!frame.dataset.wired) {
    frame.dataset.wired = '1';
    frame.addEventListener('load', pluginPageNavigated);
    // Escape closes the dialog without passing through Close.
    dlg.addEventListener('close', function() { frame.src = 'about:blank'; });
  }
  frame.src = btn.dataset.href;
  dlg.showModal();
}

function pluginPageNavigated(e) {
  var dlg = document.getElementById('plugin-open-dialog');
  if (!dlg || !dlg.open) return;
  var path;
  try {
    path = e.target.contentWindow.location.pathname;
  } catch (err) {
    return; // the plugin sent the browser off monbooru's origin entirely
  }
  if (path.indexOf('/plugins/') === 0) return;
  closePluginPage();
  window.location.reload();
}

function closePluginPage() {
  var dlg = document.getElementById('plugin-open-dialog');
  if (dlg && dlg.open) dlg.close();
}

// The selection survives by default - a scope the operator built is usually
// worth more than one action. opts: endpoint, scope, params (array of
// already-encoded "k=v" parts not including _csrf or scope), dialogId,
// flashId, failMsg, consumesScope (the rows are gone afterwards, so keeping
// them picked would only misreport the count).
function runBatchOp(opts) {
  var flash = document.getElementById(opts.flashId);
  if (flash) flash.innerHTML = '';
  var parts = ['scope=' + encodeURIComponent(opts.scope)].concat(opts.params || []);
  postForm(opts.endpoint, new URLSearchParams(parts.join('&')), {
    dialogId: opts.dialogId,
    flashId: opts.flashId,
    failMsg: opts.failMsg || 'Action failed.',
    onOK: function() {
      if (opts.consumesScope) clearSelection();
      _pendingGalleryReload = true;
      refreshJobStatus();
    }
  });
}

// openBatchDialog runs the shared open skeleton for a batch dialog whose
// elements share an id prefix (#<prefix>-dialog/-scope/-count/-noun/-flash):
// guard an empty selection, fill the count + noun pair, stamp the scope,
// reset the per-dialog extras, clear the flash, showModal. opts:
//   countId       - count element id when it isn't <prefix>-count
//   requireMatches - also abort a search-scope open that matches 0 images
//   radio         - selector of the radio re-checked to its default
//   clearIds      - element ids emptied on open (inputs get value='',
//                   suggest dropdowns innerHTML='')
//   beforeShow    - hook run after the resets, before showModal
//   focusId       - input focused after showModal
function openBatchDialog(prefix, scope, opts) {
  opts = opts || {};
  // An escalated bar is still clicked through the selection-scoped buttons,
  // so the flip happens once here and every dialog reads the search scope it
  // already knows how to post.
  if (scope === 'selection' && selectAllMatching) scope = 'search';
  if (scope === 'selection' && pickedIDs.length === 0) return;
  var n = scopeCount(scope, document.getElementById(opts.countId || prefix + '-count'),
                     document.getElementById(prefix + '-noun'));
  if (opts.requireMatches && scope === 'search' && n === 0) return;
  document.getElementById(prefix + '-scope').value = scope;
  if (opts.radio) {
    var dflt = document.querySelector(opts.radio);
    if (dflt) dflt.checked = true;
  }
  (opts.clearIds || []).forEach(function(id) {
    var el = document.getElementById(id);
    if (!el) return;
    if (el.tagName === 'INPUT') el.value = ''; else el.innerHTML = '';
  });
  var flash = document.getElementById(prefix + '-flash');
  if (flash) flash.innerHTML = '';
  var dlg = document.getElementById(prefix + '-dialog');
  if (opts.beforeShow) opts.beforeShow();
  dlg.showModal();
  var focusEl = opts.focusId ? document.getElementById(opts.focusId) : null;
  if (focusEl) focusEl.focus();
}

// clearSlots empties each listed element by id, skipping ids absent from
// the page: the shared "reset error/suggest/flash slots" step of dialog-open.
function clearSlots() {
  for (var i = 0; i < arguments.length; i++) {
    var el = document.getElementById(arguments[i]);
    if (el) el.innerHTML = '';
  }
}

// A suggest request still inside its debounce when the form submits lands
// after the answer and paints its dropdown over it. The dismissal handlers
// only drop a response that arrives while the input has lost focus, and these
// inputs keep it; disarming marks the input so the pending response is
// dropped too, and the next keystroke re-arms it.
function disarmSuggest(inputId) {
  var input = document.getElementById(inputId);
  if (input) input.dataset.suggestStale = '1';
}

function suggestDisarmed(input) {
  if (!input.dataset.suggestStale) return false;
  delete input.dataset.suggestStale;
  return true;
}

// A PTR spelling picked from the look-up dialog's candidate list: fill the
// input and run the preview, so one click goes from "which name?" to "what
// would a pull under it do?".
function applyPTRSpelling(btn) {
  var input = document.getElementById('ptr-lookup-as');
  var form = document.getElementById('ptr-lookup-form');
  if (!input || !form) return;
  input.value = btn.dataset.spelling;
  form.requestSubmit();
}

// openHxRewriteDialog repoints a form's htmx attribute (attr is 'hx-post'
// or 'hx-patch') at url, re-processes the form so htmx picks up the new
// target, and opens the dialog. Callers prefill before, focus after.
function openHxRewriteDialog(formId, attr, url, dialogId) {
  var form = document.getElementById(formId);
  form.setAttribute(attr, url);
  if (window.htmx) window.htmx.process(form);
  document.getElementById(dialogId).showModal();
}

// confirmBatchSimple runs the shared confirm shape: resolve the scope the
// open path stashed in #<prefix>-scope into query or id params (flashing
// on an empty selection), then post through runBatchOp. extraParams are
// already-encoded "k=v" parts placed before the scope params.
function confirmBatchSimple(prefix, endpoint, extraParams, failMsg) {
  var scope = document.getElementById(prefix + '-scope').value;
  var scoped = batchScopeParams(scope, document.getElementById(prefix + '-flash'));
  if (!scoped) return;
  var params = (extraParams || []).concat(scoped);
  runBatchOp({endpoint: endpoint, scope: scope, params: params,
              dialogId: prefix + '-dialog', flashId: prefix + '-flash', failMsg: failMsg});
}

// postForm sends a form-encoded request with the page CSRF token in both
// the body and the X-CSRF-Token header, then closes the dialog on success
// or flashes the error body. params is a URLSearchParams or plain object
// (or null); _csrf is appended here. opts:
//   method   - HTTP verb, default 'POST'
//   hx       - also send the HX-Request header (relations bulk endpoints)
//   okStatus - success only on this exact status; default is res.ok
//   dialogId - dialog closed on success
//   onOK     - called with the response after the dialog close
//   flashId  - element that receives the error body / failure flashes
//   failMsg  - fallback error text when the error body is empty
//   onErr    - called with the error body text instead of the flash write
//   catchMsg - network-failure flash text (default 'Request failed.');
//              pass null to keep a converted site's original no-catch shape
function postForm(url, params, opts) {
  opts = opts || {};
  var csrf = getCSRFToken();
  var body = params instanceof URLSearchParams ? params : new URLSearchParams(params || {});
  body.append('_csrf', csrf);
  var headers = {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf};
  if (opts.hx) headers['HX-Request'] = 'true';
  var flash = opts.flashId ? document.getElementById(opts.flashId) : null;
  var p = fetch(url, {
    method: opts.method || 'POST',
    headers: headers,
    body: body.toString()
  }).then(function(res) {
    var ok = opts.okStatus ? res.status === opts.okStatus : res.ok;
    if (ok) {
      var dlg = opts.dialogId ? document.getElementById(opts.dialogId) : null;
      if (dlg) dlg.close();
      if (opts.onOK) opts.onOK(res);
    } else {
      res.text().then(function(t) {
        if (opts.onErr) { opts.onErr(t); return; }
        if (flash) flash.innerHTML = t || (opts.failMsg ? '<div class="flash flash-err">' + opts.failMsg + '</div>' : '');
      });
    }
  });
  if (opts.catchMsg !== null) {
    p.catch(function() {
      if (flash) flash.innerHTML = '<div class="flash flash-err">' + (opts.catchMsg || 'Request failed.') + '</div>';
    });
  }
}

// mirrorJobSummary copies the finished job's own words into a dialog slot.
// A job reports in the topbar widget, which a modal backdrop dims, so a
// dialog that stays open through one has to bring the answer to where the
// operator is looking.
function mirrorJobSummary(flashId) {
  var status = document.getElementById('job-status');
  if (!status || !status.parentNode) return;
  var startedAt = status.dataset.finishedAt || '';
  var obs = new MutationObserver(function() {
    var now = document.getElementById('job-status');
    if (!now || (now.dataset.finishedAt || '') === startedAt) return;
    var done = document.getElementById('job-done-msg');
    var failed = document.getElementById('job-error-msg');
    if (!done && !failed) return;
    obs.disconnect();
    var node = done || failed;
    setFlashText(document.getElementById(flashId), done ? 'ok' : 'err',
                 node.getAttribute('title') || node.textContent);
  });
  obs.observe(status.parentNode, {childList: true, subtree: true});
}

function dismissJobStatus() {
  _lastReloadedFinishedAt = '';
  _lastJobProcessed = -1;
  _lastWatcherNotices = -1;
  _jobAutoClearFinishedAt = '';
  if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
  postForm('/internal/job/dismiss');
  var js = document.getElementById('job-status');
  if (js) {
    js.innerHTML = '';
    js.removeAttribute('hx-trigger');
    if (window.htmx) window.htmx.process(js);
  }
}

// cancelJobStatus interrupts the running auto-tagging job. The worker observes
// ctx.Done() and wraps up via Complete; the 30s auto-dismiss takes over from
// there so the user still sees a "cancelled" summary on the status bar.
function cancelJobStatus() {
  postForm('/internal/job/cancel');
}

// Shared suggest-dropdown keyboard navigation (search, tag input, merge)
function handleSuggestKey(e, dropdownId, inputId) {
  var dd = document.getElementById(dropdownId);
  if (!dd) return;
  var items = Array.from(dd.querySelectorAll('.suggest-item'));
  var focused = dd.querySelector('.suggest-item.kbd-focused');
  var idx = focused ? items.indexOf(focused) : -1;
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    e.preventDefault();
    items.forEach(function(i){ i.classList.remove('kbd-focused'); });
    idx = e.key === 'ArrowDown' ? Math.min(idx + 1, items.length - 1) : Math.max(idx - 1, 0);
    if (idx >= 0) {
      items[idx].classList.add('kbd-focused');
      items[idx].scrollIntoView({ block: 'nearest' });
    }
  } else if (e.key === 'Enter' && focused) {
    e.preventDefault();
    focused.click();
  } else if (e.key === 'Escape') {
    dd.innerHTML = '';
  }
}

// A suggest dropdown is closed by a click outside it, or by a submit of the
// enclosing form (Enter or the Search button). Without the submit hook the
// dropdown stays visible on top of the result page until the next outside
// click. suggestPairs is every dropdown and the input that fills it, plus
// the two per-surface quirks:
//
// blurOnSubmit: the search form navigates away, so page-level arrow-key
// gallery navigation should kick in after Enter without an extra Escape;
// the tag and folder dropdowns are built for repeated entry and keep focus.
//
// clearOnEmpty: drop a suggest swap that lands while the input is empty.
// The tag input keeps focus while a successful add clears it, so the focus
// check alone can't catch its late responses. Not for the label inputs,
// whose empty-prefix listing is served on purpose.
var suggestPairs = {
  'search-suggest': {input: 'search-input', blurOnSubmit: true, clearOnEmpty: true},
  'tag-suggest-dropdown': {input: 'tag-input', clearOnEmpty: true},
  'batch-place-suggest': {input: 'batch-place-folder'},
  'place-image-suggest': {input: 'place-image-folder'},
  'batch-tag-suggest': {input: 'batch-tag-input', clearOnEmpty: true},
  'batch-strip-suggest': {input: 'batch-strip-input', clearOnEmpty: true},
  'source-suggest': {input: 'source-site-input'},
  'batch-series-search-suggest': {input: 'batch-series-search-input'},
  'batch-series-selected-suggest': {input: 'batch-series-selected-input'},
  'batch-collection-suggest': {input: 'batch-collection-input'},
  'collection-suggest': {input: 'collection-name-input'},
  'collection-rename-suggest': {input: 'collection-rename-input'},
  'alias-create-suggest': {input: 'alias-create-canon', clearOnEmpty: true},
  'batch-alias-suggest': {input: 'batch-alias-canon', clearOnEmpty: true},
  'batch-imply-suggest': {input: 'batch-imply-target', clearOnEmpty: true},
  'detail-alias-suggest': {input: 'detail-alias-canon', clearOnEmpty: true},
  'implication-add-suggest': {input: 'implication-add-input', clearOnEmpty: true},
  'implied-by-add-suggest': {input: 'implied-by-add-input', clearOnEmpty: true},
  'ptr-lookup-suggest': {input: 'ptr-lookup-as', clearOnEmpty: true},
};

var suggestDropdownByInput = {};
for (var _suggestID in suggestPairs) suggestDropdownByInput[suggestPairs[_suggestID].input] = _suggestID;

// One registration of each handler over the whole table, not one per pair.
// They resolve their elements per event: some surfaces live in re-rendered
// fragments (the implications editor, the danger zone's move dialog), where
// listeners bound to the initial nodes would be orphaned by the first
// rebuild. htmx events bubble, so document-level delegation keeps seeing
// swaps on the fresh nodes.
document.addEventListener('click', function(e) {
  for (var ddId in suggestPairs) {
    var dd = document.getElementById(ddId);
    if (dd && !dd.contains(e.target) && e.target.id !== suggestPairs[ddId].input) {
      dd.innerHTML = '';
    }
  }
});

document.addEventListener('submit', function(e) {
  for (var ddId in suggestPairs) {
    var input = document.getElementById(suggestPairs[ddId].input);
    if (!input || e.target !== input.form) continue;
    var dd = document.getElementById(ddId);
    if (dd) dd.innerHTML = '';
    if (suggestPairs[ddId].blurOnSubmit) input.blur();
  }
});

// A pending suggest request (debounced 200ms by hx-trigger) can land after
// the user submits or moves focus elsewhere; drop the swap if the input no
// longer holds focus (or, with clearOnEmpty, sits empty) so the dropdown
// doesn't get refilled behind the user's back.
//
// Also tag the dropdown as `suggest-fresh` after every swap so the CSS can
// suppress the :hover highlight until the user actually moves the mouse over
// it - otherwise an item happening to land under the cursor's previous
// position appears "selected" without the user picking it.
document.addEventListener('htmx:afterSwap', function(e) {
  var pair = suggestPairs[e.target.id];
  if (!pair) return;
  var input = document.getElementById(pair.input);
  if (document.activeElement !== input || (pair.clearOnEmpty && input.value === '')) { e.target.innerHTML = ''; return; }
  e.target.classList.add('suggest-fresh');
  e.target.addEventListener('mousemove', clearSuggestFresh, {once: true});
});

// The label and search suggest endpoints answer 204 when nothing matches,
// and htmx swaps nothing on 204 - the previous matches would stay showing
// under text they no longer match.
document.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail || !e.detail.elt) return;
  var ddId = suggestDropdownByInput[e.detail.elt.id];
  if (!ddId) return;
  if (e.detail.xhr && e.detail.xhr.status === 204) {
    var dd = document.getElementById(ddId);
    if (dd) dd.innerHTML = '';
  }
});

// mergeChoiceItem builds one row of a merge dialog's choice list: the radio
// naming which of the selected rows wins, plus whatever that page hangs
// beside it (a thumbnail and a group label, or a coloured name and its
// category).
function mergeChoiceItem(name, value, checked, decorate) {
  var li = document.createElement('li');
  li.className = 'merge-dup-choice';
  var label = document.createElement('label');
  var radio = document.createElement('input');
  radio.type = 'radio';
  radio.name = name;
  radio.value = value;
  radio.checked = checked;
  label.appendChild(radio);
  decorate(label);
  li.appendChild(label);
  return li;
}

// clearSuggestFresh re-enables the dropdown's :hover highlight on the first
// real mouse move; re-armed by the afterSwap handler above.
function clearSuggestFresh(e) {
  e.currentTarget.classList.remove('suggest-fresh');
}


// Detail page: tags added in the current session are echoed in a
// "just-added" list, reset on full page reload.
var _initialTagIDs = null;
var _addedTagOrder = [];

function captureInitialTags() {
  if (_initialTagIDs !== null) return;
  _initialTagIDs = new Set();
  sidebarTagRows().forEach(function(li) { _initialTagIDs.add(li.dataset.tagId); });
}

// separateNewTags mirrors the session's own adds under the editor. The
// sidebar holds the image's tags; a row that wasn't there at load and
// carries the user's attribution is something typed here, so it gets a
// chip. Anything a tagger or a source brought in meanwhile joins the
// baseline instead.
function separateNewTags() {
  var added = document.querySelector('.tag-list-added');
  if (!added) return;
  if (_initialTagIDs === null) { captureInitialTags(); }

  var rowsById = {};
  sidebarTagRows().forEach(function(li) {
    var id = li.dataset.tagId;
    if (_initialTagIDs.has(id)) return;
    if (li.dataset.source !== 'user') { _initialTagIDs.add(id); return; }
    rowsById[id] = li;
    if (_addedTagOrder.indexOf(id) === -1) _addedTagOrder.push(id);
  });
  _addedTagOrder = _addedTagOrder.filter(function(id) { return rowsById[id] !== undefined; });

  added.innerHTML = '';
  _addedTagOrder.forEach(function(id) { added.appendChild(justAddedChip(rowsById[id])); });

  var title = document.querySelector('.tag-list-added-title');
  if (title) title.hidden = _addedTagOrder.length === 0;
}

// justAddedChip renders a sidebar row as a chip for the "Just added"
// list. Its x defers to the sidebar row's own button so the removal
// keeps riding the htmx wiring the server rendered.
function justAddedChip(row) {
  var link = row.querySelector('.tag-link');
  var li = document.createElement('li');
  li.className = 'tag-item';
  li.dataset.tagId = row.dataset.tagId;
  if (link) li.style.color = link.style.color;
  var a = document.createElement('a');
  a.className = 'tag-chip-name';
  a.href = link ? link.getAttribute('href') : '#';
  a.textContent = link ? link.textContent.trim() : '';
  li.appendChild(a);
  var rm = document.createElement('button');
  rm.type = 'button';
  rm.className = 'tag-remove-btn';
  rm.title = 'Remove tag';
  rm.textContent = '×';
  rm.onclick = function() {
    var btn = row.querySelector('.tag-entry-remove');
    if (btn) btn.click();
  };
  li.appendChild(rm);
  return li;
}

// The tag list and the "Just added" mirror live in two containers the
// server swaps in one response, so either settling is a cue to rebuild.
document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail ? e.detail.elt : null;
  if (!el || (el.id !== 'image-tags' && el.id !== 'sidebar-inner')) return;
  separateNewTags();
  // Re-anchor tag-focus after the swap. The removed row takes the
  // .focused class with it, so without this the next ArrowRight / Enter
  // has no cursor to act on and the user has to press r again to keep
  // deleting.
  if (document.body.classList.contains('tag-focus') && !focusedTagRow()) {
    var first = tagFocusRows()[0];
    if (first) {
      first.classList.add('focused');
      first.scrollIntoView({block: 'nearest'});
    } else {
      // Image has no tags left - leave the mode rather than holding it
      // open against an empty list.
      document.body.classList.remove('tag-focus');
    }
  }
});

document.addEventListener('DOMContentLoaded', function() {
  if (document.getElementById('image-tags')) captureInitialTags();
});


// Mobile sidebar drawer: at narrow widths the topbar wraps to several
// rows (nav + topbar-right + logo), so the drawer's hard-coded top:42px
// would cover the wrapped nav links. Mirror the real topbar height into
// a --topbar-h custom property so the drawer always slots in below it.
(function () {
  var root = document.documentElement;
  function sync() {
    var tb = document.getElementById('topbar');
    if (!tb) return;
    root.style.setProperty('--topbar-h', Math.round(tb.getBoundingClientRect().height) + 'px');
  }
  document.addEventListener('DOMContentLoaded', sync);
  window.addEventListener('resize', sync);
  window.addEventListener('load', sync);
})();

// Reader: clicking the page counter opens the page-jump dialog.
document.addEventListener('DOMContentLoaded', function() {
  var counter = document.getElementById('reader-counter');
  if (counter && document.getElementById('reader-jump-dialog')) {
    counter.addEventListener('click', function(e) {
      e.preventDefault();
      openReaderJumpDialog();
    });
  }
});

// Pages grid: when the URL carries a #page-N fragment (set by the
// reader's Back / Pages link so the operator returns to the cell they
// were just reading), focus that cell so the keyboard nav picks up
// from the right spot and the cell is visually anchored.
document.addEventListener('DOMContentLoaded', function() {
  if (!document.getElementById('pages-grid-page')) return;
  var hash = window.location.hash;
  if (!hash || hash.indexOf('#page-') !== 0) return;
  var target = document.querySelector(hash + '.manga-page-cell');
  if (!target) return;
  var cells = Array.from(document.querySelectorAll('.manga-page-cell'));
  // block: 'center' - the return-from-reader landing wants the cell
  // anchored mid-viewport, not just nudged into view.
  setFocused(cells, cells.indexOf(target), 'center');
});

// initInboxUpload wires the inline drop zone the gallery renders at the
// top of the grid whenever the query positively asserts inbox:true.
// Idempotent via a data-wired latch on the drop zone so repeat calls
// (htmx grid swaps) don't pile up listeners. The drop zone reuses the
// upload page's CSS hooks; the post-submit reload uses htmx.ajax against
// the current URL so the new inbox rows show up without a full reload.
function initInboxUpload() {
  var dz = document.getElementById('inbox-upload-drop');
  var input = document.getElementById('inbox-upload-file-input');
  var list = document.getElementById('inbox-upload-file-list');
  var pickBtn = document.getElementById('inbox-upload-pick-btn');
  var form = document.getElementById('inbox-upload-form');
  var submitBtn = document.getElementById('inbox-upload-submit-btn');
  var resetBtn = document.getElementById('inbox-upload-reset-btn');
  var result = document.getElementById('inbox-upload-result');
  if (!dz || !input || !list || !form || dz.dataset.wired === '1') return;
  dz.dataset.wired = '1';

  // _pending owns the staged FileList. The browser's native file
  // picker overwrites input.files on every change, so we mirror the
  // accumulating set here and copy it back onto input before submit.
  var _pending = new DataTransfer();

  function renderList() {
    list.innerHTML = '';
    var files = _pending.files;
    for (var i = 0; i < files.length; i++) {
      var li = document.createElement('li');
      var kib = Math.round(files[i].size / 1024);
      var sizeText = kib > 0 ? (kib + ' KiB') : '<1 KiB';
      li.textContent = files[i].name + ' (' + sizeText + ')';
      list.appendChild(li);
    }
    input.files = _pending.files;
    var empty = files.length === 0;
    if (submitBtn) submitBtn.disabled = empty;
    if (resetBtn) resetBtn.disabled = empty;
  }

  function appendFiles(incoming) {
    for (var i = 0; i < incoming.length; i++) _pending.items.add(incoming[i]);
    renderList();
  }

  function clearPending() {
    _pending = new DataTransfer();
    renderList();
    if (result) result.innerHTML = '';
  }

  input.addEventListener('change', function() {
    // Browser picker returned a fresh selection; append onto the
    // pending set rather than replace it. The renderList sync at the
    // end of appendFiles writes _pending.files back over input.files
    // so the form submit carries the full accumulated set.
    if (input.files && input.files.length) appendFiles(input.files);
  });
  if (pickBtn) {
    pickBtn.addEventListener('click', function(e) { e.preventDefault(); input.click(); });
  }
  if (resetBtn) {
    resetBtn.addEventListener('click', function(e) { e.preventDefault(); clearPending(); });
  }
  // Initial empty state disables submit / reset until the user picks
  // or drops a file. An empty submit posts /upload as a no-op (server
  // 200, no rows added) and an empty reset does nothing visible;
  // surfacing the disabled state up front is honest.
  renderList();

  ['dragenter', 'dragover'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.add('drag-over'); });
  });
  ['dragleave', 'drop'].forEach(function(ev) {
    dz.addEventListener(ev, function(e) { e.preventDefault(); dz.classList.remove('drag-over'); });
  });
  dz.addEventListener('drop', function(e) {
    if (!e.dataTransfer || !e.dataTransfer.files) return;
    appendFiles(e.dataTransfer.files);
  });

  // htmx fires htmx:xhr:progress for the request upload and the response
  // download alike; latch at 100% so the tiny response's own progress
  // events don't rewind the counter the operator is watching.
  var uploadDone = false;
  form.addEventListener('htmx:beforeRequest', function() {
    uploadDone = false;
    if (submitBtn) submitBtn.disabled = true;
    if (result) result.innerHTML = '<div class="field-hint">Uploading...</div>';
  });
  form.addEventListener('htmx:xhr:progress', function(e) {
    if (uploadDone || !result || !e.detail || !e.detail.lengthComputable || !e.detail.total) return;
    var pct = Math.round((e.detail.loaded / e.detail.total) * 100);
    if (pct >= 100) {
      uploadDone = true;
      result.innerHTML = '<div class="field-hint">Processing...</div>';
      return;
    }
    result.innerHTML = '<div class="field-hint">Uploading... ' + pct + '%</div>';
  });
  form.addEventListener('htmx:afterRequest', function(e) {
    if (submitBtn) submitBtn.disabled = false;
    if (!e.detail.successful) return;
    // The upload handler renders the summary flash into #inbox-upload-result;
    // that slot lives inside #gallery-grid and the upcoming refresh would
    // wipe it. Lift the text into the gallery-level slot before the swap.
    if (result && result.innerHTML.trim() !== '') {
      showActionFlash(result.innerHTML, /flash-err/.test(result.innerHTML) ? 'err' : 'ok');
    }
    clearPending();
    // Refresh the grid so the freshly ingested rows surface at the top
    // of the inbox. Same idiom the job-status auto-refresh uses.
    refreshGalleryGrid();
  });
}

document.addEventListener('DOMContentLoaded', initInboxUpload);

// mergeCategoryCollision re-runs an inline category move with merge=1
// after the collision flash offered it. Shared by the /tags listing and
// the tag detail page, which both carry a #tag-flash slot.
function mergeCategoryCollision(tagID, catID) {
  var body = new URLSearchParams();
  body.set('category_id', String(catID));
  body.set('merge', '1');
  fetch('/tags/' + tagID + '/category', {
    method: 'PATCH',
    headers: {'X-CSRF-Token': getCSRFToken(), 'Content-Type': 'application/x-www-form-urlencoded'},
    body: body
  }).then(function(r) {
    if (r.ok) {
      stashActionFlash('Merged into the existing tag.', 'ok');
      window.location.reload();
    } else {
      // Raw fetch, so the handler answers with an unescaped plain-text
      // body; tag names legally carry < and >.
      r.text().then(function(t) {
        setFlashText(document.getElementById('tag-flash'), 'err', t || 'Merge failed.');
      });
    }
  });
}

// deleteTagFlow is the confirm-then-DELETE a tag goes through from both
// the /tags listing and the tag detail page. The two differ only in
// where the id comes from and where they go afterwards, so onSuccess
// carries the navigation.
function deleteTagFlow(btn, tagID, onSuccess) {
  var name = btn.dataset.name;
  var isRating = btn.dataset.category === 'rating';
  var msg = isRating
    ? 'Strip "' + name + '" from every image?'
    : 'Delete tag "' + name + '" and remove it from every image?';
  var danger = isRating
    ? 'The rating tag itself stays in the catalog and can be re-added later.'
    : 'This cannot be undone.';
  showConfirm(msg, function() {
    // The cascade on a heavily-used tag takes seconds; surface the wait
    // and block a second click until the DELETE answers.
    btn.disabled = true;
    var el = document.getElementById('tag-flash');
    setFlashText(el, 'warn', (isRating ? 'Stripping' : 'Deleting') + ' "' + name + '"…');
    fetch('/tags/' + tagID, {
      method: 'DELETE',
      headers: {'X-CSRF-Token': getCSRFToken()}
    }).then(function(r) {
      if (r.ok) {
        // The DELETE returns 204; raw fetch doesn't process HX-Trigger,
        // so seed the shared stash directly before navigating.
        stashActionFlash(escapeHTML(isRating
          ? 'Stripped "' + name + '" from every image.'
          : 'Tag "' + name + '" deleted.'), 'ok');
        onSuccess();
      } else {
        btn.disabled = false;
        r.text().then(function(t) {
          setFlashText(el, 'err', t || 'Delete failed.');
        });
      }
    }).catch(function() {
      btn.disabled = false;
      setFlashText(el, 'err', 'Delete request failed.');
    });
  }, danger, isRating ? 'Strip' : 'Delete');
}
