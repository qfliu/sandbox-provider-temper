// Small progressive enhancements for the dashboard. Loaded as a file (the
// CSP forbids inline scripts).
document.addEventListener("click", (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const text = document.getElementById(btn.dataset.copy).textContent;
  navigator.clipboard.writeText(text).then(() => {
    const label = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(() => { btn.textContent = label; }, 1500);
  });
});

document.addEventListener("submit", (e) => {
  const btn = e.submitter;
  if (btn && btn.dataset.confirm && !window.confirm(btn.dataset.confirm)) {
    e.preventDefault();
  }
});
