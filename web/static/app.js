// BrewDat Exfil Governance: theme, drag and drop upload, ring animation, toasts.
(function () {
  const root = document.documentElement;

  // Theme toggle (initial theme is set inline in <head> to avoid a flash).
  document.querySelectorAll("[data-theme-toggle]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const next = root.dataset.theme === "dark" ? "light" : "dark";
      root.dataset.theme = next;
      localStorage.setItem("theme", next);
      btn.querySelector("[data-theme-label]").textContent = next === "dark" ? "Dark" : "Light";
    });
    btn.querySelector("[data-theme-label]").textContent = root.dataset.theme === "dark" ? "Dark" : "Light";
  });

  // Mobile menu
  const side = document.querySelector(".side");
  document.querySelectorAll("[data-menu]").forEach((b) =>
    b.addEventListener("click", () => side && side.classList.toggle("open"))
  );

  // Progress ring: animate from empty to value.
  document.querySelectorAll(".ring").forEach((ring) => {
    const fill = ring.querySelector(".fill");
    const p = Math.max(0, Math.min(1, parseFloat(ring.dataset.value || "0")));
    requestAnimationFrame(() => requestAnimationFrame(() => {
      fill.style.strokeDashoffset = String(389.56 * (1 - p));
    }));
  });

  // Meters and the gauge animate from empty to their value.
  const meters = document.querySelectorAll(".meter_fill[data-value]");
  const gauge = document.querySelector(".gauge[data-value]");
  requestAnimationFrame(() => requestAnimationFrame(() => {
    meters.forEach((m) => {
      const v = Math.max(0, Math.min(1, parseFloat(m.dataset.value) || 0));
      m.style.width = (v * 100).toFixed(1) + "%";
    });
    if (gauge) {
      const v = Math.max(0, Math.min(1, parseFloat(gauge.dataset.value) || 0));
      gauge.querySelector(".fill").style.strokeDashoffset = String(100 - v * 100);
      gauge.querySelector(".needle").style.transform = "rotate(" + (v * 180 - 90).toFixed(1) + "deg)";
    }
  }));

  // Drag and drop upload zone
  document.querySelectorAll(".drop").forEach((zone) => {
    const input = zone.querySelector("input[type=file]");
    const list = zone.parentElement.querySelector(".files");
    const show = () => {
      if (!list) return;
      list.innerHTML = "";
      Array.from(input.files).forEach((f) => {
        const li = document.createElement("li");
        li.textContent = f.name + " (" + Math.max(1, Math.round(f.size / 1024)) + " KB)";
        list.appendChild(li);
      });
    };
    ["dragenter", "dragover"].forEach((e) =>
      zone.addEventListener(e, (ev) => { ev.preventDefault(); zone.classList.add("over"); })
    );
    ["dragleave", "drop"].forEach((e) =>
      zone.addEventListener(e, (ev) => { ev.preventDefault(); zone.classList.remove("over"); })
    );
    zone.addEventListener("drop", (ev) => { input.files = ev.dataTransfer.files; show(); });
    input.addEventListener("change", show);
    zone.addEventListener("keydown", (ev) => {
      if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); input.click(); }
    });
  });

  // Confirm destructive actions without browser dialogs: first click arms, second click submits.
  document.querySelectorAll("[data-confirm]").forEach((btn) => {
    btn.addEventListener("click", (ev) => {
      if (btn.dataset.armed) return;
      ev.preventDefault();
      const label = btn.textContent;
      btn.dataset.armed = "1";
      btn.textContent = btn.dataset.confirm;
      setTimeout(() => { delete btn.dataset.armed; btn.textContent = label; }, 3500);
    });
  });

  // Flash messages become toasts.
  document.querySelectorAll("[data-toast]").forEach((el) => {
    const t = document.createElement("div");
    t.className = "toast";
    t.setAttribute("role", "status");
    t.textContent = el.dataset.toast;
    document.body.appendChild(t);
    setTimeout(() => t.remove(), 4600);
  });

  // Live filter: <input data-filter="#list"> hides children whose data-text does not match.
  document.querySelectorAll("[data-filter]").forEach((input) => {
    const list = document.querySelector(input.dataset.filter);
    if (!list) return;
    const empty = list.closest(".panel, fieldset")?.querySelector("[data-empty]");
    input.addEventListener("input", () => {
      const q = input.value.trim().toLowerCase();
      let shown = 0;
      list.querySelectorAll("[data-text]").forEach((el) => {
        const hit = !q || el.dataset.text.toLowerCase().includes(q);
        el.hidden = !hit;
        if (hit) shown++;
      });
      if (empty) empty.hidden = shown > 0;
    });
  });

  // Status chips: show all rows or only one decision, combined with the text filter.
  document.querySelectorAll("[data-chip]").forEach((chip) => {
    chip.addEventListener("click", () => {
      const panel = chip.closest(".panel");
      panel.querySelectorAll("[data-chip]").forEach((c) => c.setAttribute("aria-pressed", String(c === chip)));
      const want = chip.dataset.chip;
      let shown = 0;
      panel.querySelectorAll("tr[data-status]").forEach((row) => {
        const hit = !want || row.dataset.status === want;
        row.classList.toggle("off", !hit);
        if (hit && !row.hidden) shown++;
      });
      const empty = panel.querySelector("[data-empty]");
      if (empty) empty.hidden = shown > 0;
    });
  });

  // Selects that navigate on change.
  document.querySelectorAll("[data-autosubmit]").forEach((sel) =>
    sel.addEventListener("change", () => sel.form.submit())
  );

  // Busy state for slow submits (uploads, test email).
  document.querySelectorAll("[data-busy]").forEach((btn) => {
    btn.form.addEventListener("submit", () => {
      btn.disabled = true;
      btn.textContent = btn.dataset.busy + "…";
    });
  });

  // File preview in a modal: PDFs and text in a frame, images as an image. Esc or the backdrop closes it.
  const viewer = document.querySelector("dialog.viewer");
  if (viewer) {
    const body = viewer.querySelector("[data-viewer-body]");
    document.querySelectorAll("[data-preview]").forEach((link) => {
      link.addEventListener("click", (ev) => {
        ev.preventDefault();
        const name = link.textContent.trim();
        viewer.querySelector("[data-viewer-name]").textContent = name;
        viewer.querySelector("[data-viewer-download]").href = link.dataset.download;
        let el;
        if (link.dataset.preview === "image") {
          el = document.createElement("img");
          el.alt = name;
        } else {
          el = document.createElement("iframe");
          el.title = name;
          el.className = link.dataset.preview;
        }
        el.src = link.href;
        body.replaceChildren(el);
        viewer.showModal();
      });
    });
    viewer.querySelector("[data-viewer-close]").addEventListener("click", () => viewer.close());
    viewer.addEventListener("click", (ev) => { if (ev.target === viewer) viewer.close(); });
    viewer.addEventListener("close", () => body.replaceChildren());
  }

  // Live pending figure on the upload form: identified minus remediated minus accepted.
  const pending = document.querySelector("[data-pending]");
  if (pending) {
    const ident = document.querySelector("[data-identified]");
    const rem = document.querySelector("[data-remediated]");
    const update = () => {
      const i = ident ? parseInt(ident.value, 10) : parseInt(pending.dataset.pending, 10);
      const r = parseInt(rem.value, 10);
      const a = parseInt(pending.dataset.accepted, 10) || 0;
      if (isNaN(i) || isNaN(r)) { pending.textContent = "None yet"; return; }
      const p = i - r - a;
      pending.textContent = p < 0 ? "Too high" : String(p);
    };
    [ident, rem].forEach((el) => el && el.addEventListener("input", update));
  }

  // Login page bubbles
  const bubbles = document.querySelector(".bubbles");
  if (bubbles && !matchMedia("(prefers-reduced-motion: reduce)").matches) {
    for (let i = 0; i < 18; i++) {
      const b = document.createElement("i");
      const s = 6 + Math.random() * 22;
      b.style.cssText = `left:${Math.random() * 100}%;width:${s}px;height:${s}px;animation-duration:${8 + Math.random() * 10}s;animation-delay:${-Math.random() * 12}s`;
      bubbles.appendChild(b);
    }
  }
})();
