'use strict';
// Copy buttons: the page renders them hidden; this script shows them and
// makes them copy the command next to them.
document.addEventListener('DOMContentLoaded', function () {
  var timer = 0;
  var active = null; // {button, kids}: the button showing a result
  var status = document.getElementById('copy-status');

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
    button.replaceChildren(document.createTextNode(label));
    if (status) {
      status.textContent = label;
    }
    timer = setTimeout(restore, 2000);
  }

  function select(code) {
    var sel = window.getSelection();
    var range = document.createRange();
    range.selectNodeContents(code);
    sel.removeAllRanges();
    sel.addRange(range);
  }

  document.querySelectorAll('button.copy[hidden]').forEach(function (button) {
    button.removeAttribute('hidden');
    button.addEventListener('click', function () {
      var box = button.closest('.cmd');
      var code = box ? box.querySelector('code') : null;
      var text = button.getAttribute('data-copy');
      if (text === null) {
        text = code ? code.textContent : '';
      }
      var fail = function () {
        if (code) {
          select(code);
        }
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
});
