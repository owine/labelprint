// Runs before the stylesheets: picks the stored theme, else the system one.
(function () {
  var t = null;
  try { t = localStorage.getItem('owine-theme'); } catch (e) { /* storage blocked */ }
  if (t !== 'light' && t !== 'dark') {
    t = window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  document.documentElement.setAttribute('data-theme', t);
})();
