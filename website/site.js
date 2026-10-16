// Theme toggle
(function () {
  var btn = document.getElementById('theme');
  var sun = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg>';
  var moon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/></svg>';
  function paint() { btn.innerHTML = document.documentElement.dataset.theme === 'dark' ? sun : moon; }
  btn.addEventListener('click', function () {
    var t = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = t;
    try { localStorage.setItem('theme', t); } catch (e) {}
    paint();
  });
  paint();
})();

// Copy buttons on code blocks
document.querySelectorAll('.codeblock').forEach(function (block) {
  var b = document.createElement('button');
  b.className = 'copy'; b.textContent = 'Copy'; b.type = 'button';
  b.addEventListener('click', function () {
    var text = block.querySelector('pre').innerText;
    navigator.clipboard.writeText(text).then(function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy'; }, 1500); });
  });
  block.appendChild(b);
});

// Scroll spy for "On this page"
(function () {
  var links = Array.prototype.slice.call(document.querySelectorAll('.toc a'));
  if (!links.length) return;
  var map = {};
  links.forEach(function (a) { map[a.getAttribute('href').slice(1)] = a; });
  var heads = Object.keys(map).map(function (id) { return document.getElementById(id); }).filter(Boolean);
  var current = null;
  function update() {
    var y = window.scrollY + 90, best = heads[0];
    heads.forEach(function (h) { if (h.offsetTop <= y) best = h; });
    if (best && best !== current) {
      links.forEach(function (a) { a.classList.remove('active'); });
      map[best.id].classList.add('active');
      current = best;
    }
  }
  window.addEventListener('scroll', update, { passive: true });
  update();
})();

// Client-side search over the embedded index
(function () {
  var input = document.getElementById('q'), box = document.getElementById('results');
  var index = JSON.parse(document.getElementById('search-index').textContent);
  function esc(s) { return s.replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  function snippet(text, q) {
    var i = text.toLowerCase().indexOf(q); if (i < 0) return esc(text.slice(0, 120));
    var s = Math.max(0, i - 50), e = Math.min(text.length, i + q.length + 70);
    var part = esc(text.slice(s, e));
    return (s ? '…' : '') + part.replace(new RegExp('(' + q.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + ')', 'ig'), '<mark>$1</mark>') + (e < text.length ? '…' : '');
  }
  function run() {
    var q = input.value.trim().toLowerCase();
    if (q.length < 2) { box.hidden = true; box.innerHTML = ''; return; }
    var hits = [];
    index.forEach(function (e) {
      var hay = (e.section + ' ' + e.text).toLowerCase();
      var score = e.section.toLowerCase().indexOf(q) >= 0 ? 2 : (hay.indexOf(q) >= 0 ? 1 : 0);
      if (score) hits.push([score, e]);
    });
    hits.sort(function (a, b) { return b[0] - a[0]; });
    hits = hits.slice(0, 8);
    if (!hits.length) { box.innerHTML = '<div class="hit none">No results</div>'; box.hidden = false; return; }
    box.innerHTML = hits.map(function (h) {
      var e = h[1];
      return '<a class="hit" href="' + e.slug + '.html' + (e.anchor ? '#' + e.anchor : '') + '"><div class="hit-title">' + esc(e.page) + ' › ' + esc(e.section) + '</div><div class="hit-text">' + snippet(e.text, q) + '</div></a>';
    }).join('');
    box.hidden = false;
  }
  input.addEventListener('input', run);
  input.addEventListener('focus', run);
  document.addEventListener('click', function (ev) { if (!ev.target.closest('.search')) box.hidden = true; });
  document.addEventListener('keydown', function (ev) {
    if (ev.key === '/' && document.activeElement !== input) { ev.preventDefault(); input.focus(); }
    if (ev.key === 'Escape') { box.hidden = true; input.blur(); }
  });
})();
