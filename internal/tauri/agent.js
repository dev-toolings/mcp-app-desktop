// mcp-app-desktop page agent for the Tauri bridge. The server installs it in
// a webview the first time it drives it (and again after every navigation):
// it reads the page as an accessibility-like tree with refs, resolves the
// server's targets, checks actionability and dispatches DOM events. It never
// uses eval or new Function, so a strict CSP (no 'unsafe-eval') is fine.
//
// Every entry point returns a plain object; the server's wrapper turns it
// into a JSON string, the only kind of value the bridge may hand back.
(function () {
  'use strict';
  var VERSION = '__AGENT_VERSION__';
  if (window.__MCP_AD_AGENT__ && window.__MCP_AD_AGENT__.v === VERSION) return;

  var A = {
    v: VERSION,
    refs: new Map(), // "@N" → Element (last snapshot)
    meta: {}, // "@N" → {role, name, nth}
    results: {}, // async eval results by id
    nextId: 0,
    lastMutation: Date.now(),
    dialogs: { accept: true, text: '' },
  };

  // ---------------------------------------------------------------- roles

  var INTERACTIVE = toSet('button link textbox checkbox radio combobox menuitem tab switch slider spinbutton option searchbox listbox menuitemcheckbox menuitemradio treeitem');
  var SKELETON = toSet('heading button link textbox checkbox radio combobox menuitem menuitemcheckbox menuitemradio treeitem tab switch slider spinbutton option searchbox listbox menu menubar dialog alertdialog navigation form search banner main complementary contentinfo');
  var CONTENT = toSet('StaticText text paragraph listitem img image table rowheader columnheader');
  // Roles whose name is their content: their text is not repeated below them.
  var NAME_FROM_CONTENT = toSet('button link heading option tab menuitem menuitemcheckbox menuitemradio treeitem cell columnheader rowheader switch checkbox radio');
  var VALID_ROLES = toSet('alert alertdialog application article banner blockquote button caption cell checkbox code columnheader combobox complementary contentinfo definition deletion dialog directory document emphasis feed figure form generic grid gridcell group heading img image insertion link list listbox listitem log main marquee math menu menubar menuitem menuitemcheckbox menuitemradio meter navigation none note option paragraph presentation progressbar radio radiogroup region row rowgroup rowheader scrollbar search searchbox separator slider spinbutton status strong subscript superscript switch tab table tablist tabpanel term textbox time timer toolbar tooltip tree treegrid treeitem');

  function toSet(s) {
    var o = {};
    s.split(' ').forEach(function (k) {
      o[k] = true;
    });
    return o;
  }

  function lower(s) {
    return (s || '').toLowerCase();
  }

  function collapse(s) {
    return String(s || '').replace(/\s+/g, ' ').trim();
  }

  function roleOf(el) {
    var explicit = el.getAttribute && el.getAttribute('role');
    if (explicit) {
      var parts = explicit.trim().split(/\s+/);
      for (var i = 0; i < parts.length; i++) {
        var r = lower(parts[i]);
        if (VALID_ROLES[r]) return r === 'presentation' ? 'none' : r === 'image' ? 'img' : r;
      }
    }
    var tag = lower(el.tagName);
    switch (tag) {
      case 'a':
      case 'area':
        return el.hasAttribute('href') ? 'link' : 'generic';
      case 'button':
        return 'button';
      case 'summary':
        return 'button';
      case 'input': {
        var t = lower(el.getAttribute('type') || 'text');
        if (t === 'hidden') return 'none';
        if (t === 'button' || t === 'submit' || t === 'reset' || t === 'image') return 'button';
        if (t === 'checkbox') return el.getAttribute('switch') !== null ? 'switch' : 'checkbox';
        if (t === 'radio') return 'radio';
        if (t === 'range') return 'slider';
        if (t === 'number') return 'spinbutton';
        if (t === 'search') return el.hasAttribute('list') ? 'combobox' : 'searchbox';
        if (t === 'file' || t === 'color' || t === 'date' || t === 'datetime-local' || t === 'month' || t === 'time' || t === 'week') return 'button';
        return el.hasAttribute('list') ? 'combobox' : 'textbox';
      }
      case 'textarea':
        return 'textbox';
      case 'select':
        return el.multiple || el.size > 1 ? 'listbox' : 'combobox';
      case 'option':
        return 'option';
      case 'h1':
      case 'h2':
      case 'h3':
      case 'h4':
      case 'h5':
      case 'h6':
        return 'heading';
      case 'img':
        return el.getAttribute('alt') === '' ? 'none' : 'img';
      case 'svg':
        return el.getAttribute('aria-label') || el.querySelector('title') ? 'img' : 'none';
      case 'nav':
        return 'navigation';
      case 'main':
        return 'main';
      case 'header':
        return el.closest('article,aside,main,nav,section') ? 'generic' : 'banner';
      case 'footer':
        return el.closest('article,aside,main,nav,section') ? 'generic' : 'contentinfo';
      case 'aside':
        return 'complementary';
      case 'form':
        return 'form';
      case 'search':
        return 'search';
      case 'section':
        return el.hasAttribute('aria-label') || el.hasAttribute('aria-labelledby') ? 'region' : 'generic';
      case 'dialog':
        return 'dialog';
      case 'ul':
      case 'ol':
      case 'menu':
        return 'list';
      case 'li':
        return 'listitem';
      case 'table':
        return 'table';
      case 'tr':
        return 'row';
      case 'th':
        return el.getAttribute('scope') === 'row' ? 'rowheader' : 'columnheader';
      case 'td':
        return 'cell';
      case 'p':
        return 'paragraph';
      case 'fieldset':
      case 'details':
        return 'group';
      case 'progress':
        return 'progressbar';
      case 'meter':
        return 'meter';
      case 'hr':
        return 'separator';
      case 'article':
        return 'article';
      case 'label':
      case 'span':
      case 'div':
        break;
    }
    if (el.isContentEditable && (!el.parentElement || !el.parentElement.isContentEditable)) return 'textbox';
    return 'generic';
  }

  function headingLevel(el) {
    var m = /^h([1-6])$/i.exec(el.tagName);
    if (m) return +m[1];
    var a = parseInt(el.getAttribute('aria-level'), 10);
    return a > 0 ? a : 2;
  }

  // ---------------------------------------------------------------- names

  function textOf(node, depth) {
    if (depth > 20) return '';
    if (node.nodeType === 3) return node.nodeValue;
    if (node.nodeType !== 1) return '';
    var el = node;
    if (!rendered(el)) return '';
    var tag = lower(el.tagName);
    if (tag === 'script' || tag === 'style' || tag === 'noscript' || tag === 'template') return '';
    if (tag === 'img') return el.getAttribute('alt') || '';
    if (tag === 'input') {
      var t = lower(el.type);
      if (t === 'button' || t === 'submit' || t === 'reset') return el.value || '';
      return '';
    }
    var label = el.getAttribute('aria-label');
    if (label && depth > 0) return label;
    var out = '';
    var kids = el.shadowRoot ? el.shadowRoot.childNodes : el.childNodes;
    for (var i = 0; i < kids.length; i++) {
      var piece = textOf(kids[i], depth + 1);
      if (piece) {
        var block = kids[i].nodeType === 1 && /^(div|p|li|br|h[1-6]|tr)$/i.test(kids[i].tagName);
        out += block ? ' ' + piece + ' ' : piece;
      }
    }
    return out;
  }

  function byIds(el, attr) {
    var ids = (el.getAttribute(attr) || '').trim().split(/\s+/);
    var root = el.getRootNode ? el.getRootNode() : document;
    var out = [];
    for (var i = 0; i < ids.length; i++) {
      if (!ids[i]) continue;
      var t = root.getElementById ? root.getElementById(ids[i]) : document.getElementById(ids[i]);
      if (t) out.push(collapse(t.getAttribute('aria-label') || textOf(t, 1)));
    }
    return out.join(' ');
  }

  function labelsOf(el) {
    var out = [];
    if (el.labels && el.labels.length) {
      for (var i = 0; i < el.labels.length; i++) out.push(collapse(textOf(el.labels[i], 1)));
    } else {
      var wrap = el.closest && el.closest('label');
      if (wrap) out.push(collapse(textOf(wrap, 1)));
    }
    return out.join(' ');
  }

  function nameOf(el, role) {
    var n;
    if (el.hasAttribute('aria-labelledby')) {
      n = byIds(el, 'aria-labelledby');
      if (n) return clip(n);
    }
    n = collapse(el.getAttribute('aria-label'));
    if (n) return clip(n);
    var tag = lower(el.tagName);
    if (tag === 'input' || tag === 'textarea' || tag === 'select') {
      var t = lower(el.type);
      if (tag === 'input' && (t === 'button' || t === 'submit' || t === 'reset')) {
        n = collapse(el.value || (t === 'submit' ? 'Submit' : t === 'reset' ? 'Reset' : ''));
        if (n) return clip(n);
      }
      n = labelsOf(el);
      if (n) return clip(n);
      if (tag === 'input' && t === 'image') return clip(collapse(el.alt || el.value));
      n = collapse(el.getAttribute('placeholder'));
      if (n) return clip(n);
      return clip(collapse(el.getAttribute('title')));
    }
    if (tag === 'img') return clip(collapse(el.getAttribute('alt') || el.getAttribute('title')));
    if (tag === 'svg') {
      var title = el.querySelector('title');
      return clip(collapse(title ? title.textContent : ''));
    }
    if (tag === 'fieldset') {
      var legend = el.querySelector('legend');
      if (legend) return clip(collapse(textOf(legend, 1)));
    }
    if (tag === 'table') {
      var cap = el.querySelector('caption');
      if (cap) return clip(collapse(textOf(cap, 1)));
    }
    if (NAME_FROM_CONTENT[role]) {
      n = collapse(textOf(el, 0));
      if (n) return clip(n);
      if (role === 'checkbox' || role === 'radio' || role === 'switch') {
        n = labelsOf(el);
        if (n) return clip(n);
      }
    }
    if (role === 'textbox' && el.isContentEditable) {
      n = collapse(el.getAttribute('aria-placeholder') || el.getAttribute('data-placeholder'));
      if (n) return clip(n);
    }
    return clip(collapse(el.getAttribute('title')));
  }

  function clip(s) {
    return s.length > 500 ? s.slice(0, 500) + '…' : s;
  }

  function valueOf(el, role) {
    var tag = lower(el.tagName);
    if (tag === 'input') {
      var t = lower(el.type);
      if (t === 'checkbox' || t === 'radio' || t === 'button' || t === 'submit' || t === 'reset' || t === 'file' || t === 'image') return '';
      if (t === 'password') return el.value ? '••••' : '';
      return el.value || '';
    }
    if (tag === 'textarea') return el.value || '';
    if (tag === 'select') {
      var sel = [];
      for (var i = 0; i < el.options.length; i++) if (el.options[i].selected) sel.push(collapse(el.options[i].textContent));
      return sel.join(', ');
    }
    if (role === 'textbox' && el.isContentEditable) return clip(collapse(el.innerText));
    var v = el.getAttribute('aria-valuetext') || el.getAttribute('aria-valuenow');
    return v || '';
  }

  function checkedOf(el, role) {
    if (role !== 'checkbox' && role !== 'radio' && role !== 'switch' && role !== 'menuitemcheckbox' && role !== 'menuitemradio' && role !== 'option' && role !== 'treeitem') return undefined;
    var aria = el.getAttribute('aria-checked');
    if (aria === 'true' || aria === 'mixed') return true;
    if (aria === 'false') return false;
    if (role === 'option') {
      var s = el.getAttribute('aria-selected');
      if (s === 'true') return true;
      if (s === 'false') return undefined;
      if (lower(el.tagName) === 'option') return el.selected || undefined;
      return undefined;
    }
    if ('checked' in el && lower(el.tagName) === 'input') return !!el.checked;
    return undefined;
  }

  function disabledOf(el) {
    if (el.disabled === true) return true;
    if (el.getAttribute('aria-disabled') === 'true') return true;
    var fs = el.closest && el.closest('fieldset[disabled]');
    if (fs && !(fs.querySelector('legend') && fs.querySelector('legend').contains(el))) return true;
    return false;
  }

  // ---------------------------------------------------------------- visibility

  function style(el) {
    try {
      return el.ownerDocument.defaultView.getComputedStyle(el);
    } catch (_e) {
      return null;
    }
  }

  // rendered: part of the layout and of the accessibility tree.
  function rendered(el) {
    if (el.hidden && lower(el.tagName) !== 'dialog') return false;
    if (el.getAttribute('aria-hidden') === 'true') return false;
    if (el.hasAttribute && el.hasAttribute('inert')) return false;
    var tag = lower(el.tagName);
    if (tag === 'dialog' && !el.open) return false;
    if (tag === 'details') return true;
    var cs = style(el);
    if (!cs) return true;
    if (cs.display === 'none') return false;
    if (cs.display === 'contents') return true;
    return true;
  }

  function inClosedDetails(el) {
    var d = el.parentElement && el.parentElement.closest('details:not([open])');
    if (!d) return false;
    var sum = d.querySelector(':scope > summary');
    return !(sum && (sum === el || sum.contains(el)));
  }

  // visible: rendered, with a box, not visibility:hidden (Playwright's rule:
  // opacity 0 still counts as visible).
  function visible(el) {
    if (!el.isConnected) return false;
    for (var p = el; p && p.nodeType === 1; p = p.parentElement || (p.getRootNode && p.getRootNode().host)) {
      if (!rendered(p)) return false;
    }
    if (inClosedDetails(el)) return false;
    var cs = style(el);
    if (cs && cs.visibility !== 'visible') return false;
    var r = el.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) return true;
    if (cs && cs.display === 'contents') {
      for (var c = el.firstElementChild; c; c = c.nextElementSibling) if (visible(c)) return true;
    }
    return false;
  }

  function hiddenBy(el) {
    var cs = style(el);
    if (cs && cs.visibility !== 'visible') return 'visibility';
    for (var p = el; p && p.nodeType === 1; p = p.parentElement) {
      var s = style(p);
      if (s && parseFloat(s.opacity) === 0) return 'opacity';
    }
    return '';
  }

  // ---------------------------------------------------------------- tree

  function shouldInclude(role, name, level) {
    if (!role || role === 'none' || role === 'generic') return false;
    if (INTERACTIVE[role]) return true;
    if (level === 'skeleton') return !!SKELETON[role];
    if (level === 'content') return !!(SKELETON[role] || CONTENT[role]);
    return name !== '' || !!SKELETON[role] || !!CONTENT[role];
  }

  function childrenOf(el) {
    var out = [];
    if (el.shadowRoot) {
      for (var s = el.shadowRoot.firstChild; s; s = s.nextSibling) out.push(s);
      return out;
    }
    var tag = lower(el.tagName);
    if (tag === 'slot' && el.assignedNodes) {
      var assigned = el.assignedNodes({ flatten: true });
      if (assigned.length) return Array.prototype.slice.call(assigned);
    }
    if (tag === 'iframe' || tag === 'frame') {
      try {
        var doc = el.contentDocument;
        if (doc && doc.body) return [doc.body];
      } catch (_e) {}
      return out;
    }
    for (var c = el.firstChild; c; c = c.nextSibling) out.push(c);
    return out;
  }

  function snapshot(level, selector) {
    level = level || 'content';
    A.refs = new Map();
    A.meta = {};
    var stats = { total_nodes: 0, filtered_nodes: 0, interactive_count: 0 };
    var warnings = [];
    var counter = 0;
    var nth = {};
    var roots = [document.body || document.documentElement];
    if (selector) {
      var scoped = null;
      try {
        scoped = document.querySelector(selector);
      } catch (e) {
        warnings.push('selector ' + JSON.stringify(selector) + ' is invalid (' + e.message + '): full page instead');
      }
      if (scoped) roots = [scoped];
      else if (!warnings.length) warnings.push('selector ' + JSON.stringify(selector) + ' matched no element: full page instead');
    }

    function walk(node, parentNamed) {
      if (node.nodeType === 3) {
        if (level === 'skeleton' || parentNamed) return [];
        var t = collapse(node.nodeValue);
        if (!t) return [];
        stats.total_nodes++;
        stats.filtered_nodes++;
        return [{ role: 'StaticText', name: clip(t) }];
      }
      if (node.nodeType !== 1) return [];
      var el = node;
      var tag = lower(el.tagName);
      if (tag === 'script' || tag === 'style' || tag === 'noscript' || tag === 'template' || tag === 'head') return [];
      if (!rendered(el) || inClosedDetails(el)) return [];
      stats.total_nodes++;
      var role = roleOf(el);
      var cs = style(el);
      var hiddenVis = cs && cs.visibility === 'hidden';
      var name = role !== 'generic' && role !== 'none' && !hiddenVis ? nameOf(el, role) : '';
      var include = !hiddenVis && shouldInclude(role, name, level);
      var named = parentNamed || (include && NAME_FROM_CONTENT[role] && name !== '');
      // Refs are numbered in document order, a parent before its children
      // (a <select> before its options), like the CDP extractor.
      var ref = '';
      if (include && INTERACTIVE[role]) {
        counter++;
        ref = '@' + counter;
      }
      var kids = [];
      var cn = childrenOf(el);
      for (var i = 0; i < cn.length; i++) {
        var sub = walk(cn[i], named);
        for (var j = 0; j < sub.length; j++) kids.push(sub[j]);
      }
      if (!include) return mergeText(kids);
      var n = { role: role };
      if (name) n.name = name;
      var value = valueOf(el, role);
      if (value && value !== name) n.value = clip(value);
      if (role === 'heading') n.level = headingLevel(el);
      if (role === 'link' && el.href) n.href = String(el.href);
      var checked = checkedOf(el, role);
      if (checked !== undefined) n.checked = checked;
      if (INTERACTIVE[role] && disabledOf(el)) n.disabled = true;
      if (ref) {
        n.ref = ref;
        stats.interactive_count++;
        var key = role + '\u0000' + (name || '');
        var k = nth[key] || 0;
        nth[key] = k + 1;
        A.refs.set(n.ref, el);
        A.meta[n.ref] = { role: role, name: name || '', nth: k };
      }
      kids = mergeText(kids);
      if (kids.length) n.children = kids;
      stats.filtered_nodes++;
      return [n];
    }

    var nodes = [];
    for (var r = 0; r < roots.length; r++) {
      var got = walk(roots[r], false);
      for (var g = 0; g < got.length; g++) nodes.push(got[g]);
    }
    return { nodes: nodes, stats: stats, warnings: warnings, url: location.href, title: document.title };
  }

  // Adjacent text pieces of one parent become one line.
  function mergeText(kids) {
    var out = [];
    for (var i = 0; i < kids.length; i++) {
      var k = kids[i];
      var last = out[out.length - 1];
      if (k.role === 'StaticText' && last && last.role === 'StaticText' && !last.children) {
        last.name = clip(last.name + ' ' + k.name);
        continue;
      }
      out.push(k);
    }
    return out;
  }

  // ---------------------------------------------------------------- locate

  function allElements() {
    var out = [];
    function visit(root) {
      var it = root.querySelectorAll('*');
      for (var i = 0; i < it.length; i++) {
        var el = it[i];
        out.push(el);
        if (el.shadowRoot) visit(el.shadowRoot);
        var tag = lower(el.tagName);
        if (tag === 'iframe' || tag === 'frame') {
          try {
            if (el.contentDocument) visit(el.contentDocument);
          } catch (_e) {}
        }
      }
    }
    visit(document);
    return out;
  }

  function matcher(loc, field) {
    var want = loc[field] || '';
    if (loc.pattern) {
      var re;
      try {
        re = new RegExp(loc.pattern.source, loc.pattern.flags.replace(/[gy]/g, ''));
      } catch (e) {
        throw fail('invalid', 'regex /' + loc.pattern.source + '/' + loc.pattern.flags + ': ' + e.message);
      }
      return function (s) {
        return re.test(collapse(s));
      };
    }
    if (loc.exact) {
      var w = collapse(want);
      return function (s) {
        return collapse(s) === w;
      };
    }
    var lw = collapse(want).toLowerCase();
    return function (s) {
      return collapse(s).toLowerCase().indexOf(lw) >= 0;
    };
  }

  function fail(code, message, extra) {
    var e = new Error(message);
    e.code = code;
    if (extra) e.extra = extra;
    return e;
  }

  function describe(el) {
    var s = lower(el.tagName);
    if (el.id) s += '#' + el.id;
    else if (typeof el.className === 'string' && el.className.trim()) s += '.' + el.className.trim().split(/\s+/).slice(0, 2).join('.');
    return s;
  }

  // candidates returns every element the locator designates, visible or not
  // (refs and CSS), or only visible ones (semantic locators, like CDP).
  function candidates(loc) {
    switch (loc.kind) {
      case 'ref': {
        var el = A.refs.get(loc.ref);
        if (el && el.isConnected) return [el];
        var meta = A.meta[loc.ref];
        if (!meta) throw fail('stale', 'ref ' + loc.ref + ' not found in the last snapshot: take a new snapshot');
        var same = [];
        var all = allElements();
        for (var i = 0; i < all.length; i++) {
          var c = all[i];
          if (roleOf(c) === meta.role && nameOf(c, meta.role) === meta.name && visible(c)) same.push(c);
        }
        if (same.length > meta.nth) {
          A.refs.set(loc.ref, same[meta.nth]);
          return [same[meta.nth]];
        }
        return [];
      }
      case 'css': {
        var list;
        try {
          list = document.querySelectorAll(loc.css);
        } catch (e) {
          throw fail('invalid', 'CSS selector ' + JSON.stringify(loc.css) + ': ' + e.message);
        }
        return Array.prototype.slice.call(list);
      }
      case 'role': {
        var role = loc.role;
        var match = loc.name || loc.pattern ? matcher(loc, 'name') : null;
        var out = [];
        var els = allElements();
        for (var r = 0; r < els.length; r++) {
          var e = els[r];
          if (roleOf(e) !== role) continue;
          if (match && !match(nameOf(e, role))) continue;
          if (visible(e)) out.push(e);
        }
        return out;
      }
      case 'label': {
        var lm = matcher(loc, 'label');
        var res = [];
        var items = allElements();
        for (var l = 0; l < items.length; l++) {
          var it = items[l];
          var rl = roleOf(it);
          if (!(INTERACTIVE[rl] || rl === 'img')) continue;
          var label = it.hasAttribute('aria-labelledby') ? byIds(it, 'aria-labelledby') : it.getAttribute('aria-label') || labelsOf(it);
          if (label && lm(label) && visible(it)) res.push(it);
        }
        return res;
      }
      case 'text': {
        var tm = matcher(loc, 'text');
        var hits = [];
        var nodes = allElements();
        for (var t = 0; t < nodes.length; t++) {
          var n = nodes[t];
          var tag = lower(n.tagName);
          if (tag === 'script' || tag === 'style' || tag === 'head' || tag === 'html' || tag === 'body') continue;
          var txt = tag === 'input' && /^(button|submit|reset)$/i.test(n.type) ? n.value : n.innerText !== undefined ? n.innerText : n.textContent;
          if (!txt || !tm(txt) || !visible(n)) continue;
          hits.push(n);
        }
        // Innermost: drop an element that contains another hit.
        var inner = hits.filter(function (h) {
          for (var k = 0; k < hits.length; k++) if (hits[k] !== h && h.contains(hits[k])) return false;
          return true;
        });
        if (inner.length > 1 && !loc.pattern && !loc.exact) {
          var w = collapse(loc.text).toLowerCase();
          var exact = inner.filter(function (h) {
            return collapse(h.innerText || h.textContent).toLowerCase() === w;
          });
          if (exact.length) inner = exact;
        }
        return inner;
      }
    }
    throw fail('invalid', 'unknown target kind ' + loc.kind);
  }

  function describeLoc(loc) {
    switch (loc.kind) {
      case 'ref':
        return loc.ref;
      case 'css':
        return 'CSS selector ' + JSON.stringify(loc.css);
      default:
        return loc.desc || loc.kind;
    }
  }

  // resolve returns the single element of loc, or throws notfound/ambiguous.
  function resolve(loc) {
    var c = candidates(loc);
    if (loc.kind === 'css' && c.length > 1) {
      var vis = c.filter(visible);
      if (vis.length === 1) c = vis;
    }
    if (c.length === 0) throw fail('notfound', describeLoc(loc) + ' matched no element');
    if (c.length > 1) throw fail('ambiguous', describeLoc(loc) + ' matched ' + c.length + ' elements; target must be unique (' + c.slice(0, 5).map(describe).join(', ') + ')');
    return c[0];
  }

  var rects = new WeakMap();

  // state checks one target state: visible, hidden, attached, enabled, stable.
  function state(loc, want) {
    var el;
    try {
      el = resolve(loc);
    } catch (e) {
      if (want === 'hidden' && e.code === 'notfound') return { ok: true };
      throw e;
    }
    var vis = visible(el);
    switch (want) {
      case 'attached':
        return { ok: true };
      case 'hidden':
        return vis ? { ok: false, why: 'still visible' } : { ok: true };
      case 'enabled':
        if (!vis) return { ok: false, why: 'not visible' };
        return disabledOf(el) ? { ok: false, why: 'disabled' } : { ok: true };
      case 'stable': {
        if (!vis) return { ok: false, why: 'not visible' };
        var r = el.getBoundingClientRect();
        var key = [r.x, r.y, r.width, r.height].join(',');
        var prev = rects.get(el);
        rects.set(el, key);
        return prev === key ? { ok: true } : { ok: false, why: 'moving' };
      }
      default:
        return vis ? { ok: true } : { ok: false, why: 'not visible' };
    }
  }

  // ---------------------------------------------------------------- actions

  function center(el) {
    var r = el.getBoundingClientRect();
    var x = r.left + r.width / 2;
    var y = r.top + r.height / 2;
    // Coordinates of an element inside a same-origin iframe, in the top
    // window, for the server (screenshots); events use the frame's own.
    var ox = 0;
    var oy = 0;
    var w = el.ownerDocument.defaultView;
    while (w && w !== window && w.frameElement) {
      var fr = w.frameElement.getBoundingClientRect();
      ox += fr.left;
      oy += fr.top;
      w = w.parent;
    }
    return { x: x, y: y, top: { x: x + ox, y: y + oy }, rect: { x: r.left + ox, y: r.top + oy, w: r.width, h: r.height } };
  }

  function inViewport(el) {
    var r = el.getBoundingClientRect();
    var view = el.ownerDocument.defaultView;
    return r.bottom > 0 && r.right > 0 && r.top < view.innerHeight && r.left < view.innerWidth;
  }

  // actionable resolves loc and checks it can take a pointer action: visible
  // (or only hidden by opacity/visibility when reveal is allowed), enabled,
  // in view (scrolled there), not covered by another element.
  function actionable(loc, opts) {
    var el = resolve(loc);
    var note = '';
    if (!visible(el)) {
      var by = hiddenBy(el);
      var r = el.getBoundingClientRect();
      if (opts.reveal && by && r.width > 0 && r.height > 0) {
        note = ' (clicked while hidden by ' + by + ': the bridge cannot apply CSS :hover; its events reach the element directly)';
      } else {
        throw fail('hidden', describeLoc(loc) + ' is not visible' + (by ? ' (hidden by ' + by + ')' : ''));
      }
    } else if (hiddenBy(el) === 'opacity') {
      note = opts.reveal ? ' (clicked while hidden by opacity: the bridge cannot apply CSS :hover; its events reach the element directly)' : '';
      if (!opts.reveal) throw fail('hidden', describeLoc(loc) + ' is transparent (opacity 0): it only shows on hover; pass hover_first: true to click it anyway');
    }
    if (opts.enabled !== false && disabledOf(el)) throw fail('disabled', describeLoc(loc) + ' is disabled');
    if (!inViewport(el)) el.scrollIntoView({ block: 'center', inline: 'center' });
    if (!note && opts.hitTest !== false) {
      var c = center(el);
      var doc = el.ownerDocument;
      var hit = doc.elementFromPoint(c.x, c.y);
      // A hit inside an open shadow root reports its host.
      var target = hit;
      while (target && target.shadowRoot) {
        var deeper = target.shadowRoot.elementFromPoint(c.x, c.y);
        if (!deeper || deeper === target) break;
        target = deeper;
      }
      if (target && target !== el && !el.contains(target) && !(target.contains(el) && lower(target.tagName) === 'label')) {
        var lab = target.closest && target.closest('label');
        if (!(lab && lab.control === el)) throw fail('covered', describeLoc(loc) + ' is covered by <' + describe(target) + '> at its centre');
      }
    }
    return { el: el, note: note };
  }

  function mouseInit(el, x, y, button, buttons, detail, mods) {
    var view = el.ownerDocument.defaultView;
    mods = mods || {};
    return {
      bubbles: true,
      cancelable: true,
      composed: true,
      view: view,
      clientX: x,
      clientY: y,
      screenX: x + (view.screenX || 0),
      screenY: y + (view.screenY || 0),
      button: button,
      buttons: buttons,
      detail: detail,
      pointerId: 1,
      pointerType: 'mouse',
      isPrimary: true,
      ctrlKey: !!mods.Control,
      shiftKey: !!mods.Shift,
      altKey: !!mods.Alt,
      metaKey: !!mods.Meta,
    };
  }

  function fire(el, type, init, Ctor) {
    var view = el.ownerDocument.defaultView;
    var C = Ctor && view[Ctor] ? view[Ctor] : view.MouseEvent;
    var ev = new C(type, init);
    return el.dispatchEvent(ev);
  }

  function focusable(el) {
    return el.closest('a[href],button,input,select,textarea,summary,[tabindex],[contenteditable=""],[contenteditable="true"]');
  }

  function hoverAt(el, x, y) {
    var init = mouseInit(el, x, y, 0, 0, 0);
    fire(el, 'pointerover', init, 'PointerEvent');
    fire(el, 'pointerenter', Object.assign({}, init, { bubbles: false }), 'PointerEvent');
    fire(el, 'mouseover', init);
    fire(el, 'mouseenter', Object.assign({}, init, { bubbles: false }));
    fire(el, 'pointermove', init, 'PointerEvent');
    fire(el, 'mousemove', init);
  }

  var BUTTONS = { left: [0, 1], middle: [1, 4], right: [2, 2] };

  function clickEl(el, button, count) {
    var c = center(el);
    var b = BUTTONS[button] || BUTTONS.left;
    hoverAt(el, c.x, c.y);
    for (var i = 1; i <= count; i++) {
      var down = mouseInit(el, c.x, c.y, b[0], b[1], i);
      var up = mouseInit(el, c.x, c.y, b[0], 0, i);
      fire(el, 'pointerdown', down, 'PointerEvent');
      var focusOk = fire(el, 'mousedown', down);
      if (focusOk && i === 1) {
        var f = focusable(el);
        if (f && f !== el.ownerDocument.activeElement) f.focus({ preventScroll: true });
        else if (!f && el.ownerDocument.activeElement && el.ownerDocument.activeElement !== el.ownerDocument.body) el.ownerDocument.activeElement.blur();
      }
      fire(el, 'pointerup', up, 'PointerEvent');
      fire(el, 'mouseup', up);
      if (button === 'right') {
        fire(el, 'contextmenu', up);
      } else if (button === 'left' || !button) {
        fire(el, 'click', up);
      } else {
        fire(el, 'auxclick', up);
      }
    }
    if (count === 2 && (button === 'left' || !button)) fire(el, 'dblclick', mouseInit(el, c.x, c.y, 0, 0, 2));
  }

  function clickAt(x, y, count) {
    var el = document.elementFromPoint(x, y);
    if (!el) throw fail('notfound', 'no element at ' + x + ',' + y);
    var b = BUTTONS.left;
    for (var i = 1; i <= count; i++) {
      var down = mouseInit(el, x, y, b[0], b[1], i);
      var up = mouseInit(el, x, y, b[0], 0, i);
      fire(el, 'pointerdown', down, 'PointerEvent');
      if (fire(el, 'mousedown', down) && i === 1) {
        var f = focusable(el);
        if (f) f.focus({ preventScroll: true });
      }
      fire(el, 'pointerup', up, 'PointerEvent');
      fire(el, 'mouseup', up);
      fire(el, 'click', up);
    }
    if (count === 2) fire(el, 'dblclick', mouseInit(el, x, y, 0, 0, 2));
    return { tag: describe(el) };
  }

  function isEditable(el) {
    if (!el) return false;
    var tag = lower(el.tagName);
    if (tag === 'textarea') return !el.readOnly && !el.disabled;
    if (tag === 'input') {
      var t = lower(el.type);
      return !/^(checkbox|radio|button|submit|reset|file|image|range|color|hidden)$/.test(t) && !el.readOnly && !el.disabled;
    }
    return !!el.isContentEditable;
  }

  function nativeSetValue(el, value) {
    var proto = lower(el.tagName) === 'textarea' ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    var desc = Object.getOwnPropertyDescriptor(proto, 'value');
    if (desc && desc.set) desc.set.call(el, value);
    else el.value = value;
  }

  // insertText types text at the caret of the focused editable element, like
  // a keyboard would: beforeinput/input events (execCommand keeps the
  // undo stack and editors' own handlers); the native value setter plus an
  // input event when execCommand is refused.
  function insertText(el, text) {
    var before = 'value' in el && lower(el.tagName) !== 'select' ? el.value : null;
    var ok = false;
    try {
      ok = el.ownerDocument.execCommand('insertText', false, text);
    } catch (_e) {}
    if (before !== null) {
      if (!ok || el.value === before) {
        var start = el.selectionStart != null ? el.selectionStart : el.value.length;
        var end = el.selectionEnd != null ? el.selectionEnd : el.value.length;
        nativeSetValue(el, el.value.slice(0, start) + text + el.value.slice(end));
        try {
          el.setSelectionRange(start + text.length, start + text.length);
        } catch (_e) {}
        el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType: 'insertText', data: text }));
      }
    } else if (!ok) {
      throw fail('invalid', describe(el) + ' did not accept text');
    }
  }

  function selectAll(el) {
    if ('select' in el && typeof el.select === 'function' && lower(el.tagName) !== 'select') {
      try {
        el.select();
        return;
      } catch (_e) {}
    }
    if (el.isContentEditable) {
      var range = el.ownerDocument.createRange();
      range.selectNodeContents(el);
      var sel = el.ownerDocument.defaultView.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
    }
  }

  function clearEditable(el) {
    selectAll(el);
    var ok = false;
    try {
      ok = el.ownerDocument.execCommand('delete', false);
    } catch (_e) {}
    if ('value' in el && el.value !== '') {
      nativeSetValue(el, '');
      el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType: 'deleteContentBackward' }));
    } else if (!ok && el.isContentEditable && el.textContent) {
      el.textContent = '';
      el.dispatchEvent(new InputEvent('input', { bubbles: true, composed: true, inputType: 'deleteContentBackward' }));
    }
  }

  function typeInto(el, text, append) {
    if (!isEditable(el)) {
      var inner = el.querySelector && el.querySelector('input:not([type=hidden]),textarea,[contenteditable=""],[contenteditable="true"]');
      if (inner && isEditable(inner)) el = inner;
      else throw fail('invalid', describe(el) + ' is not editable (not an input, a textarea or a contenteditable element)');
    }
    el.focus({ preventScroll: true });
    if (append) {
      if ('value' in el) {
        try {
          el.setSelectionRange(el.value.length, el.value.length);
        } catch (_e) {}
      } else {
        var sel = el.ownerDocument.defaultView.getSelection();
        sel.selectAllChildren(el);
        sel.collapseToEnd();
      }
    } else {
      clearEditable(el);
    }
    if (text) insertText(el, text);
    if ('value' in el) el.dispatchEvent(new Event('change', { bubbles: true }));
    return el;
  }

  var KEYCODES = { Enter: 13, Tab: 9, Escape: 27, Backspace: 8, Delete: 46, ' ': 32, ArrowUp: 38, ArrowDown: 40, ArrowLeft: 37, ArrowRight: 39, Home: 36, End: 35, PageUp: 33, PageDown: 34, Insert: 45 };
  var CODES = { Enter: 'Enter', Tab: 'Tab', Escape: 'Escape', Backspace: 'Backspace', Delete: 'Delete', ' ': 'Space', ArrowUp: 'ArrowUp', ArrowDown: 'ArrowDown', ArrowLeft: 'ArrowLeft', ArrowRight: 'ArrowRight', Home: 'Home', End: 'End', PageUp: 'PageUp', PageDown: 'PageDown', Insert: 'Insert' };

  function keyInit(key, mods) {
    var code = CODES[key];
    var kc = KEYCODES[key];
    if (!code) {
      if (/^F\d+$/.test(key)) {
        code = key;
        kc = 111 + parseInt(key.slice(1), 10);
      } else if (/^[a-z]$/i.test(key)) {
        code = 'Key' + key.toUpperCase();
        kc = key.toUpperCase().charCodeAt(0);
      } else if (/^[0-9]$/.test(key)) {
        code = 'Digit' + key;
        kc = key.charCodeAt(0);
      } else {
        code = '';
        kc = key.charCodeAt(0);
      }
    }
    return {
      key: mods.Shift && key.length === 1 ? key.toUpperCase() : key,
      code: code,
      keyCode: kc,
      which: kc,
      bubbles: true,
      cancelable: true,
      composed: true,
      ctrlKey: !!mods.Control,
      shiftKey: !!mods.Shift,
      altKey: !!mods.Alt,
      metaKey: !!mods.Meta,
    };
  }

  function tabbables(doc) {
    var list = Array.prototype.slice.call(doc.querySelectorAll('a[href],area[href],button,input:not([type=hidden]),select,textarea,summary,iframe,[tabindex],[contenteditable=""],[contenteditable="true"]'));
    list = list.filter(function (e) {
      var ti = e.getAttribute('tabindex');
      return !(ti !== null && parseInt(ti, 10) < 0) && !e.disabled && visible(e);
    });
    var pos = list.filter(function (e) {
      return parseInt(e.getAttribute('tabindex'), 10) > 0;
    });
    pos.sort(function (a, b) {
      return parseInt(a.getAttribute('tabindex'), 10) - parseInt(b.getAttribute('tabindex'), 10);
    });
    var zero = list.filter(function (e) {
      return !(parseInt(e.getAttribute('tabindex'), 10) > 0);
    });
    return pos.concat(zero);
  }

  function deepActive(doc) {
    var a = doc.activeElement;
    while (a && a.shadowRoot && a.shadowRoot.activeElement) a = a.shadowRoot.activeElement;
    while (a && (lower(a.tagName) === 'iframe' || lower(a.tagName) === 'frame')) {
      try {
        a = a.contentDocument.activeElement;
      } catch (_e) {
        break;
      }
    }
    return a || doc.body;
  }

  // press dispatches one key combination to el (the focused element) and
  // performs the default action a keyboard would when nothing prevented it.
  function press(el, combo) {
    var mods = {};
    combo.mods.forEach(function (m) {
      mods[m] = true;
    });
    var key = combo.key;
    var init = keyInit(key, mods);
    var doc = el.ownerDocument;
    var KE = doc.defaultView.KeyboardEvent;
    var proceed = el.dispatchEvent(new KE('keydown', init));
    var printable = key.length === 1 && !mods.Control && !mods.Meta && !mods.Alt;
    if (proceed && (printable || key === 'Enter')) proceed = el.dispatchEvent(new KE('keypress', Object.assign({}, init, { charCode: printable ? init.key.charCodeAt(0) : 13 })));
    if (proceed) {
      var editable = isEditable(el);
      var plain = !mods.Control && !mods.Meta && !mods.Alt;
      if ((mods.Meta || mods.Control) && lower(key) === 'a' && editable) selectAll(el);
      else if (printable && editable) insertText(el, init.key);
      else if (key === 'Tab' && !mods.Control && !mods.Meta && !mods.Alt) {
        var order = tabbables(doc);
        if (order.length) {
          var i = order.indexOf(el);
          var next = mods.Shift ? (i <= 0 ? order[order.length - 1] : order[i - 1]) : i < 0 || i === order.length - 1 ? order[0] : order[i + 1];
          next.focus();
        }
      } else if (key === 'Enter' && plain) {
        var tag = lower(el.tagName);
        if (tag === 'textarea') insertText(el, '\n');
        else if (el.isContentEditable) {
          try {
            doc.execCommand(mods.Shift ? 'insertLineBreak' : 'insertParagraph', false);
          } catch (_e) {}
        } else if (tag === 'input' && el.form && isEditable(el)) {
          if (el.form.requestSubmit) el.form.requestSubmit();
          else el.form.submit();
        } else if (tag === 'button' || tag === 'a' || tag === 'summary' || roleOf(el) === 'button' || roleOf(el) === 'link') {
          clickEl(el, 'left', 1);
        }
      } else if (key === ' ' && plain) {
        var t2 = lower(el.tagName);
        if (t2 === 'button' || t2 === 'summary' || (t2 === 'input' && /^(checkbox|radio|button|submit|reset)$/i.test(el.type)) || roleOf(el) === 'button' || roleOf(el) === 'checkbox' || roleOf(el) === 'switch') clickEl(el, 'left', 1);
        else if (editable) insertText(el, ' ');
        else (doc.scrollingElement || doc.documentElement).scrollBy(0, doc.defaultView.innerHeight * 0.8);
      } else if ((key === 'Backspace' || key === 'Delete') && editable) {
        try {
          doc.execCommand(key === 'Backspace' ? 'delete' : 'forwardDelete', false);
        } catch (_e) {}
      } else if (!editable && plain && /^(ArrowDown|ArrowUp|PageDown|PageUp|Home|End)$/.test(key) && (el === doc.body || el === doc.documentElement)) {
        var h = doc.defaultView.innerHeight;
        var dy = { ArrowDown: 40, ArrowUp: -40, PageDown: h * 0.8, PageUp: -h * 0.8, Home: -1e9, End: 1e9 }[key];
        (doc.scrollingElement || doc.documentElement).scrollBy(0, dy);
      } else if (editable && 'value' in el && plain && /^(ArrowLeft|ArrowRight|Home|End)$/.test(key)) {
        var p = el.selectionStart || 0;
        var np = key === 'ArrowLeft' ? Math.max(0, p - 1) : key === 'ArrowRight' ? Math.min(el.value.length, p + 1) : key === 'Home' ? 0 : el.value.length;
        try {
          el.setSelectionRange(np, np);
        } catch (_e) {}
      }
    }
    el.dispatchEvent(new KE('keyup', init));
  }

  function selectOptions(el, values) {
    if (lower(el.tagName) !== 'select') {
      throw fail('invalid', describe(el) + ' is not a native <select>: click it, then click the option');
    }
    var picked = [];
    var missing = [];
    values.forEach(function (v) {
      var found = null;
      for (var i = 0; i < el.options.length; i++) {
        var o = el.options[i];
        if (collapse(o.textContent) === collapse(v) || o.value === v || o.label === v) {
          found = o;
          break;
        }
      }
      if (!found) {
        var lv = collapse(v).toLowerCase();
        for (var j = 0; j < el.options.length; j++) {
          if (collapse(el.options[j].textContent).toLowerCase() === lv) {
            found = el.options[j];
            break;
          }
        }
      }
      if (found) picked.push(found);
      else missing.push(v);
    });
    if (missing.length) {
      var avail = [];
      for (var k = 0; k < el.options.length; k++) avail.push(collapse(el.options[k].textContent));
      throw fail('invalid', 'no option ' + JSON.stringify(missing) + ' in ' + describe(el) + ' (options: ' + JSON.stringify(avail) + ')');
    }
    if (picked.length > 1 && !el.multiple) throw fail('invalid', describe(el) + ' takes a single value');
    for (var m = 0; m < el.options.length; m++) el.options[m].selected = picked.indexOf(el.options[m]) >= 0;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }

  function scrollable(el) {
    for (var p = el; p && p.nodeType === 1; p = p.parentElement) {
      var cs = style(p);
      if (cs && /(auto|scroll|overlay)/.test(cs.overflowY) && p.scrollHeight > p.clientHeight + 1) return p;
    }
    return document.scrollingElement || document.documentElement;
  }

  function wheelAt(x, y, dy) {
    var el = document.elementFromPoint(x, y) || document.body;
    var init = mouseInit(el, x, y, 0, 0, 0);
    init.deltaY = dy;
    init.deltaMode = 0;
    var proceed = el.dispatchEvent(new WheelEvent('wheel', init));
    if (proceed) scrollable(el).scrollBy(0, dy);
    return { tag: describe(el), prevented: !proceed };
  }

  function drag(src, dst, steps) {
    var a = center(src);
    var b = center(dst);
    var dt = null;
    try {
      dt = new DataTransfer();
    } catch (_e) {}
    hoverAt(src, a.x, a.y);
    fire(src, 'pointerdown', mouseInit(src, a.x, a.y, 0, 1, 1), 'PointerEvent');
    fire(src, 'mousedown', mouseInit(src, a.x, a.y, 0, 1, 1));
    var html5 = src.draggable || (src.closest && src.closest('[draggable=true]'));
    if (html5) fire(src, 'dragstart', Object.assign(mouseInit(src, a.x, a.y, 0, 1, 0), { dataTransfer: dt }), 'DragEvent');
    var last = src;
    for (var i = 1; i <= steps; i++) {
      var x = a.x + ((b.x - a.x) * i) / steps;
      var y = a.y + ((b.y - a.y) * i) / steps;
      var over = document.elementFromPoint(x, y) || dst;
      fire(over, 'pointermove', mouseInit(over, x, y, 0, 1, 0), 'PointerEvent');
      fire(over, 'mousemove', mouseInit(over, x, y, 0, 1, 0));
      if (html5) {
        if (over !== last) fire(over, 'dragenter', Object.assign(mouseInit(over, x, y, 0, 1, 0), { dataTransfer: dt }), 'DragEvent');
        fire(over, 'dragover', Object.assign(mouseInit(over, x, y, 0, 1, 0), { dataTransfer: dt }), 'DragEvent');
      }
      last = over;
    }
    if (html5) {
      fire(dst, 'drop', Object.assign(mouseInit(dst, b.x, b.y, 0, 0, 0), { dataTransfer: dt }), 'DragEvent');
      fire(src, 'dragend', Object.assign(mouseInit(src, b.x, b.y, 0, 0, 0), { dataTransfer: dt }), 'DragEvent');
    }
    fire(dst, 'pointerup', mouseInit(dst, b.x, b.y, 0, 0, 1), 'PointerEvent');
    fire(dst, 'mouseup', mouseInit(dst, b.x, b.y, 0, 0, 1));
  }

  function upload(el, files) {
    if (!(lower(el.tagName) === 'input' && lower(el.type) === 'file')) {
      var inner = el.querySelector && el.querySelector('input[type=file]');
      if (!inner && lower(el.tagName) === 'label' && el.control) inner = el.control;
      if (!inner || lower(inner.type) !== 'file') throw fail('invalid', describe(el) + ' is not a file input (give the <input type=file> as a CSS selector)');
      el = inner;
    }
    var dt = new DataTransfer();
    files.forEach(function (f) {
      var bin = atob(f.b64);
      var bytes = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      dt.items.add(new File([bytes], f.name, { type: f.type || '' }));
    });
    el.files = dt.files;
    el.dispatchEvent(new Event('input', { bubbles: true }));
    el.dispatchEvent(new Event('change', { bubbles: true }));
  }

  // ---------------------------------------------------------------- serialise

  function serialise(v) {
    if (v === undefined) return 'undefined';
    if (typeof v === 'string') return v;
    if (typeof v === 'number' || typeof v === 'boolean' || v === null) return String(v);
    if (typeof v === 'bigint') return v.toString();
    if (typeof v === 'function') return '[Function ' + (v.name || 'anonymous') + ']';
    if (typeof Element !== 'undefined' && v instanceof Element) return '<' + describe(v) + '>';
    try {
      var seen = new WeakSet();
      var out = JSON.stringify(v, function (_k, x) {
        if (typeof x === 'bigint') return x.toString() + 'n';
        if (typeof x === 'function') return '[Function ' + (x.name || 'anonymous') + ']';
        if (typeof x === 'number' && !isFinite(x)) return String(x);
        if (x && typeof x === 'object') {
          if (typeof Element !== 'undefined' && x instanceof Element) return '<' + describe(x) + '>';
          if (seen.has(x)) return '[Circular]';
          seen.add(x);
          if (x instanceof Map) return Object.fromEntries(x);
          if (x instanceof Set) return Array.from(x);
          if (x instanceof Error) return { name: x.name, message: x.message, stack: x.stack };
        }
        return x;
      });
      return out === undefined ? String(v) : out;
    } catch (e) {
      return String(v);
    }
  }

  // ---------------------------------------------------------------- dialogs

  // JavaScript dialogs would open a native, blocking panel: they are answered
  // by the session's policy (the dialog tool) and recorded in the log.
  function installDialogs() {
    var rec = function (kind, msg) {
      var st = window.__MCP_APP_DESKTOP__;
      if (st && st.add) st.add('page', 'info', kind + ' dialog ' + (A.dialogs.accept ? 'accepted' : 'dismissed') + ': ' + String(msg));
    };
    window.alert = function (msg) {
      rec('alert', msg);
    };
    window.confirm = function (msg) {
      rec('confirm', msg);
      return !!A.dialogs.accept;
    };
    window.prompt = function (msg, def) {
      rec('prompt', msg);
      if (!A.dialogs.accept) return null;
      return A.dialogs.text !== '' ? A.dialogs.text : def === undefined ? '' : String(def);
    };
  }

  // ---------------------------------------------------------------- API

  var observer = new MutationObserver(function () {
    A.lastMutation = Date.now();
  });
  observer.observe(document, { childList: true, subtree: true, attributes: true, characterData: true });

  function errorOf(e) {
    return { error: e && e.message ? e.message : String(e), code: (e && e.code) || 'error' };
  }

  A.run = function (op, args) {
    try {
      switch (op) {
        case 'ping':
          return { ok: true };
        case 'info':
          return { url: location.href, title: document.title, ready: document.readyState, boot: window.__MCP_APP_DESKTOP__ ? window.__MCP_APP_DESKTOP__.boot : '', w: window.innerWidth, h: window.innerHeight, dpr: window.devicePixelRatio, scroll: window.scrollX + window.scrollY, mutated: A.lastMutation, now: Date.now() };
        case 'snapshot':
          return snapshot(args.level, args.selector);
        case 'state':
          return state(args.loc, args.state);
        case 'text': {
          var body = document.body ? document.body.innerText : '';
          return { ok: body.indexOf(args.text) >= 0 };
        }
        case 'click': {
          var a = actionable(args.loc, { reveal: args.reveal });
          clickEl(a.el, args.button, args.count);
          return { note: a.note };
        }
        case 'clickAt':
          return clickAt(args.x, args.y, args.count);
        case 'type': {
          var el;
          if (args.loc) el = actionable(args.loc, { reveal: false, hitTest: false }).el;
          else el = deepActive(document);
          var used = typeInto(el, args.text, args.append);
          if (args.submit) press(used, { mods: [], key: 'Enter' });
          return {};
        }
        case 'press': {
          var target = args.loc ? actionable(args.loc, { reveal: false, hitTest: false }).el : deepActive(document);
          if (args.loc) target.focus({ preventScroll: true });
          press(target, args.combo);
          return {};
        }
        case 'select':
          selectOptions(actionable(args.loc, { reveal: false, hitTest: false }).el, args.values);
          return {};
        case 'hover': {
          var h = actionable(args.loc, { reveal: true, hitTest: false }).el;
          var hc = center(h);
          hoverAt(h, hc.x, hc.y);
          return {};
        }
        case 'drag': {
          var s = actionable(args.from, { reveal: false, hitTest: false }).el;
          var d = actionable(args.to, { reveal: false, hitTest: false }).el;
          drag(s, d, args.steps);
          return {};
        }
        case 'scrollIntoView': {
          var sv = resolve(args.loc);
          sv.scrollIntoView({ block: 'center', inline: 'center' });
          return {};
        }
        case 'scrollBy':
          window.scrollBy(args.dx, args.dy);
          return { pos: window.scrollX + window.scrollY };
        case 'point': {
          var pe = actionable(args.loc, { reveal: false, hitTest: false }).el;
          return center(pe);
        }
        case 'rect': {
          var re = resolve(args.loc);
          if (!inViewport(re)) re.scrollIntoView({ block: 'center', inline: 'center' });
          return center(re);
        }
        case 'wheel':
          return wheelAt(args.x, args.y, args.dy);
        case 'upload':
          upload(resolve(args.loc), args.files);
          return {};
        case 'navigate':
          location.href = args.url;
          return {};
        case 'history':
          history.go(args.delta);
          return {};
        case 'reload':
          location.reload();
          return {};
        case 'take': {
          var r = A.results[args.id];
          if (!r) return { pending: true };
          delete A.results[args.id];
          return r;
        }
        case 'drop':
          delete A.results[args.id];
          return {};
      }
      return { error: 'unknown operation ' + op, code: 'invalid' };
    } catch (e) {
      return errorOf(e);
    }
  };

  // evalTarget resolves the element an eval binds as `this`.
  A.evalTarget = function (loc) {
    return resolve(loc);
  };

  // start runs an async evaluation and keeps its outcome for take.
  // describeError renders a thrown value: "Name: message", then the stack
  // when it adds something (WebKit's stack lists frames only).
  function describeError(e) {
    if (!(e instanceof Error) && !(e && typeof e === 'object' && 'message' in e)) return serialise(e);
    var head = (e.name || 'Error') + ': ' + e.message;
    var stack = e.stack ? String(e.stack) : '';
    if (!stack) return head;
    return stack.indexOf(e.message) >= 0 ? stack : head + '\n' + stack;
  }

  A.start = function (fn, self) {
    var id = 'e' + ++A.nextId;
    var done = function (r) {
      A.results[id] = r;
    };
    try {
      Promise.resolve(fn.call(self)).then(
        function (v) {
          done({ value: serialise(v) });
        },
        function (e) {
          done({ error: describeError(e), code: 'exception' });
        }
      );
    } catch (e) {
      done({ error: describeError(e), code: 'exception' });
    }
    return { pending: id };
  };

  installDialogs();
  Object.defineProperty(window, '__MCP_AD_AGENT__', { value: A, configurable: true, enumerable: false, writable: true });
})();
