'use strict';

var _chordNode = null;
var _chordLabel = '';
var _chordTimer = null;
var _chordHint = null;
var chordTimeoutMs = 500;

// A string leaf is a URL, an array holds selectors whose first match is
// clicked, a function runs, an object is a sub-chord.
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
    n: ['#btn-detail-note'],
    c: ['#tag-batch-bar:not([hidden]) #btn-batch-category', '#btn-detail-category'],
    a: ['#tag-batch-bar:not([hidden]) #btn-batch-alias', '#btn-detail-alias'],
    m: ['#tag-batch-bar:not([hidden]) #btn-batch-merge'],
    i: ['#tag-batch-bar:not([hidden]) #btn-batch-imply'],
    f: ['#tag-batch-bar:not([hidden]) #btn-batch-merge-folded'],
    p: ['#tag-batch-bar:not([hidden]) #btn-batch-ptr'],
  },
  // Empty on purpose: its leaves are the digits, one per plugin button on
  // the bar that is up.
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

// body is overflow:hidden and #content scrolls instead, so the window
// usually does not.
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
    window.scrollTo({ top: top });
  } else {
    s.scrollTo({ top: top });
  }
}

function scrollToTop(el) { scrollPageTo(el, 0); }
function scrollToBottom(el) {
  var s = pageScroller(el);
  scrollPageTo(el, s.scrollHeight);
}

function setFocused(items, idx, block) {
  items.forEach(function(el) { el.classList.remove('focused'); });
  items[idx].classList.add('focused');
  items[idx].scrollIntoView({ block: block || 'nearest' });
}

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
    // The tracks: grid width over card width ignores the gap and overcounts.
    if (grid) cols = Math.max(1, getComputedStyle(grid).gridTemplateColumns.split(' ').length);
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

// A data-ref page was opened from another image's similar list, so back
// unwinds that chain one step at a time.
function detailRefBack() {
  var detailPage = document.getElementById('detail-page');
  if (detailPage && detailPage.dataset.ref && history.length > 1) { history.back(); return true; }
  return false;
}

function detailBack() {
  if (detailRefBack()) return;
  var backLink = document.querySelector('.back-link');
  if (backLink) backLink.click();
  else window.location.href = '/';
}

function navDetailArrow(direction, noun) {
  var word = direction === 'prev' ? 'Previous' : 'Next';
  var a = document.querySelector('.nav-arrow[title^="' + word + ' ' + noun + '"]');
  if (a && a.href) { window.location.href = a.href; return true; }
  return false;
}

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

function pruneRelationGroup(btn) {
  var sub = btn.closest('.relation-origin-sub');
  if (!sub) return;
  var ul = sub.nextElementSibling;
  if (ul && ul.classList.contains('tag-list')) ul.remove();
  sub.remove();
}

// The footer lists the ceilings low to high, so +1 is more permissive.
function cycleRatingCeiling(delta) {
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

// Collection order forces asc: a descending pick carried over would
// render the collection backwards.
function applySortChange(sortEl) {
  if (sortEl.value === 'order') {
    var orderEl = document.querySelector('#search-form select[name="order"]');
    if (orderEl) orderEl.value = 'asc';
  }
  // A seed belongs to the random sort; left on the form it rides into the
  // pushed URL and into Save search.
  if (sortEl.value !== 'random') {
    var seedEl = document.querySelector('#search-form input[name="seed"]');
    if (seedEl) seedEl.remove();
  }
  return submitSearch();
}

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

// Dispatch, not form.submit(): htmx listens for the submit event, which
// submit() never fires.
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

function clickFirstSelector(selectors) {
  for (var i = 0; i < selectors.length; i++) {
    var el = document.querySelector(selectors[i]);
    if (el) { el.click(); return true; }
  }
  return false;
}

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

// A row's data-when must be the selector its key's handler looks for.
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
  // showModal scrolls to the first focusable element, which on a short
  // viewport hides the title.
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
    // Counted off the first row: gridWidth/cellWidth ignores the gap and
    // overcounts.
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
    case 'o':
      if (idx < 0) return false;
      var link = cells[idx].querySelector('a');
      if (link && link.href) { window.location.href = link.href; return true; }
      return false;
  }
  return false;
}

var lightbox = (function () {
  var dlg = null, img = null, stage = null, zoomLbl = null, openLink = null;
  var scale = 1, tx = 0, ty = 0;
  var panning = false, px0 = 0, py0 = 0, tx0 = 0, ty0 = 0;
  // Past 4px (16 squared) a press is a pan, and its release must not
  // close the viewer.
  var pressMoved = false;
  var bound = false;

  function clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }
  // scale multiplies the CSS fit, so 1:1 is read back off the painted box
  // rather than assumed to be 1.
  function oneToOneScale() {
    if (!img || !img.naturalWidth) return 0;
    var r = contentRect();
    if (!r || !r.width) return 0;
    return img.naturalWidth * scale / r.width;
  }
  // 1:1 on an image smaller than the stage sits below 0.5, and the floor
  // must not rule it out.
  function minScale() {
    var one = oneToOneScale();
    return one > 0 ? Math.min(0.5, one) : 0.5;
  }
  function apply() {
    if (!img) return;
    img.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    if (!zoomLbl) return;
    // Against the image's own pixels, not the fit: on a small image the
    // fit is already an upscale.
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
  // object-fit: contain letterboxes the image inside the element, so the
  // painted rect is computed; the letterbox is not "on the image".
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
    // Only a release off the image closes: the first click of a dblclick
    // on it must not.
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
      // The readout needs the natural size, which only arrives after
      // open() sets the src.
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
      // Modifier and middle clicks keep the link's own navigation: a
      // truthy return leaves the default alone.
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

// Global for the templates' inline onclick.
function openLightbox(e, src) { return lightbox.open(e, src); }

function openReaderJumpDialog() {
  var dlg = document.getElementById('reader-jump-dialog');
  if (!dlg) return false;
  dlg.showModal();
  var inp = document.getElementById('reader-jump-input');
  if (inp) { inp.focus(); inp.select(); }
  return true;
}

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
    // Keep the current query: without from=pages the next Esc lands on
    // the detail page, not the pages grid.
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
    // A page handler may already have used this Esc, e.g. to close the
    // compare slider.
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

  if (!isInput && document.querySelector('dialog[open]')) {
    if (e.key === '?' && document.getElementById('shortcuts-help') && document.getElementById('shortcuts-help').open) {
      e.preventDefault();
      document.getElementById('shortcuts-help').close();
    }
    return;
  }

  if (isInput) return;

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

  // Clicks the toggle even where CSS hides it: its handler picks the job
  // by viewport width.
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

  if (e.key === 'g' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    e.preventDefault();
    enterChord(chordMap.g, 'g');
    return;
  }

  if (e.key === 'e' && !e.ctrlKey && !e.metaKey && !e.altKey && isDetailPage()) {
    e.preventDefault();
    enterChord(chordMap.e, 'e');
    return;
  }

  if (e.key === 'x' && !e.ctrlKey && !e.metaKey && !e.altKey) {
    if (chordDigitTargets(chordMap.x).length) {
      e.preventDefault();
      enterChord(chordMap.x, 'x');
      return;
    }
  }

  if (e.key === 't' && !e.ctrlKey && !e.metaKey && !e.altKey && (isTagsPage() || isTagDetailPage())) {
    e.preventDefault();
    enterChord(chordMap.t, 't');
    return;
  }

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

  // Indexed off the rendered nav so a profile-gated section cannot shift
  // the digits.
  if (isSettingsPage() && /^[0-9]$/.test(e.key)) {
    var nth = e.key === '0' ? 9 : parseInt(e.key, 10) - 1;
    var link = document.querySelectorAll('.settings-nav a')[nth];
    var sec = link && document.querySelector(link.getAttribute('href'));
    if (sec) { e.preventDefault(); sec.scrollIntoView({ block: 'start' }); return; }
  }

  if (e.key === 'f') {
    if (batchBarVisible()) {
      if (typeof openBatchFavoriteDialog === 'function') {
        e.preventDefault(); openBatchFavoriteDialog('selection'); return;
      }
    }
    if (clickFirstSelector(['.btn-fav'])) { e.preventDefault(); return; }
  }

  if (e.key === 'a' && !e.ctrlKey && !e.metaKey) {
    if (batchBarVisible()) { e.preventDefault(); openTagSelectedDialog(); return; }
    var tagInput = document.getElementById('tag-input');
    if (tagInput) { e.preventDefault(); tagInput.focus(); return; }
  }

  if (e.key === 'r') {
    if (batchBarVisible()) { e.preventDefault(); openStripSelectedDialog(); return; }
    if (isDetailPage()) { e.preventDefault(); enterTagFocusMode(); return; }
  }

  if (e.key === 'R') {
    var readBtn = document.querySelector('.btn-manga-action.btn-read');
    if (readBtn) { e.preventDefault(); window.location.href = readBtn.href; return; }
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

  if (e.key === 'B' && isDetailPage()) {
    var modeBtn = document.querySelector('.tag-mode-toggle');
    if (modeBtn) { e.preventDefault(); revealSidebarFor(modeBtn); modeBtn.click(); return; }
  }

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

  if (e.key === 'c' && batchBarVisible()) {
    if (typeof openBatchCollectionDialog === 'function') {
      e.preventDefault(); openBatchCollectionDialog('selection'); return;
    }
  }

  if (e.key === 'd' && batchBarVisible()) {
    if (typeof openBatchDownloadDialog === 'function') {
      e.preventDefault(); openBatchDownloadDialog('selection'); return;
    }
  }

  if (e.key === 'T' && batchBarVisible()) {
    if (clickFirstSelector(['#batch-transfer'])) { e.preventDefault(); return; }
  }

  // Not gated on a selection: invert and select-all-matching are used
  // with nothing picked yet.
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
    // A click, not .checked: the tags page wires selection in its own script.
    var focusedRow = document.querySelector('.tag-row.focused');
    if (focusedRow) {
      var rowBox = focusedRow.querySelector('.tag-select');
      if (rowBox) { e.preventDefault(); rowBox.click(); }
    }
    return;
  }

  if (e.key === 'Backspace' && (isDetailPage() || document.getElementById('pages-grid-page'))) {
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
      // Up and Down arrows keep their native scroll; j and k step
      // prev/next like h and l.
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

document.addEventListener('click', function(e) {
  var link = e.target.closest('.back-link');
  if (!link) return;
  if (detailRefBack()) e.preventDefault();
});

// history.back, not HX-Redirect: a redirect pushes a fresh entry and
// drops the ref chain behind it.
document.body.addEventListener('delete-go-back', function(e) {
  if (history.length > 1) { history.back(); return; }
  var fallback = e.detail && e.detail.fallback;
  if (fallback) window.location.href = fallback;
});

function closeDialogFromSaveEvent(e) {
  var id = e.detail && e.detail.dialog;
  if (!id) return;
  var dlg = document.getElementById(id);
  if (dlg && dlg.open) dlg.close();
}
document.body.addEventListener('tagger-saved', closeDialogFromSaveEvent);
document.body.addEventListener('token-saved', closeDialogFromSaveEvent);

// The modal also keeps a second click off the button while the diff loads.
function ptrContribOpen() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) load.showModal();
}

// A loading modal already closed means Escape cancelled the wait, so the
// answer is dropped.
function ptrContribOpened() {
  var load = document.getElementById('ptr-contrib-loading');
  if (load && !load.open) return;
  if (load) load.close();
  var dlg = document.getElementById('ptr-contrib-dialog');
  if (dlg) dlg.showModal();
  else htmx.trigger(document.body, 'ptr-contrib-closed');
}

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
  // Keyed on the label: the click it forwards to its checkbox carries no
  // modifier flags.
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

// Captured: a dialog's close event does not bubble.
document.addEventListener('close', function(e) {
  if (e.target.id === 'ptr-contrib-dialog') htmx.trigger(document.body, 'ptr-contrib-closed');
}, true);

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

// Enter would implicitly submit the dialog's config form; the input
// already searches on its search event.
document.body.addEventListener('keydown', function(e) {
  if (e.key !== 'Enter') return;
  var el = e.target;
  if (el && el.name === 'q' && el.closest && el.closest('.tc-mappings')) {
    e.preventDefault();
  }
});

document.body.addEventListener('click', function(e) {
  var btn = e.target.closest && e.target.closest('.tc-edit, .tc-reset, .tc-edit-cancel, .tc-filter, .tc-more-btn');
  if (!btn) return;
  var panel = btn.closest('.tc-mappings');
  if (!panel) return;
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

  var open = panel.querySelector('.tc-editor-row');
  if (open) open.remove();

  if (!btn.classList.contains('tc-edit')) return;

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
  // Left alone, Enter would submit the dialog's form and close the pop-in.
  editor.addEventListener('keydown', function(ev) {
    if (ev.key !== 'Enter') return;
    ev.preventDefault();
    apply.click();
  });
  row.after(editor);
  htmx.process(editor);
});

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
  // A class, not hidden: visibility keeps the link's width so the column
  // does not reflow.
  if (reset) reset.classList.toggle('reset-thresh-shown', !threshRowAtDefault(row));
}

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

function restoreGalleryFocusFromHash() {
  var m = window.location.hash.match(/^#img-(\d+)$/);
  if (!m) return;
  var card = document.querySelector('.thumb-card[data-id="' + m[1] + '"]');
  if (!card) return;
  var cards = Array.from(document.querySelectorAll('.thumb-card'));
  setFocused(cards, cards.indexOf(card));
}
document.addEventListener('DOMContentLoaded', restoreGalleryFocusFromHash);

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

document.addEventListener('input', function(e) {
  if (e.target.id !== 'tag-input') return;
  var tagsDiv = document.getElementById('image-tags');
  if (!tagsDiv) return;
  var err = tagsDiv.querySelector('.flash-err');
  if (err) err.remove();
});

// The live search leaves the sidebar's filter links on a stale q, and htmx
// fixes a boosted link's path at process time, so it is rewritten per request.
function sidebarFilterQ(url) {
  const input = document.querySelector('#sidebar-inner form input[name=q]');
  if (!input) return null;
  url.searchParams.set('q', input.value);
  return url.pathname + '?' + url.searchParams.toString();
}

function showPageNotice(text) {
  const el = document.getElementById('page-notice');
  if (!el) return null;
  el.textContent = text;
  delete el.dataset.session;
  el.hidden = false;
  return el;
}

// fetch follows the login redirect a write without a session gets, and
// reports the login page as a success.
function sessionLost(res) {
  return res.status === 401 || (res.redirected && new URL(res.url).pathname === '/login');
}

function showLoginNotice() {
  const el = showPageNotice('You are logged out: ');
  if (!el) return;
  const a = document.createElement('a');
  a.href = '/login';
  a.target = '_blank';
  a.textContent = '[log in]';
  el.append(a, ' in another tab, then retry here. This page keeps what you typed.');
  el.dataset.session = '1';
}

// Nothing animates; htmx would smooth-scroll a boosted swap to the top.
if (window.htmx) window.htmx.config.scrollBehavior = 'auto';

document.body.addEventListener('htmx:configRequest', function(e) {
  if (activeGallery()) e.detail.headers['X-Monbooru-Gallery'] = activeGallery();
});

document.body.addEventListener('htmx:configRequest', function(e) {
  const elt = e.detail.elt;
  if (!elt || !elt.matches || !elt.matches('#sidebar-inner a.filter-btn')) return;
  const path = sidebarFilterQ(new URL(e.detail.path, window.location.origin));
  if (path) e.detail.path = path;
});

// A Back restores markup, not values: the boxes a swap never re-renders
// would keep the first page's query.
document.body.addEventListener('htmx:historyRestore', function() {
  const params = new URLSearchParams(window.location.search);
  const form = document.getElementById('search-form');
  if (form && document.getElementById('gallery-grid')) {
    const input = document.getElementById('search-input');
    if (input) input.value = params.get('q') || '';
    const orderEl = form.querySelector('select[name="order"]');
    const order = params.get('order');
    if (orderEl && (order === 'asc' || order === 'desc')) orderEl.value = order;
    let seedEl = form.querySelector('input[name="seed"]');
    const seed = params.get('seed');
    if (seed && !seedEl) {
      seedEl = document.createElement('input');
      seedEl.type = 'hidden';
      seedEl.name = 'seed';
      form.appendChild(seedEl);
    }
    if (seed) seedEl.value = seed;
    else if (seedEl) seedEl.remove();
    readSelection();
    syncSelectionBoxes();
    updateBatchBar();
    initInboxUpload();
  }
  ['tags-q-input', 'collections-q-input'].forEach(function(id) {
    const el = document.getElementById(id);
    if (el) el.value = params.get('q') || '';
  });
});

// htmx does not swap error responses, but a job-conflict 409's body is
// the inline flash.
document.body.addEventListener('htmx:beforeSwap', function(e) {
  const xhr = e.detail.xhr;
  if (!xhr) return;
  if (xhr.status === 401) {
    showLoginNotice();
    return;
  }
  if (xhr.status === 403) {
    showPageNotice(xhr.responseText);
    return;
  }
  const notice = document.getElementById('page-notice');
  if (xhr.status < 400 && notice && notice.dataset.session) notice.hidden = true;
  if (xhr.status !== 409) return;
  if (xhr.getResponseHeader('X-Monbooru-Gallery')) {
    showPageNotice(xhr.responseText);
    return;
  }
  e.detail.shouldSwap = true;
});

document.addEventListener('input', function(e) {
  if (!e.target.matches || !e.target.matches('#sidebar-inner form input[name=q]')) return;
  document.querySelectorAll('#sidebar-inner a.filter-btn').forEach(function(a) {
    const href = sidebarFilterQ(new URL(a.getAttribute('href'), window.location.origin));
    if (href) a.setAttribute('href', href);
  });
});

// The button's term is category:name, but the query may hold the bare
// name, so both forms are removed.
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

// Blurred so the keydown router, which ignores keys while an input has
// focus, still sees the next shortcut.
document.addEventListener('change', function(e) {
  if (!e.target.classList.contains('thumb-checkbox')) return;
  pickCheckbox(e.target);
  writeSelection();
  updateBatchBar();
  e.target.blur();
});

function forEachClusterCheckbox(header, fn) {
  var node = header.nextElementSibling;
  while (node && !node.classList.contains('thumb-cluster-header')) {
    var cb = node.querySelector ? node.querySelector('.thumb-checkbox') : null;
    if (cb) fn(cb);
    node = node.nextElementSibling;
  }
}

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

// A cluster's batch may run past this page, so its ids come from a search
// of its own range.
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

var lastPickedCard = null;

// Ctrl/Cmd toggles like a plain click rather than opening a tab; Alt
// belongs to the band, which reads it as subtract.
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

// A drag only adds (Alt subtracts), so an accidental drag cannot wipe a
// selection.
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
  // Links are fair game, or the band could only start in the gaps between
  // cards.
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
  // Re-read on every move: the grid may have scrolled under the pointer.
  var origin = marquee.grid.getBoundingClientRect();
  marquee.x1 = e.clientX - origin.left;
  marquee.y1 = e.clientY - origin.top;
  if (Math.abs(marquee.x1 - marquee.x0) > 4 || Math.abs(marquee.y1 - marquee.y0) > 4) {
    marquee.moved = true;
  }
  // Nothing is painted before the threshold, so a click's pixel or two of
  // movement stays a click.
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
  // The release also fires a click on the card under it, which would undo
  // the band's last cell.
  if (moved) marqueeSwallowUntil = Date.now() + 100;
});

var marqueeSwallowUntil = 0;

document.addEventListener('click', function(e) {
  if (Date.now() >= marqueeSwallowUntil) return;
  if (!e.target.closest || !e.target.closest('.thumb-grid')) return;
  e.preventDefault();
  e.stopPropagation();
}, true);

// Null while the batch runs past this page and its ids have not been fetched.
function clusterAllIDs(header) {
  if (header.clusterAllIDs) return header.clusterAllIDs;
  if (!header.hasAttribute('data-cluster-whole')) return null;
  var ids = [];
  forEachClusterCheckbox(header, function(cb) { ids.push(cb.value); });
  return ids;
}

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

// Ids, not checkboxes: a selection spans pages. Pick order is the order a
// numbering job walks, so a re-pick keeps its place.
var pickedIDs = [];
// When set, the scope is the query, re-resolved on the server, not the ids.
var selectAllMatching = false;
var selectMode = false;

// Read this, not selectMode: a selection arms the grid without the toggle.
function selectionArmed() {
  return selectMode || selectAllMatching || pickedIDs.length > 0;
}

// The toggle shows armed while anything is picked, so switching it off
// clears the selection.
function toggleSelectMode() {
  if (selectionArmed()) {
    selectMode = false;
    clearSelection();
    return;
  }
  selectMode = true;
  updateBatchBar();
}

// Does not persist: the caller writes once when done, so a whole page is
// one write.
function pickCheckbox(cb) {
  var at = pickedIDs.indexOf(cb.value);
  if (cb.checked) {
    if (at === -1) pickedIDs.push(cb.value);
  } else if (at !== -1) {
    pickedIDs.splice(at, 1);
  }
  // A single toggle narrows the pick, which the whole-search scope cannot
  // describe.
  selectAllMatching = false;
}

// Every box is set: across a reload a browser restores form state by
// position, which after rows drop lands on the wrong ones. A whole-search
// scope takes the ids on screen so a toggle can narrow from them.
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
    // Shown while armed, .visible only with a scope: the keyboard router
    // reads .visible.
    bar.hidden = !armed;
    bar.classList.toggle('visible', active);
  }
  // Plugins post a list of ids, so a whole-search scope leaves them unlit.
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
    countEl.hidden = !active;
    countEl.textContent = selectAllMatching
      ? 'All ' + matching + ' matching selected'
      : pickedIDs.length + ' selected' +
        (pickedIDs.length > onPage ? ' · ' + onPage + ' on this page' : '');
  }
  // Offered only once the whole page is picked, as the step up from it.
  const escalate = document.getElementById('batch-select-all-matching');
  if (escalate) {
    escalate.textContent = 'Select all ' + matching + ' matching';
    escalate.hidden = selectAllMatching || total === 0 || onPage < total || matching <= total;
  }
  updateClusterButtons();
}

// Read off .result-count, which each htmx search swaps out of band; the
// batch bar is not re-rendered.
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

function selectAll() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = true;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// Flips the page only: picks on other pages are not on screen to reconsider.
function invertSelection() {
  document.querySelectorAll('.thumb-checkbox').forEach(function(cb) {
    cb.checked = !cb.checked;
    pickCheckbox(cb);
  });
  writeSelection();
  updateBatchBar();
}

// Minutes, not seconds: a batch job's reload can come long after the click.
var selectionStashMs = 300000;

// Part of every stash key: ids are per gallery, and after a switch the
// same URL is another library.
function activeGallery() {
  return (document.body && document.body.dataset.gallery) || '';
}

// From the URL, not the box, which may hold an unsubmitted query. Sort
// and page stay out so paging keeps the pick.
function selectionKey() {
  return activeGallery() + location.pathname + '?q=' + (new URLSearchParams(location.search).get('q') || '');
}

function writeSelection() {
  try {
    sessionStorage.setItem('monbooru_selection', JSON.stringify(
      {ids: pickedIDs, all: selectAllMatching, url: selectionKey(), t: Date.now()}));
  } catch (e) {}
}

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

// For the /tags rows only; the gallery persists its own selection on
// every change.
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
  // Every box is set: across a reload a browser restores form state by
  // position.
  var tagIDs = takeTagStash();
  document.querySelectorAll('.tag-select').forEach(function(cb) {
    cb.checked = tagIDs.indexOf(cb.value) !== -1;
  });
  updateBatchBar();
  if (typeof updateTagBatchBar === 'function') updateTagBatchBar();
});

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

// Implied tags are skipped: they cannot be removed apart from the tag
// that implies them.
function tagFocusRows() {
  return sidebarTagRows().filter(function(li) { return !li.classList.contains('tag-entry-implied'); });
}

function focusedTagRow() {
  return document.querySelector('#tag-groups .tag-entry.focused');
}

function enterTagFocusMode() {
  var items = tagFocusRows();
  if (!items.length) return;
  if (focusedTagRow()) return;
  revealSidebarFor(items[0]);
  items[0].classList.add('focused');
  items[0].scrollIntoView({block: 'nearest'});
  // On body so the mode survives an htmx swap that drops the row's .focused.
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

function batchDeleteSelected() {
  if (typeof openBatchDeleteDialog === 'function') openBatchDeleteDialog('selection');
}

function openSaveSearchDialog() {
  var dlg = document.getElementById('save-search-dialog');
  if (!dlg) return false;
  var si = document.getElementById('search-input');
  var sq = document.getElementById('save-search-query');
  var sp = document.getElementById('save-search-preview');
  if (si && sq) sq.value = si.value;
  if (si && sp) sp.textContent = si.value || '(empty)';
  var url = new URL(window.location.href);
  var ss = document.getElementById('save-search-sort');
  var so = document.getElementById('save-search-order');
  var se = document.getElementById('save-search-seed');
  var savedSort = url.searchParams.get('sort') || '';
  if (ss) ss.value = savedSort;
  if (so) so.value = url.searchParams.get('order') || '';
  if (se) se.value = savedSort === 'random' ? (url.searchParams.get('seed') || '') : '';
  dlg.showModal();
  return true;
}

function refreshJobStatus() {
  var el = document.getElementById('job-status');
  if (!el || !window.htmx) return;
  el.setAttribute('hx-trigger', 'every 2s');
  window.htmx.process(el);
  window.htmx.ajax('GET', '/internal/job/status', {target: '#job-status', swap: 'outerHTML'});
}

// alt is an optional {label, run} third choice beside OK and Cancel.
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
  if (danger) cancelBtn.focus(); else okBtn.focus();
}

document.body.addEventListener('htmx:confirm', function(e) {
  if (!e.detail || !e.detail.question) return;
  e.preventDefault();
  var elt = e.detail.elt;
  var ds = elt && elt.dataset ? elt.dataset : {};
  var alt = null;
  if (ds.confirmAlt && ds.confirmAltUrl) {
    // The form's hidden fields carry the CSRF token and the origin's identity.
    alt = {label: ds.confirmAlt, run: function() {
      var values = {};
      var form = elt.closest('form');
      if (form) {
        form.querySelectorAll('input[type="hidden"]').forEach(function(i) { values[i.name] = i.value; });
      }
      if (ds.confirmAltValue) {
        values[ds.confirmAltValue.split('=')[0]] = ds.confirmAltValue.split('=')[1];
      }
      // No source element, or htmx inherits the hx-confirm and asks the
      // same question again.
      htmx.ajax('POST', ds.confirmAltUrl, {values: values});
    }};
  }
  showConfirm(e.detail.question, function() { e.detail.issueRequest(true); }, ds.confirmDanger, ds.confirmOk, alt);
});

// htmx:confirm only fires for htmx requests, so hx-confirm on a plain form is
// handled here; form.submit() fires no submit event, so it cannot loop.
var htmxVerbs = ['hx-get', 'hx-post', 'hx-put', 'hx-patch', 'hx-delete'];
document.body.addEventListener('submit', function(e) {
  var form = e.target;
  if (!form.hasAttribute || !form.hasAttribute('hx-confirm')) return;
  if (htmxVerbs.some(function(v) { return form.hasAttribute(v); })) return;
  e.preventDefault();
  var ds = form.dataset;
  showConfirm(form.getAttribute('hx-confirm'), function() { form.submit(); }, ds.confirmDanger, ds.confirmOk);
});

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
  // data-max, not max: constraint validation would block the submit
  // before the clamp runs.
  input.dataset.max = total;
  if (totalSpan) totalSpan.textContent = total;
  dlg.showModal();
  setTimeout(function() { input.focus(); input.select(); }, 0);
});

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

// Only the form's own 204 closes: htmx events bubble, and an empty
// suggest answer from an input inside is a 204 too.
function onExternalEditResponse(event, dialogID) {
  if (!event || !event.detail || !event.detail.xhr) return;
  if (!event.detail.elt || event.detail.elt.tagName !== 'FORM') return;
  if (event.detail.xhr.status !== 204) return;
  var dlg = document.getElementById(dialogID);
  if (dlg) dlg.close();
}

document.addEventListener('click', function(e) {
  var btn = e.target.closest && e.target.closest('.markup-bar [data-wrap]');
  if (!btn) return;
  var ta = document.getElementById(btn.closest('.markup-bar').dataset.target);
  if (!ta) return;
  var kind = btn.dataset.wrap, start = ta.selectionStart, end = ta.selectionEnd;
  var sel = ta.value.slice(start, end), open, close, caret;
  if (kind === 'image') {
    open = '[image:'; close = ']'; sel = ''; caret = start + open.length;
  } else if (kind === 'tag' || kind === 'url') {
    open = '[' + kind + '=]'; close = '[/' + kind + ']'; caret = start + open.length - 1;
  } else {
    open = '[' + kind + ']'; close = '[/' + kind + ']'; caret = start + open.length + sel.length;
  }
  ta.value = ta.value.slice(0, start) + open + sel + close + ta.value.slice(end);
  ta.focus();
  ta.setSelectionRange(caret, caret);
});

function toggleAnnotations(btn) {
  var media = btn.closest('.detail-media');
  if (!media) return;
  var hidden = media.classList.toggle('annotations-hidden');
  btn.textContent = hidden ? '[show annotations]' : '[hide annotations]';
}

var actionFlashSlots = ['gallery-flash', 'detail-flash', 'tag-flash', 'cat-flash', 'collection-flash', 'flash-tagger'];

function findActionFlashSlot() {
  for (var i = 0; i < actionFlashSlots.length; i++) {
    var el = document.getElementById(actionFlashSlots[i]);
    if (el) return el;
  }
  return null;
}

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

// textContent, so a name in the text cannot inject markup.
function setFlashText(slot, kind, text) {
  if (!slot) return;
  var d = document.createElement('div');
  d.className = 'flash flash-' + kind;
  d.textContent = text;
  slot.textContent = '';
  slot.appendChild(d);
}

function stashActionFlash(html, kind) {
  if (!html) return;
  try {
    sessionStorage.setItem('monbooru_action_flash',
      JSON.stringify({h: html, k: kind || 'ok', t: Date.now()}));
  } catch (e) {}
}

// Covers an action's own reload, not a later visit after an action that
// never navigated.
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

// Shown and stashed both: the same response may or may not end in a
// redirect or refresh.
document.body.addEventListener('monbooru:flash', function(e) {
  if (!e || !e.detail) return;
  var text = e.detail.text || '';
  var kind = e.detail.kind || 'ok';
  showActionFlash(text, kind);
  stashActionFlash(text, kind);
});

function onRelationEditResponse(event) {
  var src = document.getElementById('relation-edit-error');
  if (!src) return;
  var overwrite = document.getElementById('relation-edit-overwrite-btn');
  // Added and refused both come back 200, so the slot's body tells them
  // apart; a failed request swaps nothing and must not read the stale slot.
  var answered = event && event.detail && event.detail.successful &&
    src.querySelector('.flash-ok, .flash-err');
  if (!answered) {
    if (overwrite) overwrite.hidden = true;
    src.innerHTML = '<div class="flash flash-err">The relation was not saved.</div>';
    return;
  }
  var success = src.querySelector('.flash-ok');
  if (!success) {
    // Keyed on the server's conflict wording: rewording those errors
    // hides the button.
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

// The URL is copied, not rebuilt: it carries the back context.
function submitPageJump(inputId) {
  var input = document.getElementById(inputId || 'page-jump-input');
  if (!input) return;
  var p = parseInt(input.value, 10);
  if (!p || p < 1) p = 1;
  var max = parseInt(input.dataset.max, 10);
  if (max && p > max) p = max;
  var u = new URL(window.location.href);
  u.searchParams.set('page', String(p));
  window.location.href = u.toString();
}

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

// Page 1: the page number in the URL indexes the listing at the old size.
function setPageSize(size) {
  postForm('/internal/view-prefs', {page_size: size}, {
    onOK: function() {
      var u = new URL(location.href);
      u.searchParams.set('page', '1');
      location.href = u.toString();
    }
  });
}

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

function setSidebarCollapsed(collapsed) {
  var layout = document.getElementById('main-layout');
  if (!layout) return;
  layout.classList.toggle('sidebar-collapsed', collapsed);
  document.cookie = 'monbooru_sidebar=' + (collapsed ? 'collapsed' : '') +
    '; path=/; max-age=' + (collapsed ? 31536000 : 0);
  // A collapsed layout renders its lazy panels without a load trigger;
  // this event fetches them.
  if (!collapsed) htmx.trigger(document.body, 'sidebar-shown');
}

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

// The cookie holds the sections toggled off their default, open or collapsed.
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
  // onclick, not addEventListener: this runs again on every htmx settle.
  toggle.onclick = clickToggle;
  var header = toggle.closest('.sidebar-section-header');
  var title = header && header.querySelector('.sidebar-section-title');
  if (title) title.onclick = clickToggle;
}

// Kept in JS, not the DOM: OOB swaps replace #sidebar-inner on every
// refresh, even with no URL change. Without it the URL-driven force-open
// undoes a collapse the operator just made.
var _lastInitedQuery = null;

function initFolderTree() {
  // DOMContentLoaded beats the lazy sidebar; an empty pass must not latch
  // the URL.
  if (!document.querySelector('.folder-toggle-btn')) return;

  var expanded = getFolderCookie();
  var currentQuery = window.location.search;
  var urlChanged = currentQuery !== _lastInitedQuery;
  _lastInitedQuery = currentQuery;

  var currentFolder = '';
  var urlParams = new URLSearchParams(currentQuery);
  var q = urlParams.get('q') || '';
  var folderMatch = q.match(/(?:^|\s)folder(?:only)?:(?:"([^"]+)"|([^\s]*))/);
  if (folderMatch) {
    currentFolder = folderMatch[1] || folderMatch[2];
  }

  initSectionToggle('tags-toggle', 'tag-groups', '__tags__', false, true);
  initSectionToggle('folder-tree-toggle', 'folder-tree-list', '__tree__', false);
  var sourceLabelMatch = q.match(/(?:^|\s)source:(?:"([^"]+)"|([^\s]+))/);
  var sourceLabelOpen = urlChanged && !!sourceLabelMatch;
  initSectionToggle('source-labels-toggle', 'source-labels-list', '__sources__', sourceLabelOpen);
  var treeToggle = document.getElementById('folder-tree-toggle');
  var treeList = document.getElementById('folder-tree-list');

  // onclick, not addEventListener: this runs again on every htmx settle.
  document.querySelectorAll('.folder-toggle-btn[data-path]').forEach(function(btn) {
    var path = btn.dataset.path;
    var targetId = btn.dataset.target || ('fc-' + path);
    var list = document.getElementById(targetId);
    if (!list) return;

    var urlDriven = currentFolder && (currentFolder === path || currentFolder.startsWith(path + '/'));
    var shouldExpand = expanded.has(path) || (urlChanged && urlDriven);

    if (shouldExpand) {
      list.style.display = '';
      btn.textContent = '▼';
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
document.addEventListener('htmx:afterSettle', initFolderTree);

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
  var form = input.closest('form') || container.querySelector('form');
  if (form && form.id === 'add-tag-form') {
    form.requestSubmit();
  }
}

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

// The chip cancels its mousedown so it never takes focus: activeElement
// must stay the field with the caret.
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

// A search scope has no picked ids; the rendered thumbnails head the same
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

// A click, not a keystroke: a {md5} template hashes files that have no
// digest yet.
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
  // A row ending in a colon or an operator waits for its value, so no
  // trailing space.
  var keepCursor = /[:<>=]$|\.\.$/.test(tagName);
  si.value = words.join('') + (keepCursor ? '' : ' ');
  var dd = document.getElementById('search-suggest');
  if (dd) dd.innerHTML = '';
  si.focus();
  // The synthetic input event makes htmx's input trigger fetch the next
  // level of hints.
  if (keepCursor) {
    si.dispatchEvent(new Event('input', { bubbles: true }));
  }
}

// The execCommand fallback is for plain-HTTP LAN installs, which have no
// secure context and so no navigator.clipboard.
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

// Browse only opens the dialog; the button's own hx-get fetches the listing.
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
var _jobAutoClearFinishedAt = '';
var _lastReloadedFinishedAt = '';
function refreshGalleryGrid() {
  if (!document.getElementById('gallery-grid') || !window.htmx) return;
  var u = new URL(window.location.href);
  window.htmx.ajax('GET', u.pathname + u.search, {target: '#gallery-grid', swap: 'innerHTML'});
}

// The first settle reports whatever job last finished, often before this
// page rendered, so its reload is skipped.
var _firstJobStatusSettle = true;
var _lastJobProcessed = -1;
var _lastWatcherNotices = -1;
// Set when an action starts a job, whose completion then reloads the
// page: the grid swap after a job intermittently fails to settle.
var _pendingGalleryReload = false;

function armGalleryReload() {
  _pendingGalleryReload = true;
  if (typeof refreshJobStatus === 'function') refreshJobStatus();
}

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail.elt;

  // By afterSettle htmx has pushed the new URL, so readSelection keys on
  // the new listing.
  if (el && el.id === 'gallery-grid') {
    readSelection();
    syncSelectionBoxes();
    updateBatchBar();
    restoreGalleryFocusFromHash();
    initInboxUpload();
    return;
  }

  // The swap drops .focused; put it back one row above the removed tag,
  // from the index Enter stashed.
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

  var firstSettle = _firstJobStatusSettle;
  _firstJobStatusSettle = false;

  var isDone = !!el.querySelector('.job-done');
  var isErr  = !!el.querySelector('.job-error');
  // On #job-status itself, where querySelector would never find it.
  var isRunning = el.classList.contains('job-running');
  var finishedAt = el.dataset.finishedAt || '';

  // The listed types advance Processed as they change the grid; watcher
  // notices cover what the watcher ingests or removes while any job runs.
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

  if (isIdle) {
    _jobAutoClearFinishedAt = '';
    if (_jobAutoClearTimer) { clearTimeout(_jobAutoClearTimer); _jobAutoClearTimer = null; }
    return;
  }

  // Re-armed whenever FinishedAt advances: a dismiss mid-batch would
  // strip hx-trigger and stop the widget polling.
  if ((isDone || isErr) && finishedAt && finishedAt !== _jobAutoClearFinishedAt) {
    _jobAutoClearFinishedAt = finishedAt;
    if (_jobAutoClearTimer) clearTimeout(_jobAutoClearTimer);
    _jobAutoClearTimer = setTimeout(function() {
      _jobAutoClearFinishedAt = '';
      dismissJobStatus();
    }, 30000);
  }

  if (isDone && _pendingGalleryReload) {
    if (finishedAt) _lastReloadedFinishedAt = finishedAt;
    if (document.getElementById('gallery-grid') || document.getElementById('tags-page') || document.getElementById('tag-detail-page') || document.getElementById('collections-page')) {
      var pendingDone = el.querySelector('.job-done');
      if (pendingDone) stashActionFlash(escapeHTML(pendingDone.textContent || ''), 'ok');
      // Left armed: reload() does not stop this script, and a navigation
      // is still pending.
      window.location.reload();
      return;
    }
    _pendingGalleryReload = false;
  }

  if (isDone && finishedAt && finishedAt !== _lastReloadedFinishedAt) {
    _lastReloadedFinishedAt = finishedAt;
    if (firstSettle) return;

    // The relations hub computes its counters at render time, so only a
    // reload updates them.
    if (document.getElementById('relations-page') && (jobType === 'relations' || jobType === 'phash')) {
      window.location.reload();
      return;
    }

    // A check changes nothing, so the grid is left alone.
    var grid = jobType === 'check' ? null : document.getElementById('gallery-grid');
    if (grid) {
      var doneEl = el.querySelector('.job-done');
      if (doneEl) showActionFlash(escapeHTML(doneEl.textContent || ''), 'ok');
      refreshGalleryGrid();
    }

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

function scopeCount(scope, countEl, nounEl) {
  var n = scope === 'selection' ? pickedIDs.length : matchingTotal();
  if (countEl) countEl.textContent = n;
  if (nounEl) {
    var suffix = n === 1 ? 'image' : 'images';
    nounEl.textContent = scope === 'selection' ? 'selected ' + suffix : suffix + ' in current search';
  }
  return n;
}

// From the URL, not the form, which may hold an unsubmitted query.
function searchScopeParts() {
  var params = new URLSearchParams(location.search);
  var sortEl = document.getElementById('search-sort');
  var orderEl = document.querySelector('#search-form select[name="order"]');
  var parts = ['q=' + encodeURIComponent(params.get('q') || ''),
               'sort=' + encodeURIComponent(params.get('sort') || (sortEl ? sortEl.value : 'newest')),
               'order=' + encodeURIComponent(params.get('order') || (orderEl ? orderEl.value : 'desc'))];
  // Without the seed, a job that numbers by position walks a different
  // shuffle than the one on screen.
  var seed = params.get('seed');
  if (seed) parts.push('seed=' + encodeURIComponent(seed));
  return parts;
}

function selectedImageIds() {
  return pickedIDs.slice();
}

// One comma-joined parameter: a selection built across pages can pass
// net/url's 10000-parameter cap, and the parser rejects the whole request.
function selectionScopeIds() {
  var ids = selectedImageIds();
  if (ids.length === 0) return null;
  return ['ids=' + encodeURIComponent(ids.join(','))];
}

function batchScopeParams(scope, flash) {
  if (scope === 'search') return searchScopeParts();
  var ids = selectionScopeIds();
  if (!ids) {
    if (flash) flash.innerHTML = '<div class="flash flash-err">No images selected.</div>';
    return null;
  }
  return ids;
}

function relayPlugin(btn) {
  var pinned = btn.closest('[data-image-id]');
  var ids = pinned ? [pinned.dataset.imageId] : selectedImageIds();
  if (!ids.length || !window.htmx) return;
  window.htmx.ajax('POST', '/internal/plugin/relay', {
    swap: 'none',
    values: {_csrf: getCSRFToken(), plugin: btn.dataset.plugin, button: btn.dataset.button, ids: ids.join(',')}
  });
}

// A plugin signals it is done by sending the frame to its {back_url}, off
// /plugins/.
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

// opts: endpoint, scope, params (encoded "k=v" parts, without _csrf or
// scope), dialogId, flashId, failMsg, consumesScope (clears the
// selection: the rows are gone afterwards).
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

// opts: countId, requireMatches (refuse a search scope matching nothing),
// radio (re-checked as the default), clearIds (emptied on open),
// beforeShow, focusId.
function openBatchDialog(prefix, scope, opts) {
  opts = opts || {};
  // An escalated bar still opens dialogs with the selection scope;
  // flipping here lets each post the search.
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

function clearSlots() {
  for (var i = 0; i < arguments.length; i++) {
    var el = document.getElementById(arguments[i]);
    if (el) el.innerHTML = '';
  }
}

// A suggest still in its debounce at submit lands after the answer and paints
// over it; these inputs keep focus, so the focus check alone misses it.
function disarmSuggest(inputId) {
  var input = document.getElementById(inputId);
  if (input) input.dataset.suggestStale = '1';
}

function suggestDisarmed(input) {
  if (!input.dataset.suggestStale) return false;
  delete input.dataset.suggestStale;
  return true;
}

function applyPTRSpelling(btn) {
  var input = document.getElementById('ptr-lookup-as');
  var form = document.getElementById('ptr-lookup-form');
  if (!input || !form) return;
  input.value = btn.dataset.spelling;
  form.requestSubmit();
}

function openHxRewriteDialog(formId, attr, url, dialogId) {
  var form = document.getElementById(formId);
  form.setAttribute(attr, url);
  if (window.htmx) window.htmx.process(form);
  document.getElementById(dialogId).showModal();
}

function confirmBatchSimple(prefix, endpoint, extraParams, failMsg) {
  var scope = document.getElementById(prefix + '-scope').value;
  var scoped = batchScopeParams(scope, document.getElementById(prefix + '-flash'));
  if (!scoped) return;
  var params = (extraParams || []).concat(scoped);
  runBatchOp({endpoint: endpoint, scope: scope, params: params,
              dialogId: prefix + '-dialog', flashId: prefix + '-flash', failMsg: failMsg});
}

// opts: method (POST), hx (sends HX-Request), okStatus (the one success
// status), dialogId (closed on OK), onOK(res), flashId (gets the error
// body), failMsg, onErr(text) (instead of the flash), catchMsg (null: none).
function postForm(url, params, opts) {
  opts = opts || {};
  var csrf = getCSRFToken();
  var body = params instanceof URLSearchParams ? params : new URLSearchParams(params || {});
  body.append('_csrf', csrf);
  var headers = {'Content-Type': 'application/x-www-form-urlencoded', 'X-CSRF-Token': csrf};
  if (opts.hx) headers['HX-Request'] = 'true';
  if (activeGallery()) headers['X-Monbooru-Gallery'] = activeGallery();
  var flash = opts.flashId ? document.getElementById(opts.flashId) : null;
  var p = fetch(url, {
    method: opts.method || 'POST',
    headers: headers,
    body: body.toString()
  }).then(function(res) {
    if (sessionLost(res)) {
      showLoginNotice();
      return;
    }
    if (res.status === 403 || (res.status === 409 && res.headers.get('X-Monbooru-Gallery'))) {
      res.text().then(showPageNotice);
      return;
    }
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

function cancelJobStatus() {
  postForm('/internal/job/cancel');
}

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

// blurOnSubmit: after Enter the arrow keys drive the gallery, not the
// input. clearOnEmpty: drop a swap landing on an input emptied by an add,
// which keeps focus; label inputs list on an empty prefix on purpose.
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

// Resolved per event and delegated to document: some inputs live in
// re-rendered fragments, where listeners on the first nodes would be
// orphaned.
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

// A late response can land after a submit or a focus move, so it is dropped
// unless the input still has focus. suggest-fresh holds off :hover until the
// mouse moves, or the item under a still cursor looks picked.
document.addEventListener('htmx:afterSwap', function(e) {
  var pair = suggestPairs[e.target.id];
  if (!pair) return;
  var input = document.getElementById(pair.input);
  if (document.activeElement !== input || (pair.clearOnEmpty && input.value === '')) { e.target.innerHTML = ''; return; }
  e.target.classList.add('suggest-fresh');
  e.target.addEventListener('mousemove', clearSuggestFresh, {once: true});
});

// Suggest endpoints answer 204 on no match, and htmx swaps nothing on a
// 204, which would leave stale matches showing.
document.addEventListener('htmx:afterRequest', function(e) {
  if (!e.detail || !e.detail.elt) return;
  var ddId = suggestDropdownByInput[e.detail.elt.id];
  if (!ddId) return;
  if (e.detail.xhr && e.detail.xhr.status === 204) {
    var dd = document.getElementById(ddId);
    if (dd) dd.innerHTML = '';
  }
});

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

function clearSuggestFresh(e) {
  e.currentTarget.classList.remove('suggest-fresh');
}


var _initialTagIDs = null;
var _addedTagOrder = [];

function captureInitialTags() {
  if (_initialTagIDs !== null) return;
  _initialTagIDs = new Set();
  sidebarTagRows().forEach(function(li) { _initialTagIDs.add(li.dataset.tagId); });
}

// Only rows with the user's attribution are this session's adds; a
// tagger's or a source's join the baseline.
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

// Its x clicks the sidebar row's own button, which carries the
// server-rendered htmx wiring.
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

document.body.addEventListener('htmx:afterSettle', function(e) {
  var el = e.detail ? e.detail.elt : null;
  if (!el || (el.id !== 'image-tags' && el.id !== 'sidebar-inner')) return;
  separateNewTags();
  if (document.body.classList.contains('tag-focus') && !focusedTagRow()) {
    var first = tagFocusRows()[0];
    if (first) {
      first.classList.add('focused');
      first.scrollIntoView({block: 'nearest'});
    } else {
      document.body.classList.remove('tag-focus');
    }
  }
});

document.addEventListener('DOMContentLoaded', function() {
  if (document.getElementById('image-tags')) captureInitialTags();
});


// The topbar wraps to several rows at narrow widths, so the drawer's top
// comes from its measured height.
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

document.addEventListener('DOMContentLoaded', function() {
  var counter = document.getElementById('reader-counter');
  if (counter && document.getElementById('reader-jump-dialog')) {
    counter.addEventListener('click', function(e) {
      e.preventDefault();
      openReaderJumpDialog();
    });
  }
});

document.addEventListener('DOMContentLoaded', function() {
  if (!document.getElementById('pages-grid-page')) return;
  var hash = window.location.hash;
  if (!hash || hash.indexOf('#page-') !== 0) return;
  var target = document.querySelector(hash + '.manga-page-cell');
  if (!target) return;
  var cells = Array.from(document.querySelectorAll('.manga-page-cell'));
  setFocused(cells, cells.indexOf(target), 'center');
});

function initInboxUpload() {
  var dz = document.getElementById('inbox-upload-drop');
  var input = document.getElementById('inbox-upload-file-input');
  var list = document.getElementById('inbox-upload-file-list');
  var pickBtn = document.getElementById('inbox-upload-pick-btn');
  var form = document.getElementById('inbox-upload-form');
  var submitBtn = document.getElementById('inbox-upload-submit-btn');
  var resetBtn = document.getElementById('inbox-upload-reset-btn');
  var result = document.getElementById('inbox-upload-result');
  // A property: a Back's snapshot would keep a data- flag, not the listeners.
  if (!dz || !input || !list || !form || dz.inboxWired) return;
  dz.inboxWired = true;

  // The file picker replaces input.files on every change, so picks
  // accumulate here and are copied back.
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
    if (input.files && input.files.length) appendFiles(input.files);
  });
  if (pickBtn) {
    pickBtn.addEventListener('click', function(e) { e.preventDefault(); input.click(); });
  }
  if (resetBtn) {
    resetBtn.addEventListener('click', function(e) { e.preventDefault(); clearPending(); });
  }
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

  // htmx:xhr:progress fires for the response download too; the latch
  // keeps its events from rewinding the counter.
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
    // The result slot sits inside #gallery-grid, which the refresh below
    // replaces.
    if (result && result.innerHTML.trim() !== '') {
      showActionFlash(result.innerHTML, /flash-err/.test(result.innerHTML) ? 'err' : 'ok');
    }
    clearPending();
    refreshGalleryGrid();
  });
}

document.addEventListener('DOMContentLoaded', initInboxUpload);

function mergeCategoryCollision(tagID, catID) {
  var body = new URLSearchParams();
  body.set('category_id', String(catID));
  body.set('merge', '1');
  fetch('/tags/' + tagID + '/category', {
    method: 'PATCH',
    headers: {'X-CSRF-Token': getCSRFToken(), 'X-Monbooru-Gallery': activeGallery(), 'Content-Type': 'application/x-www-form-urlencoded'},
    body: body
  }).then(function(r) {
    if (sessionLost(r)) {
      showLoginNotice();
      return;
    }
    if (r.ok) {
      stashActionFlash('Merged into the existing tag.', 'ok');
      window.location.reload();
    } else {
      // The body is unescaped plain text, and tag names may hold < and >.
      r.text().then(function(t) {
        setFlashText(document.getElementById('tag-flash'), 'err', t || 'Merge failed.');
      });
    }
  });
}

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
    btn.disabled = true;
    var el = document.getElementById('tag-flash');
    setFlashText(el, 'warn', (isRating ? 'Stripping' : 'Deleting') + ' "' + name + '"…');
    fetch('/tags/' + tagID, {
      method: 'DELETE',
      headers: {'X-CSRF-Token': getCSRFToken(), 'X-Monbooru-Gallery': activeGallery()}
    }).then(function(r) {
      if (sessionLost(r)) {
        btn.disabled = false;
        el.textContent = '';
        showLoginNotice();
        return;
      }
      if (r.ok) {
        // A raw fetch does not process HX-Trigger, so the flash is
        // stashed by hand.
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
