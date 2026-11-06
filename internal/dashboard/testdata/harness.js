'use strict';
// Test-only (never embedded): TestScriptBehaviourInChrome injects it after
// copy.js. The URL fragment #s=<name> picks the scenario; it drives copy.js
// with synthetic events and writes what it saw on body[data-result].
(function () {
  var name = (location.hash.match(/s=([a-z]+)/) || [])[1];
  if (!name) {
    return;
  }
  var b = document.body;
  function out(s) {
    b.setAttribute('data-result', s);
  }
  function key(k, opts, target) {
    var o = opts || {};
    o.key = k;
    o.bubbles = true;
    o.cancelable = true;
    (target || document.body).dispatchEvent(new KeyboardEvent('keydown', o));
  }
  function desc(e) {
    return e ? e.tagName.toLowerCase() + '.' + e.className.replace(/ /g, '+') + (e.name ? '[' + e.name + ']' : '') : 'none';
  }
  function q(sel) {
    return document.querySelector(sel);
  }
  function clip(fn) {
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: fn } });
  }
  // A modifier test: nothing may follow; the page must stay.
  function stay(first, second) {
    return function () {
      out('sent');
      key('g', first);
      key('q', second);
      setTimeout(function () { out('stayed'); }, 300);
    };
  }
  var S = {
    gq: function () { out('sent'); key('g'); key('q'); },
    refresh: function () { out('sent'); key('r'); },
    ctrl: stay({ ctrlKey: true }),
    alt: stay({ altKey: true }),
    meta: stay({ metaKey: true }),
    ctrlq: stay({}, { ctrlKey: true }),
    slow: function () {
      out('sent');
      key('g');
      setTimeout(function () {
        key('q');
        setTimeout(function () { out('stayed'); }, 100);
      }, 1500);
    },
    typing: function () {
      var d = q('details.proj');
      var i = d.querySelector('input[name=project]');
      d.open = true;
      i.focus();
      out('sent');
      key('g', {}, i);
      key('q', {}, i);
      setTimeout(function () { out('stayed'); }, 300);
    },
    slash: function () {
      key('/');
      out('open=' + q('details.proj').open + ';focus=' + desc(document.activeElement));
    },
    help: function () {
      var d = q('dialog.keys');
      var before = q('a.refresh-t');
      before.focus();
      key('?', { shiftKey: true });
      var opened = d.open + ':' + desc(document.activeElement);
      key('Escape');
      setTimeout(function () {
        out('opened=' + opened + ';closed=' + !d.open + ';focus=' + desc(document.activeElement) + ';before=' + desc(before));
      }, 100);
    },
    nodialog: function () {
      key('?', { shiftKey: true });
      out('dialog=' + !!q('dialog') + ';button=' + !!q('button.keyhint-btn'));
    },
    footer: function () {
      var d = q('dialog.keys');
      var btn = q('button.keyhint-btn');
      var hidden = btn.hidden;
      btn.focus();
      btn.click();
      var opened = d.open + ':' + desc(document.activeElement);
      d.querySelector('button').click();
      setTimeout(function () {
        out('hidden=' + hidden + ';opened=' + opened + ';closed=' + !d.open + ';focus=' + desc(document.activeElement) + ';btn=' + desc(btn));
      }, 100);
    },
    cal: function () {
      var tip = q('p.hm-tip');
      var first = q('a.hm-c[tabindex="0"]');
      first.focus();
      function at(prefix, prev) {
        var cur = document.activeElement;
        return prefix + ':' + cur.getAttribute('href') + ':' + cur.getAttribute('tabindex') + ':' + prev.getAttribute('tabindex') +
          ':tip=' + (tip.textContent === cur.getAttribute('aria-label') ? 'ok' : 'bad');
      }
      key('ArrowRight', {}, first);
      var r = at('right', first);
      var second = document.activeElement;
      key('ArrowDown', {}, second);
      var d = at('down', second);
      var third = document.activeElement;
      key('End', {}, third);
      var e = at('end', third);
      var last = document.activeElement;
      key('Home', {}, last);
      out([r, d, e, at('home', last), 'zeros=' + document.querySelectorAll('a.hm-c[tabindex="0"]').length].join(';'));
    },
    copyfail: function () {
      clip(function () { return Promise.reject(new Error('refused')); });
      var btn = q('button.copy[data-copy]');
      btn.click();
      setTimeout(function () {
        out('open=' + q('details.summary-text').open + ';label=' + btn.textContent.trim());
      }, 300);
    },
    copyok: function () {
      var got = '';
      clip(function (t) { got = t; return Promise.resolve(); });
      var btn = q('button.copy[data-copy]');
      btn.click();
      setTimeout(function () {
        out('text=' + got + ';open=' + q('details.summary-text').open + ';label=' + btn.textContent.trim());
      }, 300);
    }
  };
  document.addEventListener('DOMContentLoaded', function () {
    setTimeout(S[name], 0);
  });
})();
