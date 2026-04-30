// Skydive Memory — theme toggle. Adds / removes `sm is-light sm-sky`
// classes on <body> based on localStorage preference. Mirror this file in
// web/server/templates/static/ — both apps load it from /static/.
//
// Usage (any page that links /static/skydive-memory.css):
//   <script src="/static/theme-toggle.js" defer></script>
//   <button data-theme-toggle aria-label="Toggle theme">
//     <span data-theme-icon></span>
//   </button>
//
// Persistence: localStorage["sm-theme"] = "light" | "dark". Default = dark
// (no class on body). The first matching button anywhere on the page is
// enough to flip every other element via the CSS cascade.

(function () {
  var KEY = 'sm-theme';
  var SUN = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="4"/><line x1="12" y1="2" x2="12" y2="5"/><line x1="12" y1="19" x2="12" y2="22"/><line x1="4.93" y1="4.93" x2="6.34" y2="6.34"/><line x1="17.66" y1="17.66" x2="19.07" y2="19.07"/><line x1="2" y1="12" x2="5" y2="12"/><line x1="19" y1="12" x2="22" y2="12"/><line x1="4.93" y1="19.07" x2="6.34" y2="17.66"/><line x1="17.66" y1="6.34" x2="19.07" y2="4.93"/></svg>';
  var MOON = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>';

  function isLight() {
    return document.body && document.body.classList.contains('is-light');
  }

  function applyLight(on) {
    var body = document.body;
    if (!body) return;
    if (on) {
      body.classList.add('sm', 'is-light', 'sm-sky');
    } else {
      body.classList.remove('sm', 'is-light', 'sm-sky');
    }
    refreshButtons();
  }

  function refreshButtons() {
    var light = isLight();
    var btns = document.querySelectorAll('[data-theme-toggle]');
    for (var i = 0; i < btns.length; i++) {
      var b = btns[i];
      b.setAttribute('aria-pressed', light ? 'true' : 'false');
      b.title = light ? 'Switch to dark' : 'Switch to light';
      var icoEl = b.querySelector('[data-theme-icon]');
      if (icoEl) {
        // Show SUN while light is active (click to go dark), MOON when dark.
        icoEl.innerHTML = light ? SUN : MOON;
      }
    }
  }

  function init() {
    var stored = null;
    try { stored = localStorage.getItem(KEY); } catch (_) { /* private mode */ }
    if (stored === 'light') applyLight(true);
    else applyLight(false);

    var btns = document.querySelectorAll('[data-theme-toggle]');
    for (var i = 0; i < btns.length; i++) {
      btns[i].addEventListener('click', function () {
        var next = !isLight();
        applyLight(next);
        try { localStorage.setItem(KEY, next ? 'light' : 'dark'); } catch (_) {}
      });
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
