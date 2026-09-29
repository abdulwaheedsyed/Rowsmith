// Applies the saved theme before first paint (external file: the CSP forbids inline scripts).
(function () {
  try {
    var t = localStorage.getItem("rowsmith.theme");
    if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
  } catch (e) {}
})();
