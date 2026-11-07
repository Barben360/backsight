'use strict';
// The one script. Copy buttons: the page renders them hidden; this script
// shows them and makes them copy the command (or data-copy text) next to
// them. Keyboard shortcuts, the shortcuts dialog (only under
// body[data-keys="1"]) and the calendar's arrow keys use the same listener.
document.addEventListener('DOMContentLoaded', function () {
  var doc = document;
  var timer = 0;
  var active = null; // {button, kids}: the button showing a result
  var status = doc.getElementById('copy-status');

  function restore() {
    if (!active) {
      return;
    }
    clearTimeout(timer);
    active.button.replaceChildren.apply(active.button, active.kids);
    active = null;
  }

  function show(button, label) {
    restore();
    var kids = Array.prototype.slice.call(button.childNodes);
    active = { button: button, kids: kids };
    button.replaceChildren(doc.createTextNode(label));
    if (status) {
      status.textContent = label;
    }
    timer = setTimeout(restore, 2000);
  }

  function select(code) {
    var sel = window.getSelection();
    var range = doc.createRange();
    range.selectNodeContents(code);
    sel.removeAllRanges();
    sel.addRange(range);
  }

  doc.querySelectorAll('button.copy[hidden]').forEach(function (button) {
    button.removeAttribute('hidden');
    button.addEventListener('click', function () {
      var box = button.closest('.cmd');
      var code = box ? box.querySelector('code') : null;
      var text = button.getAttribute('data-copy');
      var shown = text === null ? null : doc.querySelector('details.summary-text');
      if (text === null) {
        text = code ? code.textContent : '';
      }
      var fail = function () {
        if (shown) {
          // Copy summary: the same text sits in the disclosure.
          shown.open = true;
          show(button, "Couldn't copy: the text is shown below");
          return;
        }
        if (!code) {
          show(button, "Couldn't copy");
          return;
        }
        select(code);
        show(button, 'Selected: press Ctrl+C');
      };
      if (!navigator.clipboard) {
        fail();
        return;
      }
      navigator.clipboard.writeText(text).then(function () {
        show(button, 'Copied');
      }, fail);
    });
  });

  // Keyboard shortcuts and the dialog that lists them.
  var keys = doc.body.getAttribute('data-keys') === '1';
  var dlg = doc.querySelector('dialog.keys');
  var pages = { o: '/', q: '/quality', c: '/cost', b: '/bank', p: '/pipeline', s: '/system' };
  var gAt = 0; // when g was pressed
  var keep = ['days', 'since', 'until', 'tz', 'project', 'path', 'dev', 'machines']; // the window and scope every page takes
  var opener = null;

  // The query a chord carries: the window and scope only, since the other
  // keys (colour, sort, offset...) belong to the page it leaves.
  function scope() {
    var from = new URLSearchParams(location.search);
    var to = new URLSearchParams();
    keep.forEach(function (n) {
      if (from.has(n)) {
        to.set(n, from.get(n));
      }
    });
    var s = to.toString();
    return s ? '?' + s : '';
  }

  function openKeys() {
    opener = doc.activeElement;
    dlg.showModal();
    dlg.querySelector('button').focus();
  }

  if (keys && dlg) {
    dlg.querySelector('button').addEventListener('click', function () {
      dlg.close();
    });
    dlg.addEventListener('close', function () {
      if (opener) {
        opener.focus();
      }
    });
    var hint = doc.querySelector('button.keyhint-btn[hidden]');
    if (hint) {
      hint.removeAttribute('hidden');
      hint.addEventListener('click', openKeys);
    }
  }

  // The calendar: one tab stop (tabindex 0); the arrows move it. A week is a
  // column of seven days: up and down step one day, left and right one week.
  var steps = { ArrowDown: 1, ArrowUp: -1, ArrowRight: 7, ArrowLeft: -7 };

  function calendar(e, cell) {
    var days = Array.prototype.slice.call(cell.closest('.hm').querySelectorAll('a.hm-c'));
    var n = days.indexOf(cell) + (steps[e.key] || 0);
    if (e.key === 'Home') {
      n = 0;
    } else if (e.key === 'End') {
      n = days.length - 1;
    } else if (!steps[e.key]) {
      return false;
    }
    e.preventDefault();
    if (n >= 0 && n < days.length) {
      days.forEach(function (d) {
        d.tabIndex = -1;
      });
      days[n].tabIndex = 0;
      days[n].focus();
    }
    return true;
  }

  doc.addEventListener('focusin', function (e) {
    var tip = doc.querySelector('p.hm-tip');
    if (tip && e.target.matches('a.hm-c')) {
      tip.textContent = e.target.getAttribute('aria-label');
    }
  });

  doc.addEventListener('keydown', function (e) {
    var t = e.target;
    var k = e.key;
    if (e.ctrlKey || e.altKey || e.metaKey) {
      return;
    }
    if (t.matches && t.matches('a.hm-c') && calendar(e, t)) {
      return;
    }
    if (!keys) {
      return;
    }
    if (k === 'Escape') {
      if (dlg && dlg.open) {
        e.preventDefault();
        dlg.close();
      } else {
        doc.querySelectorAll('details.menu[open]').forEach(function (d) {
          d.open = false;
        });
      }
      return;
    }
    if (dlg && dlg.open) {
      return;
    }
    if (/^(INPUT|SELECT|TEXTAREA)$/.test(t.tagName) || t.isContentEditable) {
      return;
    }
    var after = gAt && Date.now() - gAt <= 1000;
    gAt = 0;
    if (k === 'g') {
      gAt = Date.now();
    } else if (after && pages[k]) {
      location.assign(pages[k] + scope());
    } else if (k === '/') {
      var proj = doc.querySelector('details.proj');
      var input = proj && proj.querySelector('input[name=project]');
      if (input) {
        e.preventDefault();
        proj.open = true;
        input.focus();
      }
    } else if (k === 'r') {
      var again = doc.querySelector('a.refresh-t');
      if (again) {
        location.assign(again.href);
      }
    } else if (k === '?' && dlg) {
      e.preventDefault();
      openKeys();
    }
  });
});
