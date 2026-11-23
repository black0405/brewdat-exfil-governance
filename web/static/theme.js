// Runs before first paint so the page never flashes the wrong theme.
document.documentElement.dataset.theme =
  localStorage.getItem("theme") || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
